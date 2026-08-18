package webapp

// post_sync against the real binary and a real volume flock: a hook that runs
// a bdrive subcommand — the case the whole "fire outside the lock" rule exists
// for — receives the batch on stdin and runs to completion.
//
// It does NOT prove the ordering, and the plan's claim that it would is wrong:
// the child is detached and nothing waits on it, so a hook spawned INSIDE the
// flock stalls on LOCK_EX until the parent's cycle ends rather than deadlocking
// (verified by moving the call and watching this test still pass). Firing after
// the unlock is still the right shape — a hook must not queue behind a long
// push — but it is a code-shape property, guarded by Cycle being a wrapper,
// not by an assertion here.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

func TestCLIPostSyncRunningBdriveDoesNotDeadlock(t *testing.T) {
	hub := startTestHub(t)
	first := newCLIEnvOn(t, hub)

	owner := filepath.Join(t.TempDir(), "team")
	if err := os.MkdirAll(owner, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owner, "seed.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := first.run(owner, "init", "--name", "postsync-e2e", "--yes"); err != nil {
		t.Fatalf("init owner: %v\n%s", err, out)
	}
	defer first.run(owner, "stop", owner)

	second := newCLIEnvOn(t, hub)
	joiner := filepath.Join(t.TempDir(), "joined")
	if err := os.MkdirAll(joiner, 0o755); err != nil {
		t.Fatal(err)
	}
	id := projectIDByName(t, first.browser, hub.URL, "postsync-e2e")
	if out, err := second.run(joiner, "init", "--project", id, "--yes"); err != nil {
		t.Fatalf("init joiner: %v\n%s", err, out)
	}
	defer second.run(joiner, "stop", joiner)

	// The hook runs a bdrive command that takes the same volume flock the
	// cycle spawning it just held, then leaves a marker holding the batch.
	marker := filepath.Join(t.TempDir(), "marker.json")
	batchFile := filepath.Join(t.TempDir(), "batch.json")
	cfgPath := filepath.Join(joiner, ".bdrive", "config.json")
	proj, ok, err := config.LoadProject(joiner)
	if err != nil || !ok {
		t.Fatalf("load joiner project: %v (ok=%v)", err, ok)
	}
	proj.PostSync = "cat > " + batchFile + " && " + second.bin + " sync " + joiner + " > /dev/null 2>&1 && mv " + batchFile + " " + marker
	body, err := json.Marshal(proj)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, body, 0o600); err != nil {
		t.Fatal(err)
	}

	// A teammate's change, then the joiner picks it up.
	if err := os.WriteFile(filepath.Join(owner, "from-owner.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := first.run(owner, "sync"); err != nil {
		t.Fatalf("owner sync: %v\n%s", err, out)
	}
	if out, err := second.run(joiner, "sync"); err != nil {
		t.Fatalf("joiner sync: %v\n%s", err, out)
	}

	// Whichever cycle applied it — this sync or the joiner's daemon — the hook
	// must have run its bdrive command to completion.
	deadline := time.Now().Add(60 * time.Second)
	for {
		raw, err := os.ReadFile(marker)
		if err == nil {
			var got struct {
				Changed []struct{ Path, Op string } `json:"changed"`
			}
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("post_sync stdin was not the JSON batch: %v (%q)", err, raw)
			}
			var found bool
			for _, c := range got.Changed {
				if c.Path == "from-owner.md" && c.Op == "write" {
					found = true
				}
			}
			if !found {
				t.Fatalf("batch %+v does not name the teammate's file", got.Changed)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("post_sync running `bdrive sync` never completed — spawned inside the volume flock?")
		}
		time.Sleep(200 * time.Millisecond)
	}
}
