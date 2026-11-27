package store

import (
	"os"
	"sort"
	"time"
)

// EvictLazyBlobs caps the on-demand content cache: among candidates (blobs
// that only back lazy files this device has not materialized), it keeps the
// most recently read until their total reaches capBytes and removes the rest.
// A blob's mtime is its last-read time (syncer.EnsureContent touches it), and
// a blob younger than minAge is never removed, so a read in flight keeps its
// content. The caller decides what is a candidate and must hold the volume
// flock; nothing outside candidates is ever looked at.
func (s *Store) EvictLazyBlobs(candidates map[string]bool, capBytes int64, minAge time.Duration, dryRun bool) (removed int, freed int64, err error) {
	type blob struct {
		path string
		size int64
		read time.Time
	}
	var all []blob
	for sum := range candidates {
		p := s.BlobPath(sum)
		fi, err := os.Stat(p)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		all = append(all, blob{p, fi.Size(), fi.ModTime()})
	}
	sort.Slice(all, func(i, j int) bool { return all[i].read.After(all[j].read) })
	var kept int64
	for _, b := range all {
		if kept+b.size <= capBytes || time.Since(b.read) < minAge {
			kept += b.size
			continue
		}
		if !dryRun {
			if err := os.Remove(b.path); err != nil && !os.IsNotExist(err) {
				return removed, freed, err
			}
		}
		removed++
		freed += b.size
	}
	return removed, freed, nil
}
