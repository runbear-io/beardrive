package webapp

// E2E hub harness for the frontend's Playwright suite (frontend/e2e). Not
// part of the normal test suite: it only runs with BDRIVE_E2E_SERVE=1, where
// it serves a small deterministic hub on :8993 until killed. Playwright
// starts it via its webServer config and tears it down after the run.
//
// Unlike the manual demo harness, state is wiped on every start so the
// tests always see the same world: one org ("default", owned by the admin
// account), one project ("wiki") with a handful of seeded files, two
// accounts, one share-ready markdown tree with wikilinks, and enough read
// heat to light up the insights views.

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

const (
	e2eAddr     = "0.0.0.0:8993"
	e2eAdmin    = "e2e@example.com"
	e2eMember   = "member@example.com"
	e2eSolo     = "solo@example.com"
	e2eReader   = "reader@example.com" // org member with a read-only grant on "wiki"
	e2ePassword = "e2e-pass-1"
)

func TestE2EServe(t *testing.T) {
	if os.Getenv("BDRIVE_E2E_SERVE") == "" {
		t.Skip("frontend e2e harness; set BDRIVE_E2E_SERVE=1 to run")
	}
	state := filepath.Join(os.TempDir(), "bdrive-e2e-hub")
	if err := os.RemoveAll(state); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(state, "storage"), 0o755); err != nil {
		t.Fatal(err)
	}

	be, err := remote.Open(t.Context(), "file://"+filepath.Join(state, "storage"))
	if err != nil {
		t.Fatal(err)
	}
	db, err := OpenProjectDB(filepath.Join(state, "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	p, _, err := db.GetOrCreate("wiki", "")
	if err != nil {
		t.Fatal(err)
	}
	// Volume is deliberately the lowercase storage basename, so hub.spec.ts's
	// "#vault-name reads BearDrive" assertion actually catches a brand that
	// falls back to storage instead of the product name.
	srv := &Server{Root: be, Projects: db, Device: webDevice, Refresh: 0, Volume: "beardrive", Upload: UploadConfig{Enabled: true}}

	seedE2E(t, state, filepath.Join(state, "storage", p.ID), p.ID)

	srv.Reads, err = OpenReadLedger(filepath.Join(state, "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "seed", Name: "seed-agent", OS: "linux/amd64"})

	auth, err := OpenBuiltinAuth(filepath.Join(state, "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eAdmin, "E2E Admin", e2ePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eMember, "E2E Member", e2ePassword); err != nil {
		t.Fatal(err)
	}
	// In no org: sees the onboarding empty state (and creating a project
	// from it mints a fresh org via orgForCreate).
	if _, err := auth.signup(e2eSolo, "E2E Solo", e2ePassword); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.signup(e2eReader, "E2E Reader", e2ePassword); err != nil {
		t.Fatal(err)
	}
	auth.Admins = map[string]bool{e2eAdmin: true}
	srv.Auth = auth

	orgs, err := OpenOrgDB(filepath.Join(state, "orgs.json"))
	if err != nil {
		t.Fatal(err)
	}
	org, err := orgs.Create("default", e2eAdmin)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{e2eMember, e2eReader} {
		if err := orgs.AddMember(org.ID, m, RoleMember); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.SetOrg(p.ID, org.ID); err != nil {
		t.Fatal(err)
	}
	// One member cut back to read: the suite checks that write affordances
	// are absent for them, not merely that the server would 403.
	if err := db.SetPerm(p.ID, e2eReader, PermRead); err != nil {
		t.Fatal(err)
	}
	srv.Dir = LocalDirectory{OrgDB: orgs}
	auth.InviteValid = orgs.ValidInvite

	shares, err := OpenShareDB(filepath.Join(state, "shares.json"))
	if err != nil {
		t.Fatal(err)
	}
	srv.Shares = shares

	t.Logf("e2e hub on http://%s (state: %s) — %s / %s", e2eAddr, state, e2eAdmin, e2ePassword)
	go http.ListenAndServe(e2eAddr, srv.Handler())
	time.Sleep(2 * time.Hour) // Playwright kills the process when the run ends
}

// seedE2E journals a small fixed file tree (with history on one path and one
// binary) and read heat for it.
func seedE2E(t *testing.T, state, prefix, projectID string) {
	t.Helper()
	os.MkdirAll(filepath.Join(prefix, "journal"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "blobs"), 0o755)
	now := time.Now().UTC()
	var ops []journal.Op
	var lam, seq int64
	put := func(path, content string, age time.Duration) {
		sum := sha256.Sum256([]byte(content))
		blob := hex.EncodeToString(sum[:])
		if err := os.WriteFile(filepath.Join(prefix, "blobs", blob), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		lam++
		seq++
		ops = append(ops, journal.Op{
			Seq: seq, Lamport: lam, Time: now.Add(-age),
			Device: "seed", DeviceName: "seed-agent", Author: "alice@x.io",
			User: "alice@x.io", UserName: "Alice",
			Kind: journal.KindPut, Path: path, Blob: blob,
			Size: int64(len(content)), Mode: 0o644,
		})
	}
	put("index.md", "# Wiki\n\nStart at the [[guide]] or browse [notes](notes/readme.md).\n", 72*time.Hour)
	put("guide.md", "# Guide\n\nFirst version of the guide.\n", 48*time.Hour)
	put("guide.md", "# Guide\n\nSecond version of the guide, with more detail.\n", 2*time.Hour)
	put("notes/readme.md", "# Notes\n\nNested folder content.\n", 24*time.Hour)
	put("notes/deep/topic.md", "# Topic\n\nDeeply nested file.\n", 24*time.Hour)
	// Tiny valid PNG (1x1), enough to exercise the binary/download path.
	png := "\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89" +
		"\x00\x00\x00\nIDATx\x9cc\x00\x01\x00\x00\x05\x00\x01\r\n-\xb4\x00\x00\x00\x00IEND\xaeB`\x82"
	put("assets/logo.png", png, 24*time.Hour)
	if err := journal.Append(filepath.Join(prefix, "journal", "seed.jsonl"), ops); err != nil {
		t.Fatal(err)
	}

	day := now.Format("2006-01-02")
	var stats []ReadStat
	for _, rd := range []struct {
		path  string
		kind  string
		actor string
		n     int64
	}{
		{"index.md", ReadKindHuman, "alice@x.io", 12},
		{"index.md", ReadKindAgent, "seed", 30},
		{"guide.md", ReadKindHuman, "bob@x.io", 5},
		{"guide.md", ReadKindAgent, "seed", 9},
		{"notes/readme.md", ReadKindAgent, "seed", 2},
	} {
		stats = append(stats, ReadStat{Project: projectID, Path: rd.path, Day: day,
			Kind: rd.kind, Actor: rd.actor, Count: rd.n, Last: now})
	}
	if err := newFileReadRepo(filepath.Join(state, "reads.json")).PutBatch(stats); err != nil {
		t.Fatal(err)
	}
}
