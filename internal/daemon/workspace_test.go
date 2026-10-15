package daemon

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// wsMount builds a project under a workspace root, registered as `bdrive init`
// would leave it, with an isolated $BDRIVE_HOME and a file:// remote.
func wsMount(t *testing.T, root, name string) string {
	t.Helper()
	folder := filepath.Join(root, name)
	if err := os.MkdirAll(folder, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := config.SaveProject(folder, config.Project{Volume: name, Remote: "file://" + t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// wsRunDaemon starts the global loop and pauses every enrolled mount in the
// isolated home on cleanup, including projects discovered beside the primary.
func wsRunDaemon(t *testing.T, folder string) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- Run(folder, 25*time.Millisecond, 25*time.Millisecond) }()
	t.Cleanup(func() {
		mounts, err := config.LoadMounts()
		if err != nil {
			t.Errorf("load mounts during supervisor cleanup: %v", err)
		}
		for id := range mounts {
			volDir, err := config.VolumeDir(id)
			if err != nil {
				t.Errorf("resolve volume during supervisor cleanup: %v", err)
				continue
			}
			if err := store.SetPausedMount(volDir, id, true); err != nil {
				t.Errorf("pause mount during supervisor cleanup: %v", err)
			}
		}
		os.RemoveAll(filepath.Join(folder, ".bdrive"))
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("supervisor cleanup: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("supervisor did not exit during cleanup")
		}
	})
}

// wsWaitFor polls until ok() or the deadline.
func wsWaitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestWorkspaceRefreshOnDaemonStart
//
// The workspace manifest is re-indexed on every DAEMON start, not on `bdrive
// init` alone. `bdrive resume` — the reboot path, and the one that runs after
// a machine has been off while teammates worked — calls Start directly and
// never touches init's code, so a refresh that lived only there would leave
// the root's index frozen at whatever init last wrote.
//
// The refresh deliberately runs AFTER the daemon announces itself (the scan is
// unbounded, and holding up startup for a wedged sibling would fail a healthy
// project's daemon), so this drives the real loop rather than an early exit.
func TestWorkspaceRefreshOnDaemonStart(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	mount := wsMount(t, root, "team")
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	// A project that appeared under the root after the manifest was written —
	// another device's, or one created by hand. Only a daemon start notices.
	second := wsMount(t, root, "docs")
	if w, _, err := config.LoadWorkspace(root); err != nil || len(w.Projects) != 1 {
		t.Fatalf("manifest before the daemon = %+v (%v), want just the first project", w.Projects, err)
	}

	wsRunDaemon(t, mount)

	wsWaitFor(t, "the daemon to re-index the workspace root", func() bool {
		w, ok, err := config.LoadWorkspace(root)
		return err == nil && ok && len(w.Projects) == 2
	})

	w, _, err := config.LoadWorkspace(root)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, p := range w.Projects {
		if p.ID == "" {
			t.Fatalf("entry %+v has no mount id", p)
		}
		got[p.Path] = true
	}
	if !got["team"] || !got["docs"] {
		t.Fatalf("manifest = %+v, want both %s and %s", w.Projects, filepath.Base(mount), filepath.Base(second))
	}
}

// TestWorkspaceRefreshNeverCreatesARoot: a project with no root above it must
// not gain one because a daemon started — that would turn an ordinary parent
// directory (often the user's home) into a workspace root behind their back.
func TestWorkspaceRefreshNeverCreatesARoot(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	parent := t.TempDir()
	mount := wsMount(t, parent, "wiki")

	wsRunDaemon(t, mount)

	// Give the daemon the same window the refresh above needed, then check
	// nothing appeared.
	wsWaitFor(t, "the daemon to come up", func() bool {
		_, running := Running(mustVolDir(t, mount))
		return running
	})
	time.Sleep(200 * time.Millisecond)

	if _, err := os.Stat(filepath.Join(parent, config.ProjectDir)); !os.IsNotExist(err) {
		t.Fatal("a daemon start wrote settings into the project's parent")
	}
	if config.IsWorkspaceRoot(parent) {
		t.Fatal("a daemon start turned the project's parent into a workspace root")
	}
}

func mustVolDir(t *testing.T, folder string) string {
	t.Helper()
	proj, ok, err := config.LoadProject(folder)
	if err != nil || !ok {
		t.Fatalf("LoadProject(%s): %v (ok=%v)", folder, err, ok)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	return vdir
}

// TestWorkspaceRefreshNeverStallsTheDaemon
//
// The manifest scan reads a directory the user chose plus one config per
// child, and neither is bounded — a FIFO, a device node, or the realistic
// case, a TCC-gated or wedged network path, blocks the read forever.
//
// Inline, that takes the daemon with it. Before `announce` it fails a healthy
// project's start; after `announce` it is worse, because the flock (which IS
// liveness in this product) says "running" while the sync loop never begins —
// `bdrive status` reports a daemon that does nothing, forever. So the refresh
// runs in its own goroutine and the loop never waits on it.
func TestWorkspaceRefreshNeverStallsTheDaemon(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	root := t.TempDir()
	mount := wsMount(t, root, "team")
	if err := config.InitWorkspace(root); err != nil {
		t.Fatal(err)
	}

	// A sibling whose config blocks any reader: opening a FIFO for reading
	// waits for a writer that never comes.
	decoy := filepath.Join(root, "wedged")
	if err := os.MkdirAll(filepath.Join(decoy, config.ProjectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	fifo := filepath.Join(decoy, config.ProjectDir, "config.json")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Skipf("cannot create a FIFO here: %v", err)
	}

	if err := os.WriteFile(filepath.Join(mount, "note.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	wsRunDaemon(t, mount)

	// The daemon must come up AND do its work. Journaling the file proves the
	// loop is running, not merely that the process is alive.
	wsWaitFor(t, "the daemon to journal a file with a wedged sibling beside it", func() bool {
		st, err := store.Open(mustVolDir(t, mount))
		if err != nil {
			return false
		}
		ops, err := st.AllOps()
		return err == nil && len(ops) > 0
	})
}
