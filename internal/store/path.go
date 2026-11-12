package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
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

// OpenRegularUnderRoot pins the mount root and walks its descendants through
// directory descriptors. No descendant component may be a symlink, and the leaf
// is opened nonblocking and checked as a regular file before it can be read.
// The caller owns the returned file.
func OpenRegularUnderRoot(root, abs string) (*os.File, os.FileInfo, error) {
	root, abs = filepath.Clean(root), filepath.Clean(abs)
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return nil, nil, err
	}
	separator := string(filepath.Separator)
	if rel == ".." || strings.HasPrefix(rel, ".."+separator) {
		// Resolve only the trusted mount root, never the candidate path.
		// This permits aliases such as /tmp and /private/tmp.
		realRoot, err := filepath.EvalSymlinks(root)
		if err != nil {
			return nil, nil, err
		}
		rel, err = filepath.Rel(realRoot, abs)
		if err != nil {
			return nil, nil, err
		}
	}
	if rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+separator) || filepath.IsAbs(rel) {
		return nil, nil, fmt.Errorf("%s is not a descendant of the mount root", abs)
	}
	dirFD, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, &os.PathError{Op: "open", Path: root, Err: err}
	}
	leaf := rel
	for {
		segment, rest, parent := strings.Cut(leaf, separator)
		if segment == "" || segment == "." || segment == ".." {
			unix.Close(dirFD)
			return nil, nil, fmt.Errorf("%s has an invalid path segment", abs)
		}
		if !parent {
			break
		}
		nextFD, err := unix.Openat(dirFD, segment, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(dirFD)
		if err != nil {
			return nil, nil, &os.PathError{Op: "openat", Path: abs, Err: err}
		}
		dirFD = nextFD
		leaf = rest
	}
	fd, err := unix.Openat(dirFD, leaf, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		nonRegular := errors.Is(err, unix.ELOOP) || errors.Is(err, unix.EMLINK)
		if !nonRegular {
			var st unix.Stat_t
			nonRegular = unix.Fstatat(dirFD, leaf, &st, unix.AT_SYMLINK_NOFOLLOW) == nil &&
				st.Mode&unix.S_IFMT != unix.S_IFREG
		}
		unix.Close(dirFD)
		if nonRegular {
			return nil, nil, fmt.Errorf("%w: %s", ErrNotRegular, abs)
		}
		return nil, nil, &os.PathError{Op: "openat", Path: abs, Err: err}
	}
	unix.Close(dirFD)
	f := os.NewFile(uintptr(fd), abs)
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
