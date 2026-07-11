package webapp

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A metaBackend is one MetaStore implementation under test. reset clears any
// shared durable state before the run; open returns a fresh store over the
// SAME underlying storage, so the suite can write, reopen, and prove the data
// persisted.
type metaBackend struct {
	name  string
	reset func(t *testing.T)
	open  func(t *testing.T) MetaStore
}

func metaBackends(t *testing.T) []metaBackend {
	dir := t.TempDir()
	sqlitePath := filepath.Join(t.TempDir(), "meta.db")
	backends := []metaBackend{
		{
			name:  "file",
			reset: func(t *testing.T) {},
			open: func(t *testing.T) MetaStore {
				s, err := OpenFileStore(dir)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		},
		{
			name:  "sqlite",
			reset: func(t *testing.T) {},
			open: func(t *testing.T) MetaStore {
				s, err := OpenSQLStore("sqlite", sqlitePath)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		},
	}
	// Postgres/Supabase is exercised only when a DSN is reachable.
	if dsn := os.Getenv("BDRIVE_TEST_POSTGRES"); dsn != "" {
		backends = append(backends, metaBackend{
			name: "postgres",
			reset: func(t *testing.T) {
				db, err := sql.Open("pgx", dsn)
				if err != nil {
					t.Fatalf("postgres reset: %v", err)
				}
				defer db.Close()
				db.Exec(`DROP TABLE IF EXISTS accounts, tokens, auth_policy, projects, orgs, org_members, invites, shares, devices, read_stats`)
			},
			open: func(t *testing.T) MetaStore {
				s, err := OpenSQLStore("pgx", dsn)
				if err != nil {
					t.Fatal(err)
				}
				return s
			},
		})
	} else {
		t.Log("BDRIVE_TEST_POSTGRES not set — postgres backend UNTESTED in this run")
	}
	return backends
}

// TestMetaStoreConformance runs the same service-level operations against every
// backend, then reopens the store and asserts the data survived — covering
// accounts+tokens, the signup policy, pending/approve, projects (create-or-join,
// rename, delete), orgs (roles), invites (create/redeem/uses/validity), shares
// (create/revoke/expiry), and devices.
func TestMetaStoreConformance(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)

			// ---- write everything through the services ----
			st := be.open(t)

			auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
			if err != nil {
				t.Fatal(err)
			}
			// account + token
			u, err := auth.signup("dev@x.io", "Dev", "password1")
			if err != nil {
				t.Fatal(err)
			}
			tok, err := auth.issueToken(u.ID, "cli")
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := auth.userForToken(tok); !ok {
				t.Fatal("token should authenticate its account")
			}
			// pending + policy
			if err := auth.SetPolicy(false, true); err != nil { // require approval
				t.Fatal(err)
			}
			pend, err := auth.signup("pending@x.io", "Pend", "password1")
			if err != nil {
				t.Fatal(err)
			}
			if pend.Status != statusPending {
				t.Fatalf("new account status = %q, want pending", pend.Status)
			}

			projects, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			p1, created, err := projects.GetOrCreate("wiki", "o-1")
			if err != nil || !created {
				t.Fatalf("create wiki: created=%v err=%v", created, err)
			}
			if _, again, _ := projects.GetOrCreate("wiki", "o-1"); again {
				t.Fatal("same name+org must join, not create")
			}
			if _, other, _ := projects.GetOrCreate("wiki", "o-2"); !other {
				t.Fatal("same name in a different org must create")
			}
			if err := projects.Rename(p1.ID, "handbook"); err != nil {
				t.Fatal(err)
			}
			p2, _, _ := projects.GetOrCreate("scratch", "o-1")
			if err := projects.Delete(p2.ID); err != nil {
				t.Fatal(err)
			}

			orgs, err := NewOrgDB(st.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			org, err := orgs.Create("Acme", "boss@x.io")
			if err != nil {
				t.Fatal(err)
			}
			if err := orgs.AddMember(org.ID, "worker@x.io", RoleMember); err != nil {
				t.Fatal(err)
			}
			if err := orgs.SetRole(org.ID, "worker@x.io", RoleOwner); err != nil {
				t.Fatal(err)
			}
			inv, err := orgs.CreateInvite(org.ID, "boss@x.io", time.Hour)
			if err != nil {
				t.Fatal(err)
			}
			orgs.RecordInviteUse(inv.Token)
			if !orgs.ValidInvite(inv.Token) {
				t.Fatal("fresh invite should be valid")
			}

			shares, err := NewShareDB(st.Shares())
			if err != nil {
				t.Fatal(err)
			}
			live, err := shares.Create(p1.ID, "handbook.md", "boss@x.io", 0)
			if err != nil {
				t.Fatal(err)
			}
			gone, _ := shares.Create(p1.ID, "temp.md", "boss@x.io", time.Hour)
			if !shares.Revoke(gone.Token) {
				t.Fatal("revoke should succeed")
			}

			devices, err := NewDeviceRegistry(st.Devices())
			if err != nil {
				t.Fatal(err)
			}
			devices.Observe(DeviceInfo{ID: "d1", Name: "laptop", OS: "mac", User: "dev@x.io", IP: "1.2.3.4"})

			reads, err := NewReadLedger(st.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			reads.Record(p1.ID, "handbook.md", ReadKindHuman, "dev@x.io")
			reads.Record(p1.ID, "handbook.md", ReadKindHuman, "boss@x.io")
			reads.Record(p1.ID, "wiki/deep.md", ReadKindAgent, "d1")
			if err := reads.Close(); err != nil {
				t.Fatal(err)
			}

			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			// ---- reopen and verify everything persisted ----
			st2 := be.open(t)
			defer st2.Close()

			auth2, _ := NewBuiltinAuth(st2.Accounts(), true, nil)
			if _, ok := auth2.userForToken(tok); !ok {
				t.Fatal("token lost across reload")
			}
			if !auth2.RequireApproval || auth2.RequireVerification {
				t.Fatal("policy lost across reload")
			}
			if got := auth2.PendingUsers(); len(got) != 1 || got[0].Email != "pending@x.io" {
				t.Fatalf("pending users after reload = %+v", got)
			}
			if err := auth2.Approve(pend.ID); err != nil {
				t.Fatal(err)
			}
			if len(auth2.PendingUsers()) != 0 {
				t.Fatal("approve did not clear pending")
			}

			projects2, _ := NewProjectDB(st2.Projects())
			list := projects2.List()
			if len(list) != 2 { // handbook (o-1), wiki (o-2); scratch was deleted
				t.Fatalf("projects after reload = %+v", list)
			}
			hb, ok := projects2.Get(p1.ID)
			if !ok || hb.Name != "handbook" {
				t.Fatalf("rename lost across reload: %+v", hb)
			}

			orgs2, _ := NewOrgDB(st2.Orgs())
			ro, ok := orgs2.Get(org.ID)
			if !ok || ro.Members["boss@x.io"] != RoleOwner || ro.Members["worker@x.io"] != RoleOwner {
				t.Fatalf("org roles lost across reload: %+v", ro)
			}
			if !orgs2.ValidInvite(inv.Token) {
				t.Fatal("invite lost across reload")
			}
			if got := orgs2.ListInvites(org.ID); len(got) != 1 || got[0].Uses != 1 {
				t.Fatalf("invite uses after reload = %+v", got)
			}

			shares2, _ := NewShareDB(st2.Shares())
			if _, ok := shares2.Get(live.Token); !ok {
				t.Fatal("live share lost across reload")
			}
			if _, ok := shares2.Get(gone.Token); ok {
				t.Fatal("revoked share came back after reload")
			}

			devices2, _ := NewDeviceRegistry(st2.Devices())
			d, ok := devices2.Get("d1")
			if !ok || d.Name != "laptop" || d.IP != "1.2.3.4" {
				t.Fatalf("device lost across reload: %+v", d)
			}

			reads2, err := NewReadLedger(st2.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			heat := reads2.Heat(p1.ID, "", time.Time{})
			if e := heat["handbook.md"]; e.Human != 2 || e.Readers != 2 {
				t.Fatalf("read buckets lost across reload: %+v", e)
			}
			if e := heat["wiki/deep.md"]; e.Agent != 1 || e.Readers != 0 {
				t.Fatalf("agent read bucket lost across reload: %+v", e)
			}
			if sub := reads2.Heat(p1.ID, "wiki", time.Time{}); len(sub) != 1 {
				t.Fatalf("prefix heat = %+v, want only wiki/deep.md", sub)
			}
		})
	}
}
