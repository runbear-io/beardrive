package webapp

// Round 11 — scoreboard row 14, measured against a REAL Postgres 16
// (BDRIVE_TEST_POSTGRES) and, where the defect is not Postgres-specific,
// against file and sqlite too.
//
// The round's premise: `metaBackends` silently omits the postgres arm when no
// DSN is reachable, so seven rounds scored row 14 "clean on every backend"
// without ever running the backend a managed deployment uses. These tests
// separate three things that a green suite had merged into one:
//
//   - what every backend does with text it cannot store (NUL, invalid UTF-8,
//     lone surrogates) — and whether the three AGREE
//   - whether a write the store reports as applied is the write that is on
//     disk (the file backend silently rewrites some bytes and says nothing)
//   - which decisions the hub makes off data whose ORDER is not defined by
//     anything — the org heir being the one that hands out ownership
//
// Helper prefix `secpg` per the harness rules; the backend matrix is
// `metaBackends` from db_conformance_test.go, reused rather than rebuilt.

import (
	"database/sql"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// secpgSQL opens the Postgres under test directly, for the schema surgery an
// older release's migration state requires. It skips when no DSN is set —
// which is itself the round's point: a skipped arm and a passing arm read the
// same in a green suite.
func secpgSQL(t *testing.T) (*sql.DB, string) {
	t.Helper()
	dsn := os.Getenv("BDRIVE_TEST_POSTGRES")
	if dsn == "" {
		t.Skip("BDRIVE_TEST_POSTGRES not set — this measures the SQL migration path and " +
			"cannot run on the file backend")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	if _, err := db.Exec(`DROP TABLE IF EXISTS accounts, tokens, auth_policy, projects, orgs,
		org_members, invites, shares, devices, device_rows, read_stats, project_perms`); err != nil {
		t.Fatal(err)
	}
	return db, dsn
}

// secpgUnstorable is text a text column may or may not be able to hold. Each
// entry is a byte sequence an ordinary account can put into a stored record
// through a URL path segment or a form field.
var secpgUnstorable = []struct{ name, s string }{
	{"nul", "ev\x00il"},
	{"invalid-utf8", "ev\xffil"},
	{"lone-surrogate", "ev\xed\xa0\x80il"},
}

// secpgWrite is one repo write, addressed by the value under test, plus the
// way to read that same value back out of a freshly loaded store.
type secpgWrite struct {
	name string
	put  func(st MetaStore, id, v string) error
	get  func(st MetaStore, id string) (string, bool)
}

func secpgWrites() []secpgWrite {
	return []secpgWrite{
		{
			name: "device name",
			put: func(st MetaStore, id, v string) error {
				return st.Devices().Put(DeviceInfo{User: "eve@x.io", ID: id, Name: v})
			},
			get: func(st MetaStore, id string) (string, bool) {
				rows, err := st.Devices().Load()
				if err != nil {
					return "", false
				}
				for _, d := range rows {
					if d.ID == id {
						return d.Name, true
					}
				}
				return "", false
			},
		},
		{
			name: "project grant email",
			put: func(st MetaStore, id, v string) error {
				return st.Projects().Put(Project{ID: id, Name: "wiki", Org: "o-1",
					Perms: map[string]string{v: PermAdmin}})
			},
			get: func(st MetaStore, id string) (string, bool) {
				rows, err := st.Projects().Load()
				if err != nil {
					return "", false
				}
				for _, p := range rows {
					if p.ID != id {
						continue
					}
					for e := range p.Perms {
						return e, true
					}
					return "", true
				}
				return "", false
			},
		},
		{
			name: "org member email",
			put: func(st MetaStore, id, v string) error {
				return st.Orgs().PutOrg(Org{ID: id, Name: "Acme",
					Members: map[string]string{v: RoleOwner}, Joined: map[string]time.Time{}})
			},
			get: func(st MetaStore, id string) (string, bool) {
				rows, _, err := st.Orgs().Load()
				if err != nil {
					return "", false
				}
				for _, o := range rows {
					if o.ID != id {
						continue
					}
					for e := range o.Members {
						return e, true
					}
					return "", true
				}
				return "", false
			},
		},
		{
			name: "account name",
			put: func(st MetaStore, id, v string) error {
				return st.Accounts().PutAccount(&authUser{ID: id, Email: id + "@x.io", Name: v,
					Pass: "x", Status: statusActive, Created: time.Now().UTC()})
			},
			get: func(st MetaStore, id string) (string, bool) {
				users, _, _, err := st.Accounts().Load()
				if err != nil {
					return "", false
				}
				for _, u := range users {
					if u.ID == id {
						return u.Name, true
					}
				}
				return "", false
			},
		},
		{
			name: "share path",
			put: func(st MetaStore, id, v string) error {
				return st.Shares().Put(Share{Token: id, Project: "p-1", Path: v, Creator: "eve@x.io"})
			},
			get: func(st MetaStore, id string) (string, bool) {
				rows, err := st.Shares().Load()
				if err != nil {
					return "", false
				}
				for _, s := range rows {
					if s.Token == id {
						return s.Path, true
					}
				}
				return "", false
			},
		},
		{
			name: "read bucket path",
			put: func(st MetaStore, id, v string) error {
				return st.Reads().PutBatch([]ReadStat{{Project: id, Path: v,
					Kind: ReadKindHuman, Actor: "eve@x.io", Count: 1}})
			},
			get: func(st MetaStore, id string) (string, bool) {
				rows, err := st.Reads().Load()
				if err != nil {
					return "", false
				}
				for _, s := range rows {
					if s.Project == id {
						return s.Path, true
					}
				}
				return "", false
			},
		},
	}
}

// secpgStore runs w.put on a store, closes it, reopens, and reports what came
// back — the only fidelity question that matters, since every registry is
// rebuilt from the store on the next hub start.
func secpgStore(t *testing.T, be metaBackend, w secpgWrite, id, v string) (err error, got string, present bool) {
	t.Helper()
	st := be.open(t)
	err = w.put(st, id, v)
	st.Close()
	st2 := be.open(t)
	defer st2.Close()
	got, present = w.get(st2, id)
	return
}

// A write the store ACCEPTS must be the write that is on disk.
//
// This is the rule round 5 established for NUL bytes — "refused, or stored,
// but never silently lost" — applied to the class nobody has tested: bytes
// that are not valid UTF-8. The file backend serializes through encoding/json,
// which does not refuse invalid UTF-8: it substitutes U+FFFD per bad byte and
// reports success. The row the caller was told it wrote is not the row on
// disk, nothing logs it, and two inputs that differ in memory can fold onto
// one key on disk. sqlite stores the bytes verbatim and Postgres refuses the
// statement outright, so all three do something different with one input.
//
// SCOPE, stated because it bounds the severity: the two fields that decide
// authorization — grant emails and org member emails — are pre-folded by
// normEmail (strings.ToLower runs through strings.Map, which already
// substitutes U+FFFD), so through the HTTP handlers memory and disk agree on
// those and no grant lands on an address nobody granted. See
// TestSec_Auth_TwoAccountsMustNotCollapseOntoOneEmail, which is green. What is
// NOT folded anywhere is every other stored string: device names, project and
// org names (which carry create-or-join-by-name semantics), share paths, read
// bucket paths, account display names. Those are the reachable half.
func TestSec_DB_AcceptedTextIsStoredVerbatimOnEveryBackend(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			for _, w := range secpgWrites() {
				for _, c := range secpgUnstorable {
					id := "secpg-" + strings.ReplaceAll(w.name, " ", "-") + "-" + c.name
					err, got, present := secpgStore(t, be, w, id, c.s)
					if err != nil {
						continue // refused: the honest answer, nothing to check
					}
					if !present {
						t.Errorf("%s / %s: the store accepted the write (no error) and the record "+
							"is not there after a reload — the hub told its caller the change "+
							"landed and it did not", w.name, c.name)
						continue
					}
					if got != c.s {
						t.Errorf("%s / %s: the store accepted the write (no error) but rewrote the "+
							"value before writing it: put %q, reloaded %q. The running hub and its "+
							"database hold different records, nothing is logged, and two inputs "+
							"that differ in memory fold onto one key on disk",
							w.name, c.name, c.s, got)
					}
				}
			}
		})
	}
}

// The three backends must agree on WHICH text is storable.
//
// This is the replacement for TestSec_DB_NULBytesDoNotTruncateRecords, whose
// assertion — "a NUL must round-trip verbatim" — is a decision Postgres cannot
// implement (a text column cannot hold 0x00 at all, SQLSTATE 22021) and so can
// only ever be satisfied by moving the metadata layer to bytea. The decided
// behaviour asserted here is the other one, and the one the hub already relies
// on everywhere else: unstorable text is REFUSED, identically, on every
// backend — so a hub cannot change what it accepts by changing its database,
// and the guards at the doors (printableOnly, hasControlChars, journal.SafePath)
// have one rule to enforce rather than three.
//
// The old test must be RETIRED by the ciso, not edited by the hacker; it and
// this one assert opposite decisions and cannot both be green.
func TestSec_DB_EveryBackendAgreesWhichTextIsStorable(t *testing.T) {
	backends := metaBackends(t)
	if len(backends) < 3 {
		// SKIP, not FAIL. The hacker wrote this as a t.Fatal so the gap could
		// never be green-because-skipped, which is exactly how row 14 stayed
		// clean for seven rounds — and that intent is right. But a default
		// suite that is permanently red for anyone without Docker trains
		// everyone to ignore a red, and then a real regression hides in the
		// noise: the same failure mode as an unread log. The property to
		// preserve is THE GAP IS NEVER SILENT, not THE SUITE IS ALWAYS RED.
		//
		// So the gap is reported loudly, on stderr, on every run, from one
		// place: TestSec_Suite_RunModeIsVisible. That test also asserts this
		// skip exists, so deleting the skip — or this test — is itself caught.
		t.Skipf("SKIPPED, NOT PASSED: cross-backend text agreement was NOT measured in this run. "+
			"It needs all three arms and only %d are configured; set BDRIVE_TEST_POSTGRES. "+
			"Unmeasured: 18 rows (6 write surfaces x {nul, invalid-utf8, lone-surrogate}) — "+
			"whether the same request from the same account succeeds or fails depending only "+
			"on which database the operator chose", len(backends))
	}
	for _, w := range secpgWrites() {
		for _, c := range secpgUnstorable {
			accepted := map[string]bool{}
			for _, be := range backends {
				id := "secpgagree-" + strings.ReplaceAll(w.name, " ", "-") + "-" + c.name
				err, _, _ := secpgStore(t, be, w, id, c.s)
				accepted[be.name] = err == nil
			}
			var yes, no []string
			for name, ok := range accepted {
				if ok {
					yes = append(yes, name)
				} else {
					no = append(no, name)
				}
			}
			if len(yes) > 0 && len(no) > 0 {
				t.Errorf("%s / %s: accepted by %v, refused by %v — the same request from the same "+
					"account succeeds or fails depending only on which database the operator chose",
					w.name, c.name, yes, no)
			}
		}
	}
}

// Two accounts must never end up sharing one email address.
//
// Every authorization decision on the hub keys on the email (OrgDB.Role,
// Project.Perms, share liveness), so two rows carrying one address is two
// people holding one set of grants — and the hub resolves it by whichever row
// findByEmail reaches first, which is a map.
//
// CLEAN, and this test is the proof, not a claim. The attack: the file backend
// (the default) folds invalid UTF-8 to U+FFFD on the way to auth.json, so
// signing up `ev\xffil@x.io` and then `ev<U+FFFD>il@x.io` would be two rows
// carrying one address after a restart — one person able to sign in with their
// own password and receive the other's grants. It is refused, and by the door
// rather than by the store: createAccount lowercases with strings.ToLower,
// which runs through strings.Map and has already substituted U+FFFD before the
// duplicate check runs, so the second signup is a duplicate and is rejected.
// That is load-bearing and undocumented — a refactor to a byte-wise
// lowercaser, or to any normalizer that preserves invalid bytes, reopens it.
// This test is the regression guard on that.
func TestSec_Auth_TwoAccountsMustNotCollapseOntoOneEmail(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := secdefStoreHub(t, st, t.TempDir())

	raw := "ev\xffil@x.io" // what the attacker posts
	folded := "ev�il@x.io" // what encoding/json writes for it

	signup := func(email, name string) int {
		form := url.Values{"email": {email}, "name": {name}, "password": {"password1"}}
		req := httptest.NewRequest("POST", "/auth/signup", strings.NewReader(form.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	// The control: the same address twice is refused, so the duplicate check
	// is on and working. This is the "authorized user does the same thing"
	// half — the delta below is the finding.
	if code := signup("mallory@x.io", "Mallory"); code != http.StatusSeeOther {
		t.Fatalf("harness: plain signup should succeed, got %d", code)
	}
	if code := signup("mallory@x.io", "Mallory Again"); code == http.StatusSeeOther {
		t.Fatal("harness: a duplicate address should be refused")
	}

	c1 := signup(raw, "Eve One")
	c2 := signup(folded, "Eve Two")
	st.Close()

	st2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	auth2, err := NewBuiltinAuth(st2.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	byEmail := map[string][]string{}
	for _, u := range auth2.Accounts() {
		byEmail[u.Email] = append(byEmail[u.Email], u.Name)
	}
	if c1 != http.StatusSeeOther {
		t.Fatalf("harness: the raw-byte signup was refused outright (%d); this test only "+
			"means something if it is accepted", c1)
	}
	for email, names := range byEmail {
		if len(names) > 1 {
			t.Errorf("after a restart, %d accounts share the address %q: %v "+
				"(signup of %q returned %d, signup of %q returned %d) — the hub accepted two "+
				"signups it saw as different and stored them as one address, so every grant, "+
				"org role and share bound to it resolves to whichever row the map hands over first",
				len(names), email, names, raw, c1, folded, c2)
		}
	}
}

// ---- the seniority data the org heir depends on --------------------------

// secpgLegacyOrg seeds a store the way an UPGRADED hub looks: account rows with
// no `created` stamp (the column arrived later) and org member rows with no
// `joined` time (round 9's column arrived later still). Both are the state of
// the hubs that have been running longest — the ones with the most to lose.
func secpgLegacyOrg(t *testing.T, be metaBackend) {
	t.Helper()
	be.reset(t)
	st := be.open(t)
	defer st.Close()
	for _, e := range []string{"zoe@x.io", "adam@x.io", "mia@x.io", "kim@x.io", "bo@x.io", "boss@x.io"} {
		if err := st.Accounts().PutAccount(&authUser{
			ID: "u-" + e, Email: e, Name: e, Pass: "x", Status: statusActive,
		}); err != nil {
			t.Fatal(err)
		}
	}
	members := map[string]string{
		"boss@x.io": RoleOwner, "zoe@x.io": RoleMember, "adam@x.io": RoleMember,
		"mia@x.io": RoleMember, "kim@x.io": RoleMember, "bo@x.io": RoleMember,
	}
	if err := st.Orgs().PutOrg(Org{ID: "o-legacy", Name: "Acme", Members: members,
		Joined: map[string]time.Time{}}); err != nil {
		t.Fatal(err)
	}
}

// secpgHeirAfterOffboard starts a hub over be, offboards the sole owner
// through the server's own choke point (Server.offboard, the path account
// removal runs), and returns whoever inherited the org.
func secpgHeirAfterOffboard(t *testing.T, be metaBackend) string {
	t.Helper()
	st := be.open(t)
	defer st.Close()
	_, srv := secdefStoreHub(t, st, t.TempDir())
	srv.offboard("boss@x.io")
	o, ok := srv.Dir.(LocalDirectory).OrgDB.Get("o-legacy")
	if !ok {
		t.Fatal("org vanished")
	}
	for e, role := range o.Members {
		if role == RoleOwner {
			return e
		}
	}
	return ""
}

// Round 10 made the org heir fall back to account seniority — AuthProvider
// .Accounts(), documented "oldest first" — because every member of an upgraded
// hub ties on Joined and the previous tie-break (lowest address) handed org
// ownership, and with it admin on every project in the org, to whoever picked
// the smallest email.
//
// That fallback is only as good as the order it reads. BuiltinAuth.Accounts
// ranges a Go map and sorts with sort.Slice (NOT stable) on Created — so when
// the accounts carry no Created stamp, which is exactly the upgraded hub the
// fallback exists for, the "oldest first" list is a random permutation that
// differs on every process. The heir is therefore drawn at random from the
// members, and the round-8 escalation is back: an ordinary member holding no
// grant on anything can be promoted to org owner by the most routine operator
// action there is.
//
// Two hubs built from byte-identical state must promote the same member.
func TestSec_Org_HeirIsNotDrawnFromMapIterationOrder(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			seen := map[string]int{}
			for i := 0; i < 12; i++ {
				secpgLegacyOrg(t, be)
				seen[secpgHeirAfterOffboard(t, be)]++
			}
			if len(seen) > 1 {
				t.Errorf("the same hub state promoted %d different members to org OWNER over 12 "+
					"identical runs: %v — org ownership carries admin on every project in the org, "+
					"and which ordinary member receives it is decided by Go map iteration order, "+
					"not by any fact about the accounts", len(seen), seen)
			}
		})
	}
}

// The seniority list itself, isolated from the heir. AuthProvider.Accounts is
// documented "oldest first" and three call sites trust it: the pre-org
// migration's choice of default-org owner, PendingUsers' display, and the heir
// tie-break. With no Created stamps there is no oldest, and the contract is
// silently answered with noise instead of with an empty or stable list.
func TestSec_Auth_AccountsOrderIsStableAcrossReloads(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)
			for _, e := range []string{"zoe@x.io", "adam@x.io", "mia@x.io", "kim@x.io", "bo@x.io"} {
				if err := st.Accounts().PutAccount(&authUser{
					ID: "u-" + e, Email: e, Name: e, Pass: "x", Status: statusActive,
				}); err != nil {
					t.Fatal(err)
				}
			}
			st.Close()

			orders := map[string]int{}
			for i := 0; i < 12; i++ {
				st2 := be.open(t)
				auth, err := NewBuiltinAuth(st2.Accounts(), true, nil)
				if err != nil {
					t.Fatal(err)
				}
				var order []string
				for _, u := range auth.Accounts() {
					order = append(order, u.Email)
				}
				orders[strings.Join(order, ",")]++
				st2.Close()
			}
			if len(orders) > 1 {
				t.Errorf("Accounts() returned %d different orders for one unchanged store: %v — "+
					"it is documented \"oldest first\" and the org heir is decided off it",
					len(orders), orders)
			}
		})
	}
}

// ---- one store, two hub processes ---------------------------------------

// A grant revoked by one hub process must not be restored by another.
//
// The reason to configure Postgres at all is to put more than one hub in front
// of one database. Every registry (ProjectDB, OrgDB, ShareDB) loads its rows
// once at construction and never re-reads them, and every write replaces the
// WHOLE record — sqlProjectRepo.Put deletes project_perms for the project and
// re-inserts the writer's in-memory map. So any write by a process that has
// not seen a revocation resurrects the revoked grant, with no error and no
// conflict: the second process is not racing, it is a minute behind and
// authoritative anyway.
func TestSec_DB_ARevokedGrantIsNotRestoredByASecondHubProcess(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)

			// Hub A and hub B, both live, both over the same store.
			stA := be.open(t)
			defer stA.Close()
			hubA, err := NewProjectDB(stA.Projects())
			if err != nil {
				t.Fatal(err)
			}
			p, _, err := hubA.GetOrCreate("wiki", "o-1")
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range []string{"boss@x.io", "eve@x.io"} {
				if err := hubA.SetPerm(p.ID, e, PermAdmin); err != nil {
					t.Fatal(err)
				}
			}
			stB := be.open(t)
			defer stB.Close()
			hubB, err := NewProjectDB(stB.Projects())
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := hubB.Get(p.ID); got.Perms["eve@x.io"] != PermAdmin {
				t.Fatalf("harness: hub B should start out seeing eve as admin, saw %q",
					got.Perms["eve@x.io"])
			}

			// An admin revokes eve on hub A.
			if err := hubA.SetPerm(p.ID, "eve@x.io", PermNone); err != nil {
				t.Fatal(err)
			}
			// Hub B does something entirely unrelated to eve on the same project.
			if err := hubB.Rename(p.ID, "notes"); err != nil {
				t.Fatal(err)
			}

			// The next hub to start reads the database.
			stC := be.open(t)
			defer stC.Close()
			hubC, err := NewProjectDB(stC.Projects())
			if err != nil {
				t.Fatal(err)
			}
			got, _ := hubC.Get(p.ID)
			if got.Perms["eve@x.io"] == PermAdmin {
				t.Errorf("eve is %q in the database after her grant was revoked on one hub and a "+
					"second hub renamed the project: the rename rewrote the whole grant set from a "+
					"stale in-memory copy, so an unrelated write by any other process undoes every "+
					"revocation it has not seen", got.Perms["eve@x.io"])
			}
		})
	}
}

// ---- clean: attacks the SQL layer correctly refuses ----------------------

// The ?→$N rewrite is only live on Postgres, so until this round it had never
// run against the engine it exists for. Values that look like a placeholder in
// either dialect must stay data: q() must not see them, and the driver must
// not re-interpret them.
func TestSec_DB_PlaceholderLookalikesInValuesStayData(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)
			defer st.Close()
			values := []string{"a?b", "$1", "?, ?", "$2 OR 1=1", "a'--", `\'; DROP TABLE shares;--`, "%_"}
			for i, v := range values {
				tok := fmt.Sprintf("secpg-tok-%d", i)
				if err := st.Shares().Put(Share{Token: tok, Project: "p-1", Path: v, Creator: v}); err != nil {
					t.Fatalf("value %q was refused: %v", v, err)
				}
			}
			rows, err := st.Shares().Load()
			if err != nil {
				t.Fatalf("shares table gone after the sweep: %v", err)
			}
			if len(rows) != len(values) {
				t.Fatalf("wrote %d shares, %d survived — a value shifted or destroyed a row",
					len(values), len(rows))
			}
			byTok := map[string]Share{}
			for _, s := range rows {
				byTok[s.Token] = s
			}
			for i, v := range values {
				tok := fmt.Sprintf("secpg-tok-%d", i)
				s, ok := byTok[tok]
				if !ok || s.Path != v || s.Creator != v || s.Project != "p-1" {
					t.Errorf("value %q did not round-trip as data: %+v ok=%v", v, s, ok)
				}
			}
		})
	}
}

// A key too large for the backend's index must fail loudly, never land
// half-written. Postgres refuses a btree key over ~2704 bytes (SQLSTATE 54000)
// where sqlite and the file backend take it; either answer is safe, silently
// dropping the row is not.
func TestSec_DB_AnOversizedKeyIsRefusedNotSilentlyDropped(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)
			long := randHex(2000) // 4000 chars, incompressible
			err := st.Reads().PutBatch([]ReadStat{{Project: "secpg-big", Path: long,
				Kind: ReadKindHuman, Actor: "eve@x.io", Count: 1}})
			st.Close()

			st2 := be.open(t)
			defer st2.Close()
			rows, lerr := st2.Reads().Load()
			if lerr != nil {
				t.Fatalf("read_stats unreadable after an oversized key: %v", lerr)
			}
			found := ""
			for _, s := range rows {
				if s.Project == "secpg-big" {
					found = s.Path
				}
			}
			switch {
			case err != nil && found != "":
				t.Errorf("PutBatch reported %v but the row is in the store anyway", err)
			case err == nil && found == "":
				t.Errorf("PutBatch reported success and the row is not in the store after a " +
					"reload — a silent loss the ledger would never retry")
			case err == nil && found != long:
				t.Errorf("the key was truncated in the store: %d chars in, %d out", len(long), len(found))
			}
		})
	}
}

// Concurrent writes through one *sql.DB pool must all reach the store: a row
// dropped under load is a device-ownership row missing from the table
// journal ownership resolves against.
func TestSec_DB_ConcurrentWritesAllReachTheStore(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			secdefDropDeviceRows(t)
			st := be.open(t)
			reg, err := NewDeviceRegistry(st.Devices())
			if err != nil {
				t.Fatal(err)
			}
			const n = 120
			done := make(chan struct{}, n)
			for i := 0; i < n; i++ {
				go func(i int) {
					reg.Observe(DeviceInfo{
						User: fmt.Sprintf("u%03d@x.io", i), ID: fmt.Sprintf("secpgd%03d", i),
						Name: "box", OS: "linux", LastSeen: time.Now().UTC(),
					})
					done <- struct{}{}
				}(i)
			}
			for i := 0; i < n; i++ {
				<-done
			}
			st.Close()

			st2 := be.open(t)
			defer st2.Close()
			rows, err := st2.Devices().Load()
			if err != nil {
				t.Fatal(err)
			}
			have := map[string]bool{}
			for _, d := range rows {
				have[d.ID] = true
			}
			for i := 0; i < n; i++ {
				if id := fmt.Sprintf("secpgd%03d", i); !have[id] {
					t.Fatalf("device %s was observed but is not in the store after a reload — "+
						"%d/%d rows durable under concurrent writes", id, len(have), n)
				}
			}
		})
	}
}

// A multi-row write must not half-land. sqlOrgRepo.PutOrg inserts the org row,
// deletes the org's member rows, then re-inserts them one statement at a time;
// sqlProjectRepo.Put does the same over project_perms. If a member insert
// fails partway (Postgres refuses the value, the connection drops), the
// members are already deleted — an org or project that comes back from the
// rollback point with FEWER grants than it had, or with a row and no members,
// is a silent authorization change. Only Postgres refuses anything mid-write
// today, so this assertion is only meaningful with the DSN set.
func TestSec_DB_APartialMultiRowWriteRollsBackWhole(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)

			good := map[string]string{"boss@x.io": RoleOwner, "bob@x.io": RoleMember}
			if err := st.Orgs().PutOrg(Org{ID: "o-tx", Name: "Acme",
				Members: good, Joined: map[string]time.Time{}}); err != nil {
				t.Fatal(err)
			}
			if err := st.Projects().Put(Project{ID: "p-tx", Name: "wiki", Org: "o-tx",
				Perms: map[string]string{"boss@x.io": PermAdmin, "bob@x.io": PermRead}}); err != nil {
				t.Fatal(err)
			}

			// The same records plus one row the store may refuse. Go map order
			// decides whether the bad row is written first or last, so run it
			// enough times to cover both.
			for i := 0; i < 8; i++ {
				poisoned := map[string]string{"boss@x.io": RoleOwner, "bob@x.io": RoleMember, "ev\x00il@x.io": RoleOwner}
				orgErr := st.Orgs().PutOrg(Org{ID: "o-tx", Name: "Acme",
					Members: poisoned, Joined: map[string]time.Time{}})
				projErr := st.Projects().Put(Project{ID: "p-tx", Name: "wiki", Org: "o-tx",
					Perms: map[string]string{"boss@x.io": PermAdmin, "bob@x.io": PermRead, "ev\x00il@x.io": PermAdmin}})
				if orgErr == nil && projErr == nil {
					continue // this backend stores it; nothing to roll back
				}

				st.Close()
				st2 := be.open(t)
				orgs, _, err := st2.Orgs().Load()
				if err != nil {
					t.Fatalf("orgs unreadable after a refused write: %v", err)
				}
				for _, o := range orgs {
					if o.ID != "o-tx" {
						continue
					}
					for e, want := range good {
						if o.Members[e] != want {
							t.Fatalf("run %d: the refused PutOrg (%v) took %s's %s role with it — "+
								"members after rollback: %v", i, orgErr, e, want, o.Members)
						}
					}
				}
				projects, err := st2.Projects().Load()
				if err != nil {
					t.Fatalf("projects unreadable after a refused write: %v", err)
				}
				for _, p := range projects {
					if p.ID != "p-tx" {
						continue
					}
					if p.Perms["boss@x.io"] != PermAdmin || p.Perms["bob@x.io"] != PermRead {
						t.Fatalf("run %d: the refused Put (%v) dropped grants — perms after "+
							"rollback: %v", i, projErr, p.Perms)
					}
				}
				st2.Close()
				st = be.open(t)
			}
			st.Close()
		})
	}
}

// Every repo write is a retry candidate — the ReadLedger retries a refused
// flush bucket by bucket, DeviceRegistry re-observes on the next sync cycle,
// and a caller that timed out on a committed write repeats it. Replaying the
// identical record must be a no-op, never a second row and never an error that
// makes the caller think the first one did not land.
func TestSec_DB_RepoWritesAreIdempotentOnRetry(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			secdefDropDeviceRows(t)
			st := be.open(t)
			defer st.Close()

			now := time.Now().UTC()
			write := func() error {
				if err := st.Accounts().PutAccount(&authUser{ID: "u-idem", Email: "idem@x.io",
					Name: "Idem", Pass: "hash", Status: statusActive, Created: now}); err != nil {
					return fmt.Errorf("account: %w", err)
				}
				if err := st.Accounts().PutToken(authToken{Hash: "h-idem", User: "u-idem",
					Device: "d-idem", Created: now}); err != nil {
					return fmt.Errorf("token: %w", err)
				}
				if err := st.Projects().Put(Project{ID: "p-idem", Name: "wiki", Org: "o-idem",
					Created: now, Perms: map[string]string{"boss@x.io": PermAdmin}}); err != nil {
					return fmt.Errorf("project: %w", err)
				}
				if err := st.Orgs().PutOrg(Org{ID: "o-idem", Name: "Acme", Created: now,
					Members: map[string]string{"boss@x.io": RoleOwner},
					Joined:  map[string]time.Time{"boss@x.io": now}}); err != nil {
					return fmt.Errorf("org: %w", err)
				}
				if err := st.Orgs().PutInvite(OrgInvite{Token: "inv-idem", Org: "o-idem",
					Creator: "boss@x.io", Created: now, Expires: now.Add(time.Hour), Uses: 2}); err != nil {
					return fmt.Errorf("invite: %w", err)
				}
				if err := st.Shares().Put(Share{Token: "s-idem", Project: "p-idem", Path: "a.md",
					Creator: "boss@x.io", Created: now}); err != nil {
					return fmt.Errorf("share: %w", err)
				}
				if err := st.Devices().Put(DeviceInfo{User: "boss@x.io", ID: "d-idem", Name: "box",
					OS: "linux", IP: "1.2.3.4", FirstSeen: now, LastSeen: now}); err != nil {
					return fmt.Errorf("device: %w", err)
				}
				return st.Reads().PutBatch([]ReadStat{{Project: "p-idem", Path: "a.md", Day: "2026-01-01",
					Kind: ReadKindHuman, Actor: "boss@x.io", Count: 3, Last: now}})
			}
			if err := write(); err != nil {
				t.Fatal(err)
			}
			count := func() [8]int {
				u, tk, _, _ := st.Accounts().Load()
				p, _ := st.Projects().Load()
				o, iv, _ := st.Orgs().Load()
				sh, _ := st.Shares().Load()
				d, _ := st.Devices().Load()
				rs, _ := st.Reads().Load()
				return [8]int{len(u), len(tk), len(p), len(o), len(iv), len(sh), len(d), len(rs)}
			}
			before := count()
			for i := 0; i < 3; i++ {
				if err := write(); err != nil {
					t.Fatalf("replay %d of an already-committed write was refused: %v — a caller "+
						"retrying a write it could not confirm is told the record does not exist",
						i+1, err)
				}
			}
			if after := count(); after != before {
				t.Errorf("replaying the identical write changed the row counts: %v -> %v — "+
					"a retry duplicated a record", before, after)
			}
			// The one field a replay must NOT move: a device row's first_seen is
			// what claimedBefore resolves ownership against.
			rows, err := st.Devices().Load()
			if err != nil {
				t.Fatal(err)
			}
			for _, d := range rows {
				if d.ID == "d-idem" && !d.FirstSeen.Equal(now) {
					t.Errorf("a replay moved device first_seen from %v to %v — the ownership "+
						"tie-break is rewritable by repeating a write", now, d.FirstSeen)
				}
			}
		})
	}
}

// A schema round-trip must not widen a project's permissions.
//
// Project.Default is "the level every org member gets without an explicit
// grant", and the empty string means WRITE (perms.go: "today's behavior ...
// so an existing hub upgrades with no migration"). That reverse-compatibility
// choice is safe going forward and fail-OPEN going backward: the column is
// `default_level TEXT NOT NULL DEFAULT ”`, added by addColumns, so any path
// that loses it — a rollback to a release whose schema predates it, a restore
// from a dump taken before it, a migration that half-applies — silently
// re-reads every locked-down project as writable by every member of its org.
//
// Nothing warns, because ” is a legal value that the code is built to read as
// permissive. Assert the outcome instead: a project an admin set to `none`
// must not come back as `write`.
func TestSec_DB_ASchemaRoundTripDoesNotWidenAProjectDefault(t *testing.T) {
	db, dsn := secpgSQL(t)

	st, err := OpenSQLStore("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	projects, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := projects.GetOrCreate("secrets", "o-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.SetDefault(p.ID, PermNone); err != nil {
		t.Fatal(err)
	}
	if got, _ := projects.Get(p.ID); got.level() != PermNone {
		t.Fatalf("harness: the project should be locked down, level is %q", got.level())
	}
	st.Close()

	// The schema of a release that predates the column — what a rollback, or a
	// restore from an older dump, leaves behind.
	if _, err := db.Exec(`ALTER TABLE projects DROP COLUMN default_level`); err != nil {
		t.Fatal(err)
	}

	st2, err := OpenSQLStore("pgx", dsn)
	if err != nil {
		// Refusing to open is a perfectly good answer: fail closed, loudly.
		t.Logf("store refused to open against the older schema: %v", err)
		return
	}
	defer st2.Close()
	projects2, err := NewProjectDB(st2.Projects())
	if err != nil {
		t.Logf("registry refused to load against the older schema: %v", err)
		return
	}
	got, ok := projects2.Get(p.ID)
	if !ok {
		t.Fatal("project vanished")
	}
	if got.level() == PermWrite {
		t.Errorf("a project an admin locked to %q reads as %q after the schema lost "+
			"default_level and addColumns re-added it with DEFAULT '': every member of org %s "+
			"can now upload to, sync into and mint share links on it, and the hub started "+
			"cleanly without a word", PermNone, got.level(), got.Org)
	}
}
