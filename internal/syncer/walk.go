package syncer

import (
	"io/fs"
	"path/filepath"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
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
			// ReservedPath, not ignoredFile(name): the builtin exclusions
			// include a whole-path rule (agent hook config) that a base name
			// cannot express, and the outbound half has to match the inbound
			// one or a file lands on the hub that no peer will ever materialize.
			// SkipUp, not Skip: this walk is the upload door, and a negation a
			// TEAMMATE pushed must not widen what leaves this machine. See
			// Filter.SkipUp.
			if !d.Type().IsRegular() || !journal.SafePath(rel) ||
				config.ReservedPath(rel) || filter.SkipUp(rel) {
				v = vSkipFile
			} else {
				v = vSync
			}
		case ignoredDir(d.Name()) || filter.PruneDir(rel):
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

// Measure reports what a first sync of this folder would actually upload:
// the number of files and their total bytes, after the same filter the cycle
// uses. It exists so `bdrive init` can warn about a folder nobody meant to
// sync (a home directory, a video library, a checkout whose .bdriveignore
// doesn't cover its build output) BEFORE the first push rather than after.
//
// It is deliberately filter-aware: a 40 GB repo whose bulk is node_modules
// measures as the few MB that really sync, so the warning fires on the cases
// that are actually expensive and stays quiet on the ones the starter rules
// already handle.
//
// Unreadable entries are skipped rather than failing — this is advice, and a
// permission error in one subtree must never block init.
func Measure(folder string, include []string) (files int, bytes int64, err error) {
	filter, err := LoadFilter(folder, include)
	if err != nil {
		return 0, 0, err
	}
	err = walkFolder(folder, filter, func(_, _ string, d fs.DirEntry, v verdict) error {
		if v != vSync {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}
