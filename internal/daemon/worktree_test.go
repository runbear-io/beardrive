package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
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

type supervisorFixture struct {
	folder  string
	remote  string
	volDir  string
	mountID string
}

func supervisorMount(t *testing.T) supervisorFixture {
	t.Helper()
	folder, remoteDir := t.TempDir(), t.TempDir()
	proj, err := config.SaveProject(folder, config.Project{Remote: "file://" + remoteDir})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	volDir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(volDir, 0o755); err != nil {
		t.Fatal(err)
	}
	return supervisorFixture{folder: folder, remote: remoteDir, volDir: volDir, mountID: proj.ID}
}

func supervisorRun(t *testing.T, mounts ...supervisorFixture) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	exited := make(chan struct{})
	go func() {
		done <- Run("", 20*time.Millisecond, 40*time.Millisecond)
		close(exited)
	}()
	t.Cleanup(func() {
		for _, mount := range mounts {
			if err := store.SetPausedMount(mount.volDir, mount.mountID, true); err != nil {
				t.Errorf("pause %s during cleanup: %v", mount.mountID, err)
			}
		}
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not exit during cleanup")
		}
	})
	return done
}

func TestSupervisor_ManagesMultipleProjectsAndIsolatesStop(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	projA, projB := supervisorMount(t), supervisorMount(t)
	if projA.volDir == projB.volDir {
		t.Fatal("distinct projects unexpectedly share a volume")
	}
	done := supervisorRun(t, projA, projB)
	worktreeWaitFor(t, "both supervisor workers", done, func() bool {
		pidA, a := Running(projA.volDir)
		pidB, b := Running(projB.volDir)
		pidGlobal, global := GlobalRunning()
		return a && b && global && pidA == os.Getpid() && pidB == pidA && pidGlobal == pidA
	})
	writeWorktreeFile(t, projA.folder, "a.txt", "project A\n")
	writeWorktreeFile(t, projB.folder, "b.txt", "project B\n")
	worktreeWaitFor(t, "both projects syncing", done, func() bool {
		return worktreeRemoteHasOp(projA.remote, "a.txt", journal.KindPut) &&
			worktreeRemoteHasOp(projB.remote, "b.txt", journal.KindPut)
	})
	if stopped, err := Stop(projB.volDir); err != nil || !stopped {
		t.Fatalf("Stop(project B) = (%v, %v)", stopped, err)
	}
	if _, running := Running(projB.volDir); running {
		t.Fatal("paused project B still owns its volume lock")
	}
	if pid, running := Running(projA.volDir); !running || pid != os.Getpid() {
		t.Fatalf("stopping B interrupted A: pid=%d running=%v", pid, running)
	}
	writeWorktreeFile(t, projA.folder, "a-after-stop.txt", "A continues\n")
	writeWorktreeFile(t, projB.folder, "b-after-stop.txt", "B stays paused\n")
	worktreeWaitFor(t, "A syncing after B stops", done, func() bool {
		return worktreeRemoteHasOp(projA.remote, "a-after-stop.txt", journal.KindPut)
	})
	if worktreeRemoteHasOp(projB.remote, "b-after-stop.txt", journal.KindPut) {
		t.Fatal("stopped project B kept syncing")
	}
	if err := store.SetPausedMount(projA.volDir, projA.mountID, true); err != nil {
		t.Fatal(err)
	}
	WakeSupervisor()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("supervisor exited with error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not exit after its last project paused")
	}
	if _, running := GlobalRunning(); running {
		t.Fatal("supervisor left its global lock held")
	}
}

func TestSupervisor_WorkerPanicRecoveryIsolatesOtherVolumes(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	projA, projB := supervisorMount(t), supervisorMount(t)
	var panicA atomic.Bool
	panicA.Store(true)
	workerCycleHook = func(volDir string) {
		if volDir == projA.volDir && panicA.Load() {
			panic("injected project A failure")
		}
	}
	// Registered before supervisorRun's cleanup so workers exit before the
	// shared test seam is cleared, avoiding a concurrent function-pointer write.
	t.Cleanup(func() { workerCycleHook = nil })
	done := supervisorRun(t, projA, projB)
	worktreeWaitFor(t, "project A panic logged with stack", done, func() bool {
		data, err := os.ReadFile(LogPath(projA.volDir))
		return err == nil && strings.Contains(string(data), "cycle panic recovered: injected project A failure") &&
			strings.Contains(string(data), "runtime/debug.Stack")
	})
	writeWorktreeFile(t, projB.folder, "b-during-panic.txt", "B is unaffected\n")
	worktreeWaitFor(t, "project B syncing while A panics", done, func() bool {
		return worktreeRemoteHasOp(projB.remote, "b-during-panic.txt", journal.KindPut)
	})
	if pid, running := Running(projA.volDir); !running || pid != os.Getpid() {
		t.Fatalf("panic dropped A's worker lock: pid=%d running=%v", pid, running)
	}
	panicA.Store(false)
	writeWorktreeFile(t, projA.folder, "a-after-panic.txt", "A recovered\n")
	worktreeWaitFor(t, "project A retrying successfully after panic", done, func() bool {
		return worktreeRemoteHasOp(projA.remote, "a-after-panic.txt", journal.KindPut)
	})
}
