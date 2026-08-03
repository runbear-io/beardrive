package webapp

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// The registries re-read their whole store on every authorization read — a
// correctness floor (rounds 12-14), not a cache. Versioned makes that read
// cheap without making it optional. These two tests are the pair that has to
// hold together: the gate must never hide a change (dbverExternalWriteIsSeen)
// and it must actually skip the work when there is none (BenchmarkRegistryRead
// notices if it stops).

// dbverStores builds one metadata store per machine-local backend, keyed by
// name. Postgres is added separately (dbverPostgres) — it is only reachable
// when a DSN is configured, and seeding thousands of rows over it does not
// belong in a benchmark.
func dbverStores(t testing.TB) map[string]MetaStore {
	t.Helper()
	out := map[string]MetaStore{}
	for _, b := range []struct{ name, driver string }{{"file", ""}, {"sqlite", "sqlite"}} {
		dir := t.TempDir()
		var (
			st  MetaStore
			err error
		)
		if b.driver == "" {
			st, err = OpenFileStore(dir)
		} else {
			st, err = OpenSQLStore(b.driver, filepath.Join(dir, "meta.db"))
		}
		if err != nil {
			t.Fatalf("open %s store: %v", b.name, err)
		}
		t.Cleanup(func() { st.Close() })
		out[b.name] = st
	}
	return out
}

// dbverPostgres adds the Postgres arm when BDRIVE_TEST_POSTGRES is set. This is
// the backend the change token matters most on — a stale token there means nine
// unfiltered SELECTs skipped that should not have been — so the correctness
// test runs against it whenever it can.
func dbverPostgres(t *testing.T, into map[string]MetaStore) {
	t.Helper()
	dsn := os.Getenv("BDRIVE_TEST_POSTGRES")
	if dsn == "" {
		t.Log("BDRIVE_TEST_POSTGRES not set — the change token is UNTESTED against Postgres in this run")
		return
	}
	// Deliberately no DROP. Every backend here shares ONE database with the
	// conformance harness, whose own reset drops a SUBSET of the tables — so a
	// more thorough drop from this test leaves a residue that reset does not
	// expect (schema_meta recording version 1 over a projects table rebuilt
	// without default_level is exactly the half-applied migration the startup
	// guard refuses). This test does not need a clean database: it asserts by
	// project id, and GetOrCreate is create-or-join, so pre-existing rows change
	// nothing.
	st, err := OpenSQLStore("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	into["postgres"] = st
}

// A second hub process writing the same store must still be seen. This is the
// whole risk in gating refresh() on a change token: a token that fails to move
// when the store did would resurrect exactly the revoked grants rounds 12-14
// spent four fixes killing. Two registries over ONE store stand in for two
// processes — the same shape those rounds' tests use.
func TestVersionGateSeesAnotherProcessWrite(t *testing.T) {
	stores := dbverStores(t)
	dbverPostgres(t, stores)
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			// Two independent registries over the same repo: separate maps,
			// separate gates, one store — two hub processes.
			a, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			b, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			// b answers first, so its gate records a token.
			if _, ok := b.Get("p-one"); ok {
				t.Fatal("fixture: project exists before it was created")
			}

			p, _, err := a.GetOrCreate("wiki", "org1")
			if err != nil {
				t.Fatal(err)
			}
			if got, ok := b.Get(p.ID); !ok {
				t.Fatal("the other process's new project is invisible: the change token did not move")
			} else if got.Name != "wiki" {
				t.Fatalf("Get returned %q, want wiki", got.Name)
			}

			// And a MUTATION of an existing row, which is the authorization
			// direction: a grant revoked by one process must not keep serving
			// on the other.
			if err := a.SetPerm(p.ID, "bob@x.io", PermNone); err != nil {
				t.Fatal(err)
			}
			got, ok := b.Get(p.ID)
			if !ok {
				t.Fatal("project vanished")
			}
			if got.Perms["bob@x.io"] != PermNone {
				t.Fatalf("the other process's grant change is invisible: perms=%v", got.Perms)
			}
		})
	}
}

// dbverSeed fills a project registry with n projects in one org.
func dbverSeed(t testing.TB, st MetaStore, n int) *ProjectDB {
	t.Helper()
	db, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, _, err := db.GetOrCreate(fmt.Sprintf("proj-%05d", i), "org1"); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// BenchmarkAuthorizedRequest is the whole per-request authorization cost: the
// project registry, the org registry and the account store, exactly as the
// proj() choke point walks them. Two things move it — the version gate (each
// registry re-reads only when it moved) and resolving the project ONCE per
// request instead of twice.
func BenchmarkAuthorizedRequest(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		for name, st := range dbverStores(b) {
			db := dbverSeed(b, st, n)
			orgs, err := NewOrgDB(st.Orgs())
			if err != nil {
				b.Fatal(err)
			}
			auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
			if err != nil {
				b.Fatal(err)
			}
			if _, err := orgs.Create("acme", "alice@x.io"); err != nil {
				b.Fatal(err)
			}
			srv := &Server{Projects: db, Dir: LocalDirectory{OrgDB: orgs}, Auth: auth}
			id := db.List()[0].ID
			req := httptest.NewRequest("GET", "/api/p/"+id+"/tree", nil)
			b.Run(fmt.Sprintf("%s/%d", name, n), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					p, ok := srv.Projects.Get(id)
					if !ok {
						b.Fatal("missing project")
					}
					srv.projectPermOf(req, p)
				}
			})
		}
	}
}

// BenchmarkRegistryRead is the number the version gate exists for: one
// authorization read against a registry that has not changed. Unconditionally
// re-reading the store here was 24 ms at 5k projects on the file backend and
// nine unfiltered SELECTs on SQL — held under a hub-wide mutex, so it
// serialized the whole hub. Run it with -benchtime=200x; a regression shows up
// as the numbers scaling with project count again.
func BenchmarkRegistryRead(b *testing.B) {
	for _, n := range []int{100, 1000, 5000} {
		for name, st := range dbverStores(b) {
			db := dbverSeed(b, st, n)
			id := ""
			for _, p := range db.List() {
				id = p.ID
				break
			}
			b.Run(fmt.Sprintf("%s/%d", name, n), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					if _, ok := db.Get(id); !ok {
						b.Fatal("missing project")
					}
				}
			})
		}
	}
}
