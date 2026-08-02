package store

import (
	"os"
	"path/filepath"
	"strings"
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
