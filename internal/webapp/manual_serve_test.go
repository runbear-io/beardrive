package webapp

// Temporary manual demo harness: a seeded hub (~500 files, zipf-ish read
// data, four agent devices) for exploring the web UI by hand. Not part of
// the test suite: it only runs with BDRIVE_MANUAL_SERVE=1 and must never be
// committed.
//
// State lives in a STABLE directory (os.TempDir()/bdrive-demo-hub, override
// with BDRIVE_MANUAL_STATE), so restarting the harness — e.g. after a
// frontend change — keeps accounts, browser sessions, the project id, and
// all seeded/demo data. Delete the directory to reset the demo.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

func TestManualServe(t *testing.T) {
	if os.Getenv("BDRIVE_MANUAL_SERVE") == "" {
		t.Skip("manual demo harness; set BDRIVE_MANUAL_SERVE=1 to run")
	}
	state := os.Getenv("BDRIVE_MANUAL_STATE")
	if state == "" {
		state = filepath.Join(os.TempDir(), "bdrive-demo-hub")
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
	p, _, err := db.GetOrCreate("proj", "") // create-or-join: id is stable across restarts
	if err != nil {
		t.Fatal(err)
	}
	srv := &Server{Root: be, Projects: db, Device: webDevice, Refresh: 0, Upload: UploadConfig{Enabled: true}}

	prefix := filepath.Join(state, "storage", p.ID)
	seedMark := filepath.Join(prefix, "journal", "seed.jsonl")
	if _, err := os.Stat(seedMark); os.IsNotExist(err) {
		seedDemo(t, state, prefix, p.ID)
	}

	srv.Reads, err = OpenReadLedger(filepath.Join(state, "reads.json"), 0)
	if err != nil {
		t.Fatal(err)
	}
	srv.Devices, _ = OpenDeviceRegistry(filepath.Join(state, "devices.json"))
	srv.Devices.Observe(DeviceInfo{ID: "dev-ci", Name: "claude-ci", OS: "linux/amd64"})
	srv.Devices.Observe(DeviceInfo{ID: "dev-snow", Name: "claude-snow", OS: "darwin/arm64"})
	srv.Devices.Observe(DeviceInfo{ID: "codex-mia", Name: "codex-mia", OS: "darwin/arm64"})
	srv.Devices.Observe(DeviceInfo{ID: "gemini-doc", Name: "gemini-doc", OS: "linux/amd64"})

	srv.Shares, _ = OpenShareDB(filepath.Join(state, "shares.json"))

	auth, err := OpenBuiltinAuth(filepath.Join(state, "auth.json"), false, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth.signup("snow@runbear.io", "Snow", "password1") // no-op if the account exists
	auth.Admins = map[string]bool{"snow@runbear.io": true}
	srv.Auth = auth

	t.Logf("serving on http://0.0.0.0:8993 (state: %s) — snow@runbear.io / password1", state)
	go http.ListenAndServe("0.0.0.0:8993", srv.Handler())
	time.Sleep(8 * time.Hour)
}

// seedDemo writes ~500 files (journal + blobs) and their read buckets. Runs
// once per state dir.
func seedDemo(t *testing.T, state, prefix, projectID string) {
	t.Helper()
	os.MkdirAll(filepath.Join(prefix, "journal"), 0o755)
	os.MkdirAll(filepath.Join(prefix, "blobs"), 0o755)
	seed := int64(42)
	rnd := func() float64 {
		seed = (seed*16807 + 7) % 2147483647
		return float64(seed) / 2147483647
	}
	folders := []struct {
		dir string
		n   int
	}{
		{"wiki/onboarding", 28}, {"wiki/architecture", 40}, {"wiki/runbooks", 55},
		{"wiki/api", 70}, {"wiki/decisions", 45}, {"docs/product", 60},
		{"docs/design", 35}, {"notes/meetings", 80}, {"notes/research", 45},
		{"shared/reports", 42},
	}
	humans := []string{"alice@x.io", "bob@x.io", "carol@x.io"}
	agents := []string{"dev-ci", "dev-snow", "codex-mia", "gemini-doc"}

	now := time.Now().UTC()
	var ops []journal.Op
	var stats []ReadStat
	var lam, seq int64
	fileNo := 0
	for _, f := range folders {
		short := filepath.Base(f.dir)[:3]
		for i := 0; i < f.n; i++ {
			fileNo++
			path := fmt.Sprintf("%s/%s-%03d.md", f.dir, short, i+1)
			content := fmt.Sprintf("# %s\n\nSeeded demo file %d in %s.\n", path, fileNo, f.dir)
			sum := sha256.Sum256([]byte(content))
			blob := hex.EncodeToString(sum[:])
			os.WriteFile(filepath.Join(prefix, "blobs", blob), []byte(content), 0o644)
			stale := int(400 * rnd() * rnd())
			lam++
			seq++
			ops = append(ops, journal.Op{
				Seq: seq, Lamport: lam, Time: now.AddDate(0, 0, -stale),
				Device: "seed", DeviceName: "seed", Author: "alice@x.io",
				User: "alice@x.io", UserName: "Alice",
				Kind: journal.KindPut, Path: path, Blob: blob,
				Size: int64(len(content)), Mode: 0o644,
			})
			hot := rnd() < 0.15
			day := now.AddDate(0, 0, -int(rnd()*20)).Format("2006-01-02")
			for _, a := range humans {
				n := int64(math.Floor((map[bool]float64{true: 30, false: 3}[hot]) * rnd() * rnd()))
				if n > 0 {
					stats = append(stats, ReadStat{Project: projectID, Path: path, Day: day,
						Kind: ReadKindHuman, Actor: a, Count: n, Last: now})
				}
			}
			for ai, a := range agents {
				boost := 1.0
				if f.dir == "wiki/runbooks" && ai < 2 {
					boost = 4
				}
				if f.dir == "notes/research" {
					boost = 0.05
				}
				n := int64(math.Floor((map[bool]float64{true: 60, false: 5}[hot]) * rnd() * rnd() * boost))
				if n > 0 {
					stats = append(stats, ReadStat{Project: projectID, Path: path, Day: day,
						Kind: ReadKindAgent, Actor: a, Count: n, Last: now})
				}
			}
		}
	}
	if err := journal.Append(filepath.Join(prefix, "journal", "seed.jsonl"), ops); err != nil {
		t.Fatal(err)
	}
	if err := newFileReadRepo(filepath.Join(state, "reads.json")).PutBatch(stats); err != nil {
		t.Fatal(err)
	}
}
