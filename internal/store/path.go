package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// UnderRoot reports whether p resolves inside root ON DISK — following every
// symlink on the way, including one planted in a directory that already
// exists. p need not exist: the deepest existing ancestor is what gets
// resolved, so the answer is available before anything is created.
//
// A lexical check (filepath.Rel, path.Clean, HasPrefix) answers a question
// about the STRING. os.MkdirAll, os.CreateTemp, os.Rename and os.Open all
// answer the question about the FILESYSTEM, and that is the one that decides
// where the bytes land: "docs/x.md" is a clean relative path by every lexical
// test and lands outside the root the moment "docs" is a symlink. It lives in
// this package because the two callers that need it — the syncer materializing
// a peer's op into a mount, and the file:// backend resolving a key under the
// hub's storage root — are the same question about different roots.
func UnderRoot(root, p string) bool {
	rootReal, err := filepath.EvalSymlinks(root)
	if err != nil {
		return false
	}
	for dir := p; ; {
		if real, err := filepath.EvalSymlinks(dir); err == nil {
			rel, err := filepath.Rel(rootReal, real)
			return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
		}
		// A component that EXISTS but does not resolve is a dangling symlink,
		// and stepping past it answered "inside the root" for a path that
		// lands wherever the link points the moment anything creates the
		// target. The guard must answer about where the bytes would go.
		if fi, err := os.Lstat(dir); err == nil && fi.Mode()&os.ModeSymlink != 0 {
			return false
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return false // walked off the top without finding anything that exists
		}
		dir = parent
	}
}

// ErrNotRegular marks an entry that must not be read as file content.
var ErrNotRegular = errors.New("not a regular file")

// OpenRegularUnderRoot opens a regular file without following its final entry
// or blocking on a FIFO. The returned metadata describes the opened descriptor,
// not a path that may have changed between inspection and opening.
func OpenRegularUnderRoot(root, abs string) (*os.File, os.FileInfo, error) {
	if !UnderRoot(root, filepath.Dir(abs)) {
		return nil, nil, fmt.Errorf("%s resolves outside the mount root", abs)
	}
	f, err := os.OpenFile(abs, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, syscall.ELOOP) || errors.Is(err, syscall.EMLINK) {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, abs)
		}
		// Some non-regular entries (for example sockets) cannot be opened
		// at all. Keep that classification distinct from a regular file's
		// permission or I/O error.
		if fi, statErr := os.Lstat(abs); statErr == nil && !fi.Mode().IsRegular() {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, abs)
		}
		return nil, nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if !fi.Mode().IsRegular() {
		f.Close()
		return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, abs)
	}
	return f, fi, nil
}
