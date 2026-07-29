package syncer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// Restore writes the historical version sha of path back into the working
// folder as an ordinary local edit. The next Cycle journals it like any other
// change — nothing here appends to a journal, and no journal is ever
// rewritten. Restoring is exactly the edit a human could have made by hand,
// which is why the sync engine needs no new write path for it.
//
// It does not take the volume flock: Cycle does, and holding it here would
// deadlock the caller that runs both.
func (s *Session) Restore(ctx context.Context, path, sha string) error {
	proj, _, err := config.LoadProject(s.Folder)
	if err != nil {
		return err
	}
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	// Without this the scan would silently drop the write and the user would
	// be told "restored" with nothing happening.
	if filter.Skip(path) || neverSync(path) {
		return fmt.Errorf("%s is excluded from syncing here (see %s and this project's scope)", path, IgnoreFile)
	}
	if err := s.fetchBlob(ctx, sha); err != nil {
		return err
	}
	abs := filepath.Join(s.Folder, filepath.FromSlash(path))
	// writeFile is the same atomic .bdrive-tmp-* + rename materialize uses, so
	// a daemon cycle landing mid-write can never journal a partial file.
	return s.writeFile(abs, journal.FileState{Blob: sha, Mode: fileMode(abs)})
}

// fetchBlob makes sure the content is in the local store, pulling it from the
// remote when this device never held that version.
func (s *Session) fetchBlob(ctx context.Context, sha string) error {
	if s.Store.HasBlob(sha) {
		return nil
	}
	if s.Backend == nil {
		return fmt.Errorf("that version isn't on this device and the hub is unreachable")
	}
	rc, err := s.Backend.Get(ctx, "blobs/"+sha)
	if err != nil {
		return fmt.Errorf("fetch version: %w", err)
	}
	defer rc.Close()
	got, _, err := s.Store.PutBlobReader(rc)
	if err != nil {
		return err
	}
	if got != sha {
		return fmt.Errorf("version %s arrived corrupt (hashed to %s)", sha, got)
	}
	return nil
}

// fileMode keeps the file's current permissions when it still exists —
// restoring content is not a reason to reset the mode.
func fileMode(abs string) uint32 {
	if fi, err := os.Stat(abs); err == nil {
		return uint32(fi.Mode().Perm())
	}
	return 0o644
}
