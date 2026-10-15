package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

func TestDaemon_CyclesConcurrentWorktreesAndSurvivesDeletedPrimary(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())

	root := t.TempDir()
	remoteDir := t.TempDir()
	remote := "file://" + remoteDir
	wt1 := filepath.Join(root, "wt1")
	wt2 := filepath.Join(root, "wt2")
	if err := os.MkdirAll(wt1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wt2, 0o755); err != nil {
		t.Fatal(err)
	}

	p1, err := config.SaveProject(wt1, config.Project{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(wt1); err != nil {
		t.Fatal(err)
	}

	// EnrollMount associates a second live worktree with the existing project
	// volume once that volume has been created. This is the same shared-store
	// relationship the daemon must preserve after either worktree disappears.
	vol1, err := config.VolumeDir(p1.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(vol1, 0o755); err != nil {
		t.Fatal(err)
	}

	p2, err := config.SaveProject(wt2, config.Project{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(wt2); err != nil {
		t.Fatal(err)
	}
	vol2, err := config.VolumeDir(p2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vol1 != vol2 {
		t.Fatalf("worktrees do not share a volume: wt1=%q wt2=%q", vol1, vol2)
	}

	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- Run(wt1, 20*time.Millisecond, 40*time.Millisecond)
		close(exited)
	}()
	t.Cleanup(func() {
		_ = os.RemoveAll(wt1)
		_ = os.RemoveAll(wt2)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
		}
	})

	writeWorktreeFile(t, wt1, "from-wt1.txt", "from wt1\n")
	worktreeWaitFor(t, "from-wt1.txt in wt2", done, func() bool {
		return worktreeFileEquals(wt2, "from-wt1.txt", "from wt1\n")
	})

	writeWorktreeFile(t, wt2, "from-wt2.txt", "from wt2\n")
	worktreeWaitFor(t, "from-wt2.txt in wt1", done, func() bool {
		return worktreeFileEquals(wt1, "from-wt2.txt", "from wt2\n")
	})

	if err := os.RemoveAll(filepath.Join(wt1, ".bdrive")); err != nil {
		t.Fatal(err)
	}
	p3, err := config.SaveProject(wt1, config.Project{Remote: remote})
	if err != nil {
		t.Fatal(err)
	}
	if p3.ID == p1.ID {
		t.Fatalf("re-initialization reused the old mount ID %q", p3.ID)
	}
	if _, _, err := config.EnrollMount(wt1); err != nil {
		t.Fatal(err)
	}
	vol3, err := config.VolumeDir(p3.ID)
	if err != nil {
		t.Fatal(err)
	}
	if vol3 != vol1 {
		t.Fatalf("re-initialized worktree changed volumes: old=%q new=%q", vol1, vol3)
	}
	writeWorktreeFile(t, wt1, "after-wt1-reinit.txt", "after wt1 reinit\n")
	worktreeWaitFor(t, "after-wt1-reinit.txt in wt2", done, func() bool {
		return worktreeFileEquals(wt2, "after-wt1-reinit.txt", "after wt1 reinit\n")
	})

	if err := os.RemoveAll(filepath.Join(wt1, ".bdrive")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt1); err != nil {
		t.Fatal(err)
	}
	writeWorktreeFile(t, wt2, "after-wt1-deleted.txt", "after wt1 deleted\n")
	worktreeWaitFor(t, "after-wt1-deleted.txt in the remote journal", done, func() bool {
		return worktreeRemoteHasOp(remoteDir, "after-wt1-deleted.txt", journal.KindPut)
	})

	if !worktreeFileEquals(wt2, "from-wt1.txt", "from wt1\n") {
		t.Fatal("deleting wt1 caused from-wt1.txt to disappear from wt2")
	}
	if worktreeRemoteHasOp(remoteDir, "from-wt1.txt", journal.KindDelete) {
		t.Fatal("deleting wt1 published a delete for from-wt1.txt to the remote")
	}

	if err := os.RemoveAll(filepath.Join(wt2, ".bdrive")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(wt2); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("daemon exited with error after both worktrees were deleted: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("daemon did not exit after the last worktree was deleted")
	}
}

func writeWorktreeFile(t *testing.T, folder, name, contents string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(folder, name), []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
}

func worktreeFileEquals(folder, name, want string) bool {
	data, err := os.ReadFile(filepath.Join(folder, name))
	return err == nil && string(data) == want
}

func worktreeWaitFor(t *testing.T, what string, done <-chan error, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("daemon exited while waiting for %s: %v", what, err)
		default:
		}
		if ok() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func worktreeRemoteHasOp(root, path, kind string) bool {
	entries, err := os.ReadDir(filepath.Join(root, "journal"))
	if err != nil {
		return false
	}
	for _, entry := range entries {
		data, err := os.ReadFile(filepath.Join(root, "journal", entry.Name()))
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(data), "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			var op journal.Op
			if err := json.Unmarshal([]byte(line), &op); err != nil {
				continue
			}
			if op.Path == path && op.Kind == kind {
				return true
			}
		}
	}
	return false
}
