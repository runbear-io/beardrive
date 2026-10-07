package webapp

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DirSource serves a plain local folder straight from disk — no bdrive remote
// or volume needed. Meant for debugging the webapp (and as a quick local
// markdown browser): the tree reflects the folder live, provenance is just
// file mtimes, and content streams from the filesystem.
type DirSource struct {
	Root string
}

var skipNames = map[string]bool{".DS_Store": true, ".bdrive": true}
var skipDirs = map[string]bool{".git": true, ".bdrive": true}

func (d *DirSource) Files(_ context.Context) (map[string]FileInfo, error) {
	files := make(map[string]FileInfo)
	err := filepath.WalkDir(d.Root, func(p string, e fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		rel, err := filepath.Rel(d.Root, p)
		if err != nil || rel == "." {
			return nil
		}
		if e.IsDir() {
			if skipDirs[e.Name()] {
				return fs.SkipDir
			}
			return nil
		}
		if skipNames[e.Name()] || strings.HasPrefix(e.Name(), ".bdrive-tmp-") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
			return nil
		}
		if e.Type()&fs.ModeSymlink != 0 {
			// A symlink is reported as its target, never resolved: Readlink,
			// not Open. A link pointing outside the served folder shows up in
			// the tree but serves no content.
			target, err := os.Readlink(p)
			if err != nil {
				return nil
			}
			files[filepath.ToSlash(rel)] = FileInfo{
				Blob: fmt.Sprintf("link-%d-%s", info.ModTime().UnixNano(), target),
				Time: info.ModTime().UTC(),
				Link: target,
			}
			return nil
		}
		if !e.Type().IsRegular() {
			return nil
		}
		files[filepath.ToSlash(rel)] = FileInfo{
			// Synthetic content identity for the ETag; changes when the
			// file does, which is all revalidation needs.
			Blob: fmt.Sprintf("dir-%d-%d", info.ModTime().UnixNano(), info.Size()),
			Size: info.Size(),
			Time: info.ModTime().UTC(),
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

// Open streams a file from disk. Paths are only ever snapshot map keys
// (produced by Files above), so they cannot escape Root.
//
// A symlink is never opened: os.Open follows it, and a link inside the folder
// pointing at ~/.ssh/id_rsa would then serve the key to anyone who can see the
// tree. The listing marks a link (fi.Link) and Lstat re-checks the disk, since
// a link placed after the last listing has no FileInfo saying so. This is the
// one door every reader goes through — viewer, download, render, shares, MCP,
// the co-editing room — so the refusal lives here and nowhere else.
func (d *DirSource) Open(_ context.Context, path string, fi FileInfo) (io.ReadCloser, error) {
	if fi.Link != "" {
		return nil, errLinkHasNoContent
	}
	abs := filepath.Join(d.Root, filepath.FromSlash(path))
	if st, err := os.Lstat(abs); err == nil && st.Mode()&fs.ModeSymlink != 0 {
		return nil, errLinkHasNoContent
	}
	return os.Open(abs)
}

// errLinkHasNoContent is what opening a symlink answers, on every Source: a
// link is its target string, shown in the tree, and has no bytes of its own.
var errLinkHasNoContent = fmt.Errorf("a symbolic link has no content of its own")
