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
			root := t.TempDir()
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
