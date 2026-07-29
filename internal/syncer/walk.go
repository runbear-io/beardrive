package syncer

import (
	"io/fs"
	"path/filepath"

	"github.com/runbear-io/beardrive/internal/config"
)

// verdict is what the sync predicate decided about one entry on disk.
type verdict int

const (
	vDescend  verdict = iota // directory, walk into it
	vPruneDir                // directory excluded whole (.git, .bdrive, PruneDir)
	vNested                  // directory is a mount of its own (syncs separately)
	vSkipFile                // file excluded (non-regular, .DS_Store/.bdrive-tmp-*, Skip)
	vSync                    // file syncs
)

// walkFolder walks a mount applying the exact predicate the sync cycle uses.
// fn sees every entry with its verdict; pruning happens here, so no caller can
// descend where scan would not. This is the only copy of the rules: scan and
// Explain both go through it, so what `bdrive scope --explain` reports cannot
// drift from what actually leaves the machine.
func walkFolder(folder string, filter *Filter, fn func(abs, rel string, d fs.DirEntry, v verdict) error) error {
	return filepath.WalkDir(folder, func(p string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return nil // skip unreadable entries
		}
		rel, err := filepath.Rel(folder, p)
		if err != nil || rel == "." {
			return nil
		}
		rel = filepath.ToSlash(rel)

		var v verdict
		switch {
		case !d.IsDir():
			if !d.Type().IsRegular() || ignoredFile(d.Name()) || filter.Skip(rel) {
				v = vSkipFile
			} else {
				v = vSync
			}
		case ignoreDirs[d.Name()] || filter.PruneDir(rel):
			v = vPruneDir
		case config.IsMount(p):
			// A mount of its own: it syncs through its own project.
			filter.addNestedMount(rel)
			v = vNested
		default:
			v = vDescend
		}

		if err := fn(p, rel, d, v); err != nil {
			return err
		}
		if v == vPruneDir || v == vNested {
			return fs.SkipDir
		}
		return nil
	})
}
