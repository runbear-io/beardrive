package store

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestSymlink_OpenRegularUnderRootReadsRegularFile(t *testing.T) {
	root := t.TempDir()
	abs := filepath.Join(root, "entry")
	if err := os.WriteFile(abs, []byte("regular bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, fi, err := OpenRegularUnderRoot(root, abs)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if !fi.Mode().IsRegular() || fi.Size() != int64(len("regular bytes")) {
		t.Fatalf("opened metadata = %v", fi)
	}
	data, err := io.ReadAll(f)
	if err != nil || string(data) != "regular bytes" {
		t.Fatalf("read = %q, %v", data, err)
	}
}

func TestSymlink_OpenRegularUnderRootRejectsNonRegularEntries(t *testing.T) {
	for _, kind := range []string{"live link", "dangling link", "directory", "fifo", "socket"} {
		t.Run(kind, func(t *testing.T) {
			var root string
			if kind == "socket" {
				// Darwin's sun_path limit is shorter than CI's long TMPDIR.
				var err error
				root, err = os.MkdirTemp("/tmp", "bdsock")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := os.RemoveAll(root); err != nil {
						t.Errorf("remove short socket directory: %v", err)
					}
				})
			} else {
				root = t.TempDir()
			}
			abs := filepath.Join(root, "entry")
			switch kind {
			case "live link", "dangling link":
				target := filepath.Join(t.TempDir(), "outside")
				if kind == "live link" {
					if err := os.WriteFile(target, []byte("OUTSIDE_SECRET_NEEDLE"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
				if err := os.Symlink(target, abs); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(abs, 0o700); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(abs, 0o600); err != nil {
					t.Fatal(err)
				}
			case "socket":
				abs = filepath.Join(root, "s")
				listener, err := net.Listen("unix", abs)
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
			}
			done := make(chan error, 1)
			go func() {
				f, _, err := OpenRegularUnderRoot(root, abs)
				if f != nil {
					f.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if !errors.Is(err, ErrNotRegular) {
					t.Fatalf("open %s = %v, want ErrNotRegular", kind, err)
				}
			case <-time.After(5 * time.Second):
				if kind == "fifo" {
					if writer, err := os.OpenFile(abs, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
						writer.Close()
						select {
						case <-done:
						case <-time.After(5 * time.Second):
						}
					}
				}
				t.Fatal("regular-file open blocked on a non-regular entry")
			}
		})
	}
}

func TestSymlink_OpenRegularUnderRootRejectsEscapingAncestor(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "entry"), []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegularUnderRoot(root, filepath.Join(root, "escape", "entry"))
	if f != nil {
		f.Close()
	}
	if err == nil {
		t.Fatal("opened a regular file through an escaping ancestor")
	}
}

func TestSymlink_OpenRegularUnderRootPreservesPermissionErrors(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 regular files")
	}
	root := t.TempDir()
	abs := filepath.Join(root, "entry")
	if err := os.WriteFile(abs, []byte("regular bytes"), 0); err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegularUnderRoot(root, abs)
	if f != nil {
		f.Close()
	}
	if !errors.Is(err, os.ErrPermission) || errors.Is(err, ErrNotRegular) {
		t.Fatalf("regular-file permission error = %v", err)
	}
}

func TestSymlink_OpenRegularUnderRootPinsParentsDuringSwaps(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
	sub := filepath.Join(root, "sub")
	parked := filepath.Join(root, "parked")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "note.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outside, "note.txt"), []byte("OUTSIDE_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	started := make(chan struct{})
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		for {
			select {
			case <-stop:
				done <- nil
				return
			default:
			}
			if err := os.Rename(sub, parked); err != nil {
				done <- err
				return
			}
			if err := os.Symlink(outside, sub); err != nil {
				done <- err
				return
			}
			if err := os.Remove(sub); err != nil {
				done <- err
				return
			}
			if err := os.Rename(parked, sub); err != nil {
				done <- err
				return
			}
		}
	}()
	<-started
	// Stop only between complete swaps, leaving the real directory in place.
	defer func() {
		close(stop)
		if err := <-done; err != nil {
			t.Errorf("swap directory: %v", err)
		}
	}()
	for range 500 {
		f, _, err := OpenRegularUnderRoot(root, filepath.Join(root, "sub", "note.txt"))
		if err != nil {
			continue
		} // a missing or symlinked parent is an ordinary refusal
		data, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read pinned file: %v, close: %v", readErr, closeErr)
		}
		if string(data) != "inside" {
			t.Fatalf("parent swap exposed %q", data)
		}
	}
}

func TestSymlink_OpenRegularUnderRootAllowsSymlinkedMountRoot(t *testing.T) {
	realMount := t.TempDir()
	mountLink := filepath.Join(t.TempDir(), "mount")
	if err := os.WriteFile(filepath.Join(realMount, "note.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realMount, mountLink); err != nil {
		t.Fatal(err)
	}
	for _, abs := range []string{filepath.Join(mountLink, "note.txt"), filepath.Join(realMount, "note.txt")} {
		f, _, err := OpenRegularUnderRoot(mountLink, abs)
		if err != nil {
			t.Fatalf("open legitimate mount alias %s: %v", abs, err)
		}
		data, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || string(data) != "inside" {
			t.Fatalf("read mount alias = %q, %v; close: %v", data, readErr, closeErr)
		}
	}
}

func TestSymlink_OpenRegularUnderRootRejectsIntermediateLinkInsideMount(t *testing.T) {
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(realDir, "note.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegularUnderRoot(root, filepath.Join(root, "alias", "note.txt"))
	if f != nil {
		f.Close()
	}
	if err == nil || errors.Is(err, ErrNotRegular) {
		t.Fatalf("intermediate symlink = %v, want a parent-walk error", err)
	}
}

func TestSymlink_OpenRegularUnderRootAllowsAliasedMountAncestors(t *testing.T) {
	physical := t.TempDir()
	tempAlias := filepath.Join(t.TempDir(), "tmp")
	if err := os.Symlink(physical, tempAlias); err != nil {
		t.Fatal(err)
	}
	mount := filepath.Join(tempAlias, "project")
	if err := os.MkdirAll(filepath.Join(mount, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"note.txt", "sub/note.txt"} {
		if err := os.WriteFile(filepath.Join(mount, rel), []byte("inside"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	mountAlias := filepath.Join(t.TempDir(), "mount")
	if err := os.Symlink(mount, mountAlias); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"note.txt", "sub/note.txt"} {
		f, _, err := OpenRegularUnderRoot(mountAlias, filepath.Join(mount, rel))
		if err != nil {
			t.Fatalf("open through aliased mount ancestor: %v", err)
		}
		data, readErr := io.ReadAll(f)
		closeErr := f.Close()
		if readErr != nil || closeErr != nil || string(data) != "inside" {
			t.Fatalf("read aliased mount ancestor = %q, %v; close: %v", data, readErr, closeErr)
		}
	}
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "note.txt"), []byte("OUTSIDE_SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mount, "escape")); err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegularUnderRoot(mountAlias, filepath.Join(mount, "escape", "note.txt"))
	if f != nil {
		f.Close()
	}
	if err == nil || errors.Is(err, ErrNotRegular) {
		t.Fatalf("aliased root allowed a descendant symlink: %v", err)
	}
}

func TestSymlink_OpenRegularUnderRootAllowsRelativeMountAlias(t *testing.T) {
	realMount := t.TempDir()
	mountAlias := filepath.Join(t.TempDir(), "mount")
	if err := os.WriteFile(filepath.Join(realMount, "note.txt"), []byte("inside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(realMount, mountAlias); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	relativeAlias, err := filepath.Rel(cwd, mountAlias)
	if err != nil {
		t.Fatal(err)
	}
	f, _, err := OpenRegularUnderRoot(relativeAlias, filepath.Join(realMount, "note.txt"))
	if err != nil {
		t.Fatalf("open relative mount alias: %v", err)
	}
	data, readErr := io.ReadAll(f)
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || string(data) != "inside" {
		t.Fatalf("read relative mount alias = %q, %v; close: %v", data, readErr, closeErr)
	}
}
