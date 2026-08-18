package syncer

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
)

// postSync writes a folder's .bdrive/config.json with a post_sync command.
func postSync(t *testing.T, folder, cmd string) {
	t.Helper()
	if _, err := config.SaveProject(folder, config.Project{PostSync: cmd}); err != nil {
		t.Fatal(err)
	}
}

// waitForFile polls until path exists, because the hook is spawned detached:
// a bare assert right after cycle() races the child.
func waitForFile(t *testing.T, path string, d time.Duration) []byte {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
			return b
		}
		if time.Now().After(deadline) {
			t.Fatalf("post_sync marker %s never appeared", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// neverAppears is the negative of waitForFile: give the spawn a fair chance,
// then assert nothing ran.
func neverAppears(t *testing.T, path string) {
	t.Helper()
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(path); err == nil {
		t.Fatalf("post_sync ran but should not have (%s exists)", path)
	}
}

func batch(t *testing.T, raw []byte) postSyncPayload {
	t.Helper()
	var p postSyncPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatalf("post_sync stdin is not the JSON batch: %v (%q)", err, raw)
	}
	return p
}

// TestResultInboundNamesPeerPaths is step 1 on its own: the cycle carries the
// applied paths out on Result, and a local-edit-only cycle carries none.
func TestResultInboundNamesPeerPaths(t *testing.T) {
	rem := sharedRemote(t)
	a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)

	write(t, a.Folder, "wiki/onboarding.md", "hello")
	write(t, a.Folder, "notes/retired.md", "bye")
	cycle(t, a)

	res := cycle(t, b)
	if len(res.Inbound) != 2 {
		t.Fatalf("Result.Inbound = %v, want 2 writes", res.Inbound)
	}
	for _, e := range res.Inbound {
		if e.Deleted {
			t.Fatalf("%s reported as a delete, want a write", e.Path)
		}
	}

	if err := os.Remove(filepath.Join(a.Folder, "notes", "retired.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	res = cycle(t, b)
	if len(res.Inbound) != 1 || !res.Inbound[0].Deleted || res.Inbound[0].Path != "notes/retired.md" {
		t.Fatalf("Result.Inbound = %v, want one delete of notes/retired.md", res.Inbound)
	}

	// A cycle that only scans and pushes local work is not inbound.
	write(t, b.Folder, "mine.md", "local only")
	res = cycle(t, b)
	if len(res.Inbound) != 0 {
		t.Fatalf("local-edit cycle reported Inbound = %v, want none", res.Inbound)
	}
}

// TestPostSyncFiresOncePerInboundBatch is the "400 files, one invocation" AC.
func TestPostSyncFiresOncePerInboundBatch(t *testing.T) {
	rem := sharedRemote(t)
	a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)

	marker := filepath.Join(t.TempDir(), "marker.json")
	postSync(t, b.Folder, "cat >> "+marker)

	write(t, a.Folder, "one.md", "1")
	write(t, a.Folder, "two.md", "2")
	write(t, a.Folder, "wiki/three.md", "3")
	cycle(t, a)

	cycle(t, b)
	got := batch(t, waitForFile(t, marker, 5*time.Second))
	if len(got.Changed) != 3 {
		t.Fatalf("batch = %+v, want 3 paths in ONE invocation", got.Changed)
	}
	for _, c := range got.Changed {
		if c.Op != "write" {
			t.Fatalf("%s op = %q, want write", c.Path, c.Op)
		}
	}
	if got.Folder != b.Folder {
		t.Fatalf("batch folder = %q, want %q", got.Folder, b.Folder)
	}
	if got.Project == "" {
		t.Fatal("batch project is empty")
	}
	// >> appends, so a second invocation would show up as a longer file.
	raw, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, new(postSyncPayload)); err != nil {
		t.Fatalf("marker holds more than one batch: %q", raw)
	}
}

// TestPostSyncSilentWithoutConfig: no post_sync key, no new behavior.
func TestPostSyncSilentWithoutConfig(t *testing.T) {
	rem := sharedRemote(t)
	a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)

	postSync(t, b.Folder, "") // a project config, deliberately with no command

	// Only a spawned command could create this.
	marker := filepath.Join(t.TempDir(), "marker")

	write(t, a.Folder, "one.md", "1")
	cycle(t, a)
	res := cycle(t, b)
	if res.Materialized == 0 {
		t.Fatal("nothing materialized; test proves nothing")
	}
	if len(res.Inbound) != 1 {
		t.Fatalf("Result.Inbound = %v, want the one applied path", res.Inbound)
	}
	neverAppears(t, marker)
}

// TestPostSyncSkipsLocalOnlyCycle: scan + push is not an inbound event.
func TestPostSyncSkipsLocalOnlyCycle(t *testing.T) {
	rem := sharedRemote(t)
	b := newDevice(t, "devb", rem)

	marker := filepath.Join(t.TempDir(), "marker")
	postSync(t, b.Folder, "cat > "+marker)

	write(t, b.Folder, "mine.md", "local only")
	res := cycle(t, b)
	if res.LocalOps == 0 {
		t.Fatal("expected a local op; test proves nothing")
	}
	neverAppears(t, marker)
}

// TestPostSyncReportsDeletes: each entry distinguishes write from delete.
func TestPostSyncReportsDeletes(t *testing.T) {
	rem := sharedRemote(t)
	a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)

	write(t, a.Folder, "gone.md", "here")
	cycle(t, a)
	cycle(t, b) // b now holds it

	marker := filepath.Join(t.TempDir(), "marker.json")
	postSync(t, b.Folder, "cat > "+marker)

	if err := os.Remove(filepath.Join(a.Folder, "gone.md")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)

	got := batch(t, waitForFile(t, marker, 5*time.Second))
	if len(got.Changed) != 1 || got.Changed[0].Path != "gone.md" || got.Changed[0].Op != "delete" {
		t.Fatalf("batch = %+v, want one delete of gone.md", got.Changed)
	}
}

// TestPostSyncFailureNeverBreaksCycle: a hook that exits non-zero or does not
// exist leaves the cycle and the next one alone.
func TestPostSyncFailureNeverBreaksCycle(t *testing.T) {
	for _, cmd := range []string{"exit 3", "definitely-not-a-real-binary-xyz"} {
		rem := sharedRemote(t)
		a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)
		postSync(t, b.Folder, cmd)

		write(t, a.Folder, "one.md", "1")
		cycle(t, a)
		res, err := b.Cycle(t.Context())
		if err != nil {
			t.Fatalf("%q: Cycle returned %v", cmd, err)
		}
		if res.Materialized != 1 || len(res.Inbound) != 1 {
			t.Fatalf("%q: Result affected by the hook: %+v", cmd, res)
		}

		// The next cycle still converges.
		write(t, a.Folder, "two.md", "2")
		cycle(t, a)
		cycle(t, b)
		if read(t, b.Folder, "two.md") != "2" {
			t.Fatalf("%q: sync stopped converging after a failing hook", cmd)
		}
	}
}

// TestPostSyncLeavesInboundSpool is the guard on the trap: firing the hook
// must not consume what `bdrive sync --hook` reports to the agent.
func TestPostSyncLeavesInboundSpool(t *testing.T) {
	rem := sharedRemote(t)
	a, b := newDevice(t, "deva", rem), newDevice(t, "devb", rem)

	marker := filepath.Join(t.TempDir(), "marker.json")
	postSync(t, b.Folder, "cat > "+marker)

	write(t, a.Folder, "wiki/page.md", "hi")
	cycle(t, a)
	cycle(t, b)
	waitForFile(t, marker, 5*time.Second)

	evs, err := b.Store.DrainInbound()
	if err != nil {
		t.Fatal(err)
	}
	if len(evs) != 1 || evs[0].Path != "wiki/page.md" {
		t.Fatalf("inbound spool = %v, want wiki/page.md still queued for the agent hook", evs)
	}
}
