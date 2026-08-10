package webapp

// Security tests for the hub's metadata store (scoreboard row 14).
//
// Everything the hub uses to decide "who may reach what" — accounts, orgs and
// their member roles, projects and their grants, invites, shares, devices,
// read buckets — lives behind MetaStore. Two things must hold:
//
//   - attacker-supplied text (project/org names, emails, paths, device names,
//     tokens) is DATA on every backend, never SQL and never a way to reach
//     another tenant's rows
//   - a change the registry reports as applied is actually durable, and a
//     write the store refused leaves the live registry agreeing with disk
//
// Helpers are prefixed `secdb` per the harness rules; the backend matrix is
// `metaBackends` from db_conformance_test.go, reused rather than rebuilt.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// secdbHostile is the injection corpus: every string is chosen so that a
// backend which concatenated it into SQL would destroy or leak something.
var secdbHostile = struct {
	projectName, orgName, email, sharePath, deviceID, deviceName, readPath, readActor string
}{
	projectName: `'; DROP TABLE projects;--`,
	orgName:     `Acme'); DROP TABLE org_members;--`,
	email:       `bob'; DELETE FROM project_perms WHERE '1'='1`,
	sharePath:   `docs/100%_quarter's "report".md`,
	deviceID:    `d'1`,
	deviceName:  `lap"top'; DROP TABLE devices;--`,
	readPath:    `wiki/a%b_c'd.md`,
	readActor:   `spy'@x.io`,
}

// Hostile text must round-trip verbatim and must not disturb any other row,
// on every backend. A file backend that mangles JSON, or a SQL backend that
// string-built any statement, fails here.
func TestSec_DB_HostileStringsStayDataOnEveryBackend(t *testing.T) {
	for _, be := range metaBackends(t) {
		t.Run(be.name, func(t *testing.T) {
			be.reset(t)
			st := be.open(t)

			projects, err := NewProjectDB(st.Projects())
			if err != nil {
				t.Fatal(err)
			}
			// A control row in a different org — it must survive intact.
			ctl, _, err := projects.GetOrCreate("control", "o-control")
			if err != nil {
				t.Fatal(err)
			}
			if err := projects.SetPerm(ctl.ID, "keeper@x.io", PermAdmin); err != nil {
				t.Fatal(err)
			}

			bad, created, err := projects.GetOrCreate(secdbHostile.projectName, "o-1")
			if err != nil || !created {
				t.Fatalf("create hostile project: created=%v err=%v", created, err)
			}
			if err := projects.SetPerm(bad.ID, secdbHostile.email, PermAdmin); err != nil {
				t.Fatal(err)
			}

			orgs, err := NewOrgDB(st.Orgs())
			if err != nil {
				t.Fatal(err)
			}
			org, err := orgs.Create(secdbHostile.orgName, secdbHostile.email)
			if err != nil {
				t.Fatal(err)
			}
			inv, err := orgs.CreateInvite(org.ID, secdbHostile.email, time.Hour)
			if err != nil {
				t.Fatal(err)
			}

			shares, err := NewShareDB(st.Shares())
			if err != nil {
				t.Fatal(err)
			}
			sh, err := shares.Create(bad.ID, secdbHostile.sharePath, secdbHostile.email, 0, FileInfo{})
			if err != nil {
				t.Fatal(err)
			}

			devices, err := NewDeviceRegistry(st.Devices())
			if err != nil {
				t.Fatal(err)
			}
			devices.Observe(DeviceInfo{
				ID: secdbHostile.deviceID, Name: secdbHostile.deviceName,
				OS: `mac"'`, User: secdbHostile.email, IP: "1.2.3.4",
			})

			reads, err := NewReadLedger(st.Reads(), 0)
			if err != nil {
				t.Fatal(err)
			}
			reads.Record(bad.ID, secdbHostile.readPath, ReadKindHuman, secdbHostile.readActor)
			if err := reads.Close(); err != nil {
				t.Fatal(err)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}

			// ---- reopen: nothing executed, nothing was lost ----
			st2 := be.open(t)
			defer st2.Close()

			projects2, err := NewProjectDB(st2.Projects())
			if err != nil {
				t.Fatalf("projects table gone after hostile write: %v", err)
			}
			got, ok := projects2.Get(bad.ID)
			if !ok || got.Name != secdbHostile.projectName {
				t.Fatalf("hostile project name did not round-trip: %+v", got)
			}
			if got.Perms[normEmail(secdbHostile.email)] != PermAdmin {
				t.Fatalf("hostile grant did not round-trip: %+v", got.Perms)
			}
			keep, ok := projects2.Get(ctl.ID)
			if !ok || keep.Perms["keeper@x.io"] != PermAdmin {
				t.Fatalf("control project/grant destroyed by the hostile write: %+v ok=%v", keep, ok)
			}

			orgs2, err := NewOrgDB(st2.Orgs())
			if err != nil {
				t.Fatalf("orgs table gone after hostile write: %v", err)
			}
			ro, ok := orgs2.Get(org.ID)
			if !ok || ro.Name != secdbHostile.orgName {
				t.Fatalf("hostile org name did not round-trip: %+v", ro)
			}
			if ro.Members[normEmail(secdbHostile.email)] != RoleOwner {
				t.Fatalf("hostile owner email did not round-trip: %+v", ro.Members)
			}
			if !orgs2.ValidInvite(inv.Token) {
				t.Fatal("invite lost after the hostile write")
			}

			shares2, err := NewShareDB(st2.Shares())
			if err != nil {
				t.Fatalf("shares table gone after hostile write: %v", err)
			}
			gs, ok := shares2.Get(sh.Token)
			if !ok || gs.Path != secdbHostile.sharePath || gs.Project != bad.ID {
				t.Fatalf("hostile share path did not round-trip: %+v ok=%v", gs, ok)
			}

			devices2, err := NewDeviceRegistry(st2.Devices())
			if err != nil {
				t.Fatalf("devices table gone after hostile write: %v", err)
			}
			gd, ok := devices2.Get(secdbHostile.deviceID)
			if !ok || gd.Name != secdbHostile.deviceName {
				t.Fatalf("hostile device name did not round-trip: %+v ok=%v", gd, ok)
			}

			reads2, err := NewReadLedger(st2.Reads(), 0)
			if err != nil {
				t.Fatalf("read_stats gone after hostile write: %v", err)
			}
			if e := reads2.Heat(bad.ID, "", time.Time{})[secdbHostile.readPath]; e.Human != 1 {
				t.Fatalf("hostile read bucket did not round-trip: %+v", e)
			}
			// The `%`/`_` in the path must be literal, not a wildcard: a
			// prefix query for another folder must not pick it up.
			if e := reads2.Heat(bad.ID, "other", time.Time{}); len(e) != 0 {
				t.Fatalf("prefix query matched an unrelated path: %+v", e)
			}
		})
	}
}

// RETIRED (round 11): TestSec_DB_NULBytesDoNotTruncateRecords.
//
// It asserted that a NUL byte in a stored identifier must round-trip verbatim
// ("refused, or stored, but never silently lost" — round 5's rule, resolved in
// the "stored" direction). Postgres cannot implement that: a text column
// rejects 0x00 outright (SQLSTATE 22021), so satisfying it would mean moving
// the whole metadata layer to bytea. Until round 11 nobody had run this suite
// against Postgres, which is why the contradiction went seven rounds unseen.
//
// Round 11 resolved the same rule in the OTHER direction, at the repo boundary
// and identically on all three backends: unstorable text is REFUSED (see
// `storable` in db.go). That is what the ingest doors already enforce
// (printableOnly, hasControlChars, journal.SafePath), and it means a hub cannot
// change what it accepts by changing its database.
//
// The property this test was protecting — "a device registered as
// laptop\x00-of-eve must not come back as laptop, impersonating another
// device" — is protected more strongly by refusal, and is now asserted by
// TestSec_DB_EveryBackendAgreesWhichTextIsStorable and
// TestSec_DB_AcceptedTextIsStoredVerbatimOnEveryBackend in sec_pg_test.go.
// The two tests assert opposite decisions and cannot both be green; this is
// the one that was wrong.

// ---- registries must not hand out their live maps ------------------------

// OrgDB stores Org by value but Org.Members is a map, so Get/OrgsFor return a
// struct that still points at the registry's live membership map. Anything
// holding that "copy" can write roles straight into the registry — skipping
// the last-owner guard, skipping the MetaStore entirely (so the change never
// reaches orgs.json and is invisible to an audit), and, since handleOrgs
// ranges over it outside OrgDB's mutex, racing every concurrent AddMember /
// RemoveMember on the same org.
func TestSec_DB_OrgMemberMapDoesNotEscapeTheRegistry(t *testing.T) {
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	orgs, err := NewOrgDB(st.Orgs())
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("Acme", "boss@x.io")
	if err != nil {
		t.Fatal(err)
	}

	snapshot, ok := orgs.Get(org.ID)
	if !ok {
		t.Fatal("org vanished")
	}
	snapshot.Members["dave@x.io"] = RoleOwner // a caller editing its own copy
	if role := orgs.Role(org.ID, "dave@x.io"); role != "" {
		t.Errorf("writing to the Org returned by Get made dave %q in the live registry — "+
			"Get must return a defensive copy of Members", role)
	}

	for _, o := range orgs.OrgsFor("boss@x.io") {
		o.Members["mallory@x.io"] = RoleOwner
	}
	if role := orgs.Role(org.ID, "mallory@x.io"); role != "" {
		t.Errorf("writing to an Org returned by OrgsFor made mallory %q in the live registry", role)
	}
}

// Same aliasing for per-project grants: Project.Perms is handed out live by
// ProjectDB.Get and List, so a holder can mint itself PermAdmin on a project
// without going through SetPerm (no last-admin guard, no repo write).
func TestSec_DB_ProjectPermsMapDoesNotEscapeTheRegistry(t *testing.T) {
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	projects, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := projects.GetOrCreate("wiki", "o-1")
	if err != nil {
		t.Fatal(err)
	}
	if err := projects.SetPerm(p.ID, "boss@x.io", PermAdmin); err != nil {
		t.Fatal(err)
	}

	snapshot, _ := projects.Get(p.ID)
	snapshot.Perms["dave@x.io"] = PermAdmin
	if got, _ := projects.Get(p.ID); got.Perms["dave@x.io"] != "" {
		t.Errorf("writing to the Project returned by Get granted dave %q in the live registry — "+
			"Get must return a defensive copy of Perms", got.Perms["dave@x.io"])
	}
}

// ---- a change reported as applied must be durable ------------------------

// secdbFlakyOrgRepo is a MetaStore OrgRepo that can be told to refuse writes,
// standing in for a full disk, a read-only volume, or a Postgres blip.
type secdbFlakyOrgRepo struct {
	OrgRepo
	fail bool
}

func (r *secdbFlakyOrgRepo) PutOrg(o Org) error {
	if r.fail {
		return os.ErrPermission
	}
	return r.OrgRepo.PutOrg(o)
}

func (r *secdbFlakyOrgRepo) DeleteInvite(token string) error {
	if r.fail {
		return os.ErrPermission
	}
	return r.OrgRepo.DeleteInvite(token)
}

// Revoking an invite is the emergency stop for a leaked join link, and on an
// invite-only hub (the default) that link is enough for a stranger to create
// an account and join the org. OrgDB.RevokeInvite drops the invite from
// memory, throws away the store's error, and reports success — so a revoke
// that never reached the store looks revoked until the hub restarts, at which
// point the link works again.
func TestSec_DB_RevokedInviteMustNotSurviveAFailedWrite(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &secdbFlakyOrgRepo{OrgRepo: st.Orgs()}
	orgs, err := NewOrgDB(flaky)
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("Acme", "boss@x.io")
	if err != nil {
		t.Fatal(err)
	}
	inv, err := orgs.CreateInvite(org.ID, "boss@x.io", time.Hour)
	if err != nil {
		t.Fatal(err)
	}

	flaky.fail = true
	revoked := orgs.RevokeInvite(inv.Token)
	flaky.fail = false
	st.Close()

	st2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	orgs2, err := NewOrgDB(st2.Orgs())
	if err != nil {
		t.Fatal(err)
	}
	if revoked && orgs2.ValidInvite(inv.Token) {
		t.Fatalf("RevokeInvite reported success but the invite %s is live again after a restart — "+
			"the store refused the delete and the error was discarded", inv.Token)
	}
}

// secdbFlakyProjectRepo is the ProjectRepo equivalent.
type secdbFlakyProjectRepo struct {
	ProjectRepo
	fail bool
}

func (r *secdbFlakyProjectRepo) Put(p Project) error {
	if r.fail {
		return os.ErrPermission
	}
	return r.ProjectRepo.Put(p)
}

// ProjectDB applies a grant change to its in-memory map BEFORE persisting and
// does not undo it when the store refuses (GetOrCreate, in the same file, does
// roll back — so this is an inconsistency, not a design choice). A demotion
// that failed to persist therefore reads as applied at runtime and reverts to
// the old, higher level on the next restart: the account gets its admin back
// without anyone granting it.
func TestSec_DB_FailedGrantWriteLeavesRegistryAgreeingWithDisk(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	flaky := &secdbFlakyProjectRepo{ProjectRepo: st.Projects()}
	projects, err := NewProjectDB(flaky)
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := projects.GetOrCreate("wiki", "o-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range []string{"boss@x.io", "dave@x.io"} {
		if err := projects.SetPerm(p.ID, e, PermAdmin); err != nil {
			t.Fatal(err)
		}
	}

	flaky.fail = true
	demoteErr := projects.SetPerm(p.ID, "dave@x.io", PermNone)
	flaky.fail = false
	if demoteErr == nil {
		t.Fatal("harness: the store was supposed to refuse this write")
	}

	live, _ := projects.Get(p.ID)
	st.Close()
	st2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	projects2, err := NewProjectDB(st2.Projects())
	if err != nil {
		t.Fatal(err)
	}
	onDisk, _ := projects2.Get(p.ID)

	if live.Perms["dave@x.io"] != onDisk.Perms["dave@x.io"] {
		t.Fatalf("the refused demotion was applied in memory anyway: live=%q on disk=%q — "+
			"dave reads as demoted until the hub restarts, then is admin again",
			live.Perms["dave@x.io"], onDisk.Perms["dave@x.io"])
	}
}

// ---- the file backend's on-disk footprint --------------------------------

// auth.json (password hashes, token digests) and reads.json (actor emails)
// ask writeFileAtomic for a 0700 parent, but MkdirAll is a no-op on a
// directory that already exists — and projects/orgs/shares/devices ask for
// 0755 over the SAME directory. Whichever repo writes first, or whatever
// created the data directory, decides. Assert the secrets' directory is not
// group/world readable.
func TestSec_DB_FileBackendSecretsDirectoryIsNotWorldReadable(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "hubdata")
	st, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// The realistic order: a project is created before the first account.
	projects, err := NewProjectDB(st.Projects())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := projects.GetOrCreate("wiki", "o-1"); err != nil {
		t.Fatal(err)
	}
	auth, err := NewBuiltinAuth(st.Accounts(), true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup("dev@x.io", "Dev", "password1"); err != nil {
		t.Fatal(err)
	}

	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := fi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("hub data directory holding auth.json is %o, want no group/other access", mode)
	}
	afi, err := os.Stat(filepath.Join(dir, "auth.json"))
	if err != nil {
		t.Fatal(err)
	}
	if mode := afi.Mode().Perm(); mode&0o077 != 0 {
		t.Errorf("auth.json is %o, want no group/other access", mode)
	}
}
