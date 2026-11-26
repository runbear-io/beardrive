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
	Files map[string]journal.FileState
	Lazy  config.Lazy
	cache map[string]store.CachedFile
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
	return &Index{Files: files, Lazy: proj.Lazy, cache: cache}, nil
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
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if f, _, err := store.OpenRegularUnderRoot(s.Folder, abs); err == nil {
		return f, FromDisk, nil
	}
	want, ok := ix.Files[rel]
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
