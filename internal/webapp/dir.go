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

var skipNames = map[string]bool{".DS_Store": true, ".beardrive": true}
var skipDirs = map[string]bool{".git": true, ".beardrive": true}

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
		if !e.Type().IsRegular() || skipNames[e.Name()] || strings.HasPrefix(e.Name(), ".beardrive-tmp-") {
			return nil
		}
		info, err := e.Info()
		if err != nil {
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
func (d *DirSource) Open(_ context.Context, path string, _ FileInfo) (io.ReadCloser, error) {
	return os.Open(filepath.Join(d.Root, filepath.FromSlash(path)))
}
