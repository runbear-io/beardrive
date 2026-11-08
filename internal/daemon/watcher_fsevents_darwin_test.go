//go:build darwin && (arm64 || amd64)

package daemon

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func TestFSEventsRawWatcherFileOperations(t *testing.T) {
	root := t.TempDir()
	w, err := newFSEventsRawWatcher([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	file := filepath.Join(root, "file.txt")
	if err := os.WriteFile(file, []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == file && ev.Op&(fsnotify.Create|fsnotify.Write) != 0
	})

	fd, err := os.OpenFile(file, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fd.WriteAt([]byte("two"), 0); err != nil {
		_ = fd.Close()
		t.Fatal(err)
	}
	if err := fd.Close(); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == file && ev.Op&fsnotify.Write != 0
	})

	renamed := filepath.Join(root, "renamed.txt")
	if err := os.Rename(file, renamed); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == renamed && ev.Op&fsnotify.Rename != 0
	})

	if err := os.Chmod(renamed, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == renamed && ev.Op&fsnotify.Chmod != 0
	})

	if err := os.Remove(renamed); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == renamed && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0
	})

	dir := filepath.Join(root, "subdir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == dir && ev.Op&(fsnotify.Create|fsnotify.Write) != 0
	})
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == dir && ev.Op&(fsnotify.Remove|fsnotify.Rename) != 0
	})
}

func TestFSEventsRawWatcherSymlinkedRoot(t *testing.T) {
	target := t.TempDir()
	parent := t.TempDir()
	root := filepath.Join(parent, "mount")
	if err := os.Symlink(target, root); err != nil {
		t.Fatal(err)
	}
	w, err := newFSEventsRawWatcher([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	name := filepath.Join(root, "through-link.txt")
	if err := os.WriteFile(name, []byte("link"), 0o644); err != nil {
		t.Fatal(err)
	}
	waitFSEvent(t, w.Events(), func(ev fsnotify.Event) bool {
		return ev.Name == name && ev.Op&(fsnotify.Create|fsnotify.Write) != 0
	})
}

func TestStartLocalFolderWatcherUsesFSEvents(t *testing.T) {
	setLocalWatchBackendForTest(t, "fsevents")
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lw, err := startLocalFolderWatcher(ctx, []string{root})
	if err != nil {
		t.Fatal(err)
	}
	defer lw.close()

	name := filepath.Join(root, "watch.txt")
	if err := os.WriteFile(name, []byte("watch"), 0o644); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lw.wake:
	case <-time.After(5 * time.Second):
		t.Fatal("local FSEvents watcher did not wake")
	}
	batch := lw.take(root)
	for _, ev := range batch.events {
		if ev.path == name {
			return
		}
	}
	t.Fatalf("FSEvents batch did not contain %q: %+v", name, batch.events)
}

func TestFSEventsRawWatcherFileDescriptorUsage(t *testing.T) {
	root := t.TempDir()
	for i := range 200 {
		dir := filepath.Join(root, "dir", formatFSEventsIndex(i))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for i := range 500 {
		dir := filepath.Join(root, "dir", formatFSEventsIndex(i%200))
		if err := os.WriteFile(filepath.Join(dir, "file-"+formatFSEventsIndex(i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	warm, err := newFSEventsRawWatcher([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	if err := warm.Close(); err != nil {
		t.Fatal(err)
	}
	baseline := darwinOpenFDCount(t)
	w, err := newFSEventsRawWatcher([]string{root})
	if err != nil {
		t.Fatal(err)
	}
	opened := darwinOpenFDCount(t)
	if opened > baseline+16 {
		_ = w.Close()
		t.Fatalf("FSEvents opened too many file descriptors: baseline=%d opened=%d", baseline, opened)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	for range 10 {
		w, err := newFSEventsRawWatcher([]string{root})
		if err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	}
	if after := darwinOpenFDCount(t); after > baseline+4 {
		t.Fatalf("FSEvents leaked file descriptors: baseline=%d after=%d", baseline, after)
	}
}

func waitFSEvent(t *testing.T, events <-chan fsnotify.Event, match func(fsnotify.Event) bool) fsnotify.Event {
	t.Helper()
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatal("FSEvents event channel closed")
			}
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("timed out waiting for FSEvents event")
		}
	}
}

func formatFSEventsIndex(i int) string {
	return fmt.Sprintf("%03d", i)
}
