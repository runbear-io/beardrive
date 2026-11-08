//go:build linux

package daemon

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"golang.org/x/sys/unix"
)

func newFanotifyTestWatcher(t *testing.T, roots ...string) rawWatcher {
	t.Helper()
	watcher, err := newFanotifyRawWatcher(roots)
	if err != nil {
		if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) || errors.Is(err, unix.ENOSYS) || errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENODEV) || errors.Is(err, unix.EINVAL) {
			t.Skipf("fanotify unavailable: %v", err)
		}
		t.Fatalf("newFanotifyRawWatcher: %v", err)
	}
	t.Cleanup(func() { _ = watcher.Close() })
	return watcher
}

func waitFanotifyEvent(t *testing.T, watcher rawWatcher, name string, op fsnotify.Op) fsnotify.Event {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event, ok := <-watcher.Events():
			if !ok {
				t.Fatal("fanotify events channel closed")
			}
			if event.Name == name && event.Op&op != 0 {
				return event
			}
		case err, ok := <-watcher.Errors():
			if ok && err != nil && !errors.Is(err, fsnotify.ErrEventOverflow) {
				t.Fatalf("fanotify watcher error: %v", err)
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for fanotify event name=%q op=%s", name, op)
		}
	}
}

func TestFanotifyRawFileLifecycle(t *testing.T) {
	root := t.TempDir()
	watcher := newFanotifyTestWatcher(t, root)

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, file, fsnotify.Create)
	if err := os.WriteFile(file, []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, file, fsnotify.Write)
	if err := os.Chmod(file, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, file, fsnotify.Chmod)

	rename := filepath.Join(root, "renamed.txt")
	if err := os.Rename(file, rename); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, file, fsnotify.Rename)
	if err := os.Remove(rename); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, rename, fsnotify.Remove)
}

func TestFanotifyRawDirectoryRenameKeepsReaderAlive(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "dirA")
	if err := os.MkdirAll(filepath.Join(dirA, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	watcher := newFanotifyTestWatcher(t, root)

	dirB := filepath.Join(root, "dirB")
	if err := os.Rename(dirA, dirB); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dirB, "sub", "after-move"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, dirA, fsnotify.Rename)

	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("still alive"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, sentinel, fsnotify.Create)
}

func TestFanotifyRawDeleteAndRecreateDirectory(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "dirA")
	if err := os.MkdirAll(filepath.Join(dirA, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	watcher := newFanotifyTestWatcher(t, root)

	if err := os.RemoveAll(dirA); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dirA, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, dirA, fsnotify.Remove)
	waitFanotifyEvent(t, watcher, dirA, fsnotify.Create)
}

func TestFanotifyRawMovedDirectoryOutsideRoot(t *testing.T) {
	root := t.TempDir()
	dirA := filepath.Join(root, "dirA")
	if err := os.MkdirAll(filepath.Join(dirA, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	watcher := newFanotifyTestWatcher(t, root)

	outside := filepath.Join(t.TempDir(), "moved")
	if err := os.Rename(dirA, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "sub", "after-move"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, dirA, fsnotify.Rename)

	sentinel := filepath.Join(root, "sentinel")
	if err := os.WriteFile(sentinel, []byte("still alive"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFanotifyEvent(t, watcher, sentinel, fsnotify.Create)
}

func TestFanotifyRawOpenCloseDoesNotLeakFileDescriptors(t *testing.T) {
	root := t.TempDir()
	baseline := fanotifyOpenFDCount(t)
	for range 20 {
		watcher := newFanotifyTestWatcher(t, root)
		if err := watcher.Close(); err != nil {
			t.Fatal(err)
		}
	}
	got := fanotifyOpenFDCount(t)
	if got > baseline+1 {
		t.Fatalf("fanotify watcher leaked file descriptors: baseline=%d after=%d", baseline, got)
	}
}

func fanotifyOpenFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatal(fmt.Errorf("read /proc/self/fd: %w", err))
	}
	return len(entries)
}
