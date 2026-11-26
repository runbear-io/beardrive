package syncer

// The read side of lazy mode: what `bdrive cat/ls/grep` and the local
// `bdrive mcp` use to see a project whose large files are not on disk.
//
// Metadata is always local (the journals are), and content is addressed by
// sha256 — so "is my cached copy current" is answered by the fold, with no
// request to the hub: if the fold says a path is blob X and the store holds
// X, the copy is valid. Only content this device never held costs a fetch,
// and it lands in the same blob store the sync engine uses, where `bdrive gc`
// and the daemon's prune cap it (daemon.PruneVolumeBlobs).
//
// Nothing here takes the volume lock or writes a journal or the state cache:
// a read must never wait on a long sync cycle, and only Cycle decides what
// the working folder holds. Blob writes are atomic temp+rename, so a fetch
// racing a cycle is safe.

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

// Index is a read-only view of a mount: every path the project holds, as
// this device's journals fold it, after this mount's ignore rules.
type Index struct {
	Files  map[string]journal.FileState
	Lazy   config.Lazy
	cache  map[string]store.CachedFile
	filter *Filter
}

// LoadIndex folds the local journals into the project's current file list.
// It never contacts the hub.
func (s *Session) LoadIndex() (*Index, error) {
	proj, _, err := config.LoadProject(s.Folder)
	if err != nil {
		return nil, err
	}
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	fold, err := s.Store.ReplayFold()
	if err != nil {
		return nil, err
	}
	if st, err := s.Store.LoadSync(); err == nil {
		filter.AcceptRules(st.IgnoreAccepted)
	}
	cache, err := s.Store.LoadCacheReadOnly(s.mountID())
	if err != nil {
		cache = nil
	}
	files := make(map[string]journal.FileState, len(fold.Target))
	for rel, fs := range fold.Target {
		if fs.Link != "" || unsafeRel(rel) || neverSync(rel) || filter.Skip(rel) {
			continue
		}
		files[rel] = fs
	}
	return &Index{Files: files, Lazy: proj.Lazy, cache: cache, filter: filter}, nil
}

// Materialized reports whether this mount wrote rel to disk (its state cache
// tracks it). A lazy file that was never downloaded is not.
func (ix *Index) Materialized(rel string) bool {
	_, ok := ix.cache[rel]
	return ok
}

// LazyCount is how many files of the project this mount keeps on the hub
// only, and their total size.
func (ix *Index) LazyCount() (n int, bytes int64) {
	for rel, fs := range ix.Files {
		if !ix.Materialized(rel) && ix.Lazy.Applies(rel, fs.Size) {
			n++
			bytes += fs.Size
		}
	}
	return n, bytes
}

// EnsureContent makes sure the store holds the content of rel's version
// want, fetching it from the remote when this device never held it, and
// marks it recently read for the cache's LRU. A cache hit makes no request.
func (s *Session) EnsureContent(ctx context.Context, rel string, want journal.FileState) error {
	if want.Blob == "" {
		return fmt.Errorf("%s has no content", rel)
	}
	if !s.Store.HasBlob(want.Blob) {
		if s.Backend == nil {
			return fmt.Errorf("%s is not downloaded on this device and the hub is unreachable", rel)
		}
		ok, cerr, abort := s.fetchOneBlob(ctx, journal.Op{Kind: journal.KindPut, Path: rel, Blob: want.Blob, Size: want.Size, Mode: want.Mode}, nil)
		if abort != nil {
			return abort
		}
		if cerr != nil {
			return cerr
		}
		if !ok || !s.Store.HasBlob(want.Blob) {
			return fmt.Errorf("could not download %s from the hub", rel)
		}
	}
	// The blob's mtime is the LRU clock (store.EvictLazyBlobs). Touching only
	// ever makes a blob look younger, which no prune rule reads as "remove".
	now := time.Now()
	_ = os.Chtimes(s.Store.BlobPath(want.Blob), now, now)
	return nil
}

// Source says where Open found a file's bytes.
type Source string

const (
	FromDisk  Source = "disk"  // the working file (may hold unsynced edits)
	FromCache Source = "cache" // the local blob store, no request
	FromHub   Source = "hub"   // fetched now, and cached
)

// Open returns rel's current content: the working file when it is on disk,
// else the blob of the version the journals fold to — from the local store,
// or fetched from the hub and kept there.
func (s *Session) Open(ctx context.Context, ix *Index, rel string) (io.ReadCloser, Source, error) {
	if !journal.SafePath(rel) || neverSync(rel) {
		return nil, "", fmt.Errorf("%s: not a path in this project", rel)
	}
	// Only what this project syncs is readable here: never a file the
	// ignore rules or scope exclude (a local .env), never one inside a nested
	// project (its own project's business). A project file not journaled yet
	// is still readable — it is on its way up.
	want, indexed := ix.Files[rel]
	diskOK := true
	if ix.filter != nil {
		if ix.filter.Skip(rel) || ix.filter.underMountOnDisk(rel) {
			return nil, "", fmt.Errorf("%s: not a file this project syncs", rel)
		}
		// The disk copy is served only if this device would upload it. A
		// path its accepted rules exclude is local-only (a .env a teammate's
		// widened rules also name): serve the journaled version, never the
		// local bytes.
		diskOK = !ix.filter.SkipUp(rel)
	}
	if !diskOK && !indexed {
		return nil, "", fmt.Errorf("%s: not a file this project syncs", rel)
	}
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if diskOK {
		if f, _, err := store.OpenRegularUnderRoot(s.Folder, abs); err == nil {
			return f, FromDisk, nil
		}
	}
	ok := indexed
	if !ok {
		return nil, "", fmt.Errorf("%s: no such file", rel)
	}
	src := FromCache
	if !s.Store.HasBlob(want.Blob) {
		src = FromHub
	}
	if err := s.EnsureContent(ctx, rel, want); err != nil {
		return nil, "", err
	}
	f, err := s.Store.OpenBlob(want.Blob)
	if err != nil {
		return nil, "", err
	}
	return f, src, nil
}

// Writable reports why a write at rel would never sync from this device, or
// nil: excluded by the ignore rules or scope, under a folder the hub made
// read-only (the next cycle reverts it), or under a folder this account
// cannot read. The local MCP refuses such a write up front instead of
// reporting a success that silently never reaches anyone.
func (s *Session) Writable(rel string) error {
	if neverSync(rel) {
		return fmt.Errorf("%s: BearDrive never syncs this path", rel)
	}
	proj, _, err := config.LoadProject(s.Folder)
	if err != nil {
		return err
	}
	filter, err := loadFilter(s.Folder, proj.Include)
	if err != nil {
		return fmt.Errorf("load %s: %w", IgnoreFile, err)
	}
	st, err := s.Store.LoadSync()
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	filter.AcceptRules(st.IgnoreAccepted)
	switch {
	case filter.underMountOnDisk(rel):
		return fmt.Errorf("%s is inside another project synced here; change it from that project", rel)
	case filter.SkipUp(rel):
		return fmt.Errorf("%s is excluded from syncing here (see %s and this project's scope)", rel, IgnoreFile)
	case underAny(rel, sanePrefixes(st.Denied)):
		return fmt.Errorf("%s is in a folder you cannot access", rel)
	case underAny(rel, sanePrefixes(st.ReadOnly)):
		return fmt.Errorf("%s is in a read-only folder; your change would be reverted", rel)
	}
	return nil
}
