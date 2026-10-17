//go:build darwin

package daemon

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/fsnotify/fsnotify"
)

const darwinFixedFDAllowancePerCycle = 4

func TestFsnotifyDarwinCloseReleasesFileDescriptors(t *testing.T) {
	root := t.TempDir()
	for i := range 16 {
		name := filepath.Join(root, fmt.Sprintf("file-%02d", i))
		if err := os.WriteFile(name, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	baseline := darwinOpenFDCount(t)
	for range 8 {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			t.Fatal(err)
		}
		if err := watcher.Add(root); err != nil {
			_ = watcher.Close()
			t.Fatal(err)
		}
		if err := watcher.Close(); err != nil {
			t.Fatal(err)
		}
	}

	got := darwinOpenFDCount(t)
	if got > baseline+8*darwinFixedFDAllowancePerCycle {
		t.Fatalf("watcher close leaked file descriptors: baseline=%d after=%d", baseline, got)
	}
}

func darwinOpenFDCount(t *testing.T) int {
	t.Helper()
	dir, err := os.Open("/dev/fd")
	if err != nil {
		t.Fatalf("open /dev/fd: %v", err)
	}
	defer dir.Close()

	entries, err := dir.Readdirnames(-1)
	if err != nil && err != io.EOF {
		t.Fatalf("read /dev/fd: %v", err)
	}
	return len(entries)
}
