package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/store"
)

func lifecycleLink(t *testing.T, s *Session, rel, target string) {
	t.Helper()
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, abs); err != nil {
		t.Fatal(err)
	}
}

func lifecycleRemove(t *testing.T, s *Session, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(s.Folder, filepath.FromSlash(rel))); err != nil {
		t.Fatal(err)
	}
}

func lifecycleAssertLink(t *testing.T, s *Session, rel, target string) {
	t.Helper()
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatalf("Lstat %s: %v", rel, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s is %v, want symlink", rel, fi.Mode())
	}
	got, err := os.Readlink(abs)
	if err != nil || got != target {
		t.Fatalf("Readlink %s = %q, %v; want %q", rel, got, err, target)
	}
}

func lifecycleAssertGone(t *testing.T, s *Session, rel string) {
	t.Helper()
	if _, err := os.Lstat(filepath.Join(s.Folder, filepath.FromSlash(rel))); !os.IsNotExist(err) {
		t.Errorf("%s still exists after peer delete: %v", rel, err)
	}
}

func lifecycleCache(t *testing.T, s *Session) map[string]store.CachedFile {
	t.Helper()
	cache, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		t.Fatal(err)
	}
	return cache
}

func lifecycleAssertNoReadOnlyOps(t *testing.T, s *Session) {
	t.Helper()
	ops, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range ops {
		if strings.HasPrefix(op.Path, "locked/") {
			t.Errorf("journaled read-only local state: %+v", op)
		}
	}
}

func lifecyclePreserve(s *Session, rel string) error {
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	fi, err := os.Lstat(abs)
	if err != nil {
		return err
	}
	return s.preserveLocalEntry(abs, rel, fi)
}

// QA-331-01: the exact two-device dangling-link deletion lifecycle.
func TestReproPeerDanglingSymlinkDeletion(t *testing.T) {
	be := sharedRemote(t)
	a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
	lifecycleLink(t, a, "docs/alias", "missing")
	cycle(t, a)
	cycle(t, b)
	lifecycleAssertLink(t, b, "docs/alias", "missing")
	lifecycleRemove(t, a, "docs/alias")
	cycle(t, a)
	cycle(t, b)
	lifecycleAssertGone(t, b, "docs/alias")
	if _, ok := lifecycleCache(t, b)["docs/alias"]; ok {
		t.Error("deleted dangling link remains cached")
	}
	cycle(t, b)
	cycle(t, a)
	lifecycleAssertGone(t, a, "docs/alias")
}

// QA-331-02: unlink the entry, never its live file or directory target.
func TestSymlink_DeleteDoesNotTouchTarget(t *testing.T) {
	for _, directory := range []bool{false, true} {
		t.Run(fmt.Sprint(directory), func(t *testing.T) {
			be := sharedRemote(t)
			a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
			target := "target.txt"
			if directory {
				target = "target"
				write(t, a.Folder, "target/keep.txt", "keep")
			} else {
				write(t, a.Folder, target, "keep")
			}
			lifecycleLink(t, a, "alias", target)
			cycle(t, a)
			cycle(t, b)
			lifecycleRemove(t, a, "alias")
			cycle(t, a)
			cycle(t, b)
			lifecycleAssertGone(t, b, "alias")
			if directory {
				target += "/keep.txt"
			}
			if got := read(t, b.Folder, target); got != "keep" {
				t.Fatalf("target changed: %q", got)
			}
		})
	}
}

// QA-331-03/04: missed watcher events must not let pull overwrite a dirty entry.
func TestSymlink_WritableDirtyEntrySurvivesPeerChanges(t *testing.T) {
	for _, initial := range []string{"link", "file"} {
		for _, local := range []string{"link", "file", "dir"} {
			for _, peer := range []string{"delete", "link", "file"} {
				t.Run(initial+"-"+local+"-"+peer, func(t *testing.T) {
					be := sharedRemote(t)
					a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
					if initial == "link" {
						lifecycleLink(t, a, "entry", "old")
					} else {
						write(t, a.Folder, "entry", "original")
					}
					cycle(t, a)
					cycle(t, b)
					lifecycleRemove(t, b, "entry")
					switch local {
					case "link":
						lifecycleLink(t, b, "entry", "local-target")
					case "file":
						write(t, b.Folder, "entry", "local bytes")
					case "dir":
						write(t, b.Folder, "entry/keep", "local bytes")
					}
					lifecycleRemove(t, a, "entry")
					switch peer {
					case "link":
						lifecycleLink(t, a, "entry", "peer-target")
					case "file":
						write(t, a.Folder, "entry", "peer bytes")
					}
					cycle(t, a)
					targetedSafetyCycle(t, b, &ScanHint{})
					if local == "link" {
						lifecycleAssertLink(t, b, "entry", "local-target")
					} else {
						rel := "entry"
						if local == "dir" {
							rel += "/keep"
						}
						if got := read(t, b.Folder, rel); got != "local bytes" {
							t.Fatalf("dirty state overwritten: %q", got)
						}
					}
				})
			}
		}
	}
}

// QA-331-05: neither writes, deletes nor read-only copy-asides may follow an ancestor link.
func TestSymlink_AncestorEscapeBlocked(t *testing.T) {
	for _, replacement := range []string{"delete", "file", "link"} {
		t.Run(replacement, func(t *testing.T) {
			s := newDevice(t, "devb", sharedRemote(t))
			s.readOnly = []string{"locked/"}
			outside := t.TempDir()
			write(t, outside, "entry", "outside must survive")
			lifecycleLink(t, s, "locked", outside)
			cache := map[string]store.CachedFile{"locked/entry": {Blob: "old", Size: 1}}
			sum, size, err := s.Store.PutBlobBytes([]byte("peer bytes"))
			if err != nil {
				t.Fatal(err)
			}
			target := map[string]journal.FileState{}
			if replacement == "file" {
				target["locked/entry"] = journal.FileState{Blob: sum, Size: size}
			}
			if replacement == "link" {
				target["locked/entry"] = journal.FileState{Link: "peer-target"}
			}
			if _, err := s.materialize(target, cache, &Filter{}); err != nil {
				t.Fatal(err)
			}
			if err := lifecyclePreserve(s, "locked/entry"); err == nil {
				t.Error("copy-aside accepted escaping ancestor")
			}
			if got := read(t, outside, "entry"); got != "outside must survive" {
				t.Fatalf("outside entry changed: %q", got)
			}
			entries, err := os.ReadDir(outside)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Fatalf("copy-aside escaped mount: %v", entries)
			}
		})
	}
}

// QA-331-06: dangling links are existing entries, and live links use their own fingerprint.
func TestSymlink_ReadOnlyCleanLinkNotDrifted(t *testing.T) {
	for _, target := range []string{"missing", "target.txt"} {
		t.Run(target, func(t *testing.T) {
			be := sharedRemote(t)
			a := newDevice(t, "deva", be)
			b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
			write(t, a.Folder, "target.txt", "a real target with a different size")
			lifecycleLink(t, a, "locked/entry", target)
			cycle(t, a)
			cycle(t, b)
			// A live link inside locked/ resolves to this target, not the root file.
			if target == "target.txt" {
				write(t, b.Folder, "locked/target.txt", "local target")
			}
			cache := lifecycleCache(t, b)
			if b.readOnlyDrifted("locked/entry", filepath.Join(b.Folder, "locked/entry"), cache["locked/entry"]) {
				t.Error("clean read-only symlink falsely drifted")
			}
			res := cycle(t, b)
			if len(res.Reverted) != 0 || res.Materialized != 0 {
				t.Errorf("clean link caused a revert: %+v", res)
			}
		})
	}
}

// QA-331-07/08/09: read-only edits are copied as entries, then remote state wins.
func TestSymlink_ReadOnlyLocalEntryPreservedBeforePeerChange(t *testing.T) {
	for _, initial := range []string{"link", "file"} {
		for _, local := range []string{"dangling", "live", "file", "dir"} {
			for _, peer := range []string{"delete", "link", "file"} {
				t.Run(initial+"-"+local+"-"+peer, func(t *testing.T) {
					be := sharedRemote(t)
					a := newDevice(t, "deva", be)
					b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
					if initial == "link" {
						lifecycleLink(t, a, "locked/entry", "old")
					} else {
						write(t, a.Folder, "locked/entry", "original")
					}
					cycle(t, a)
					cycle(t, b)
					lifecycleRemove(t, b, "locked/entry")
					linkTarget := "missing"
					switch local {
					case "live":
						linkTarget = filepath.Join(t.TempDir(), "secret")
						if err := os.WriteFile(linkTarget, []byte("target must not be read or changed"), 0o644); err != nil {
							t.Fatal(err)
						}
						lifecycleLink(t, b, "locked/entry", linkTarget)
					case "dangling":
						lifecycleLink(t, b, "locked/entry", linkTarget)
					case "file":
						write(t, b.Folder, "locked/entry", "local bytes")
					case "dir":
						write(t, b.Folder, "locked/entry/keep", "local bytes")
					}
					lifecycleRemove(t, a, "locked/entry")
					switch peer {
					case "link":
						lifecycleLink(t, a, "locked/entry", "peer-target")
					case "file":
						write(t, a.Folder, "locked/entry", "peer bytes")
					}
					cycle(t, a)
					cycle(t, b)
					if local == "dir" {
						if got := read(t, b.Folder, "locked/entry/keep"); got != "local bytes" {
							t.Fatalf("user directory destroyed: %q", got)
						}
						return
					}
					if local == "file" {
						if got := read(t, b.Folder, "locked/entry (local, not synced)"); got != "local bytes" {
							t.Fatalf("local copy = %q", got)
						}
					} else {
						lifecycleAssertLink(t, b, "locked/entry (local, not synced)", linkTarget)
					}
					switch peer {
					case "delete":
						lifecycleAssertGone(t, b, "locked/entry")
						if _, ok := lifecycleCache(t, b)["locked/entry"]; ok {
							t.Error("read-only deleted entry remains cached")
						}
					case "link":
						lifecycleAssertLink(t, b, "locked/entry", "peer-target")
					case "file":
						fi, err := os.Lstat(filepath.Join(b.Folder, "locked/entry"))
						if err != nil {
							t.Fatal(err)
						}
						if !fi.Mode().IsRegular() {
							t.Fatalf("peer file remains %v", fi.Mode())
						}
						if got := read(t, b.Folder, "locked/entry"); got != "peer bytes" {
							t.Fatalf("peer bytes = %q", got)
						}
					}
					cycle(t, b)
					lifecycleAssertNoReadOnlyOps(t, b)
					if local == "live" {
						data, err := os.ReadFile(linkTarget)
						if err != nil || string(data) != "target must not be read or changed" {
							t.Fatalf("live target changed: %q, %v", data, err)
						}
					}
				})
			}
		}
	}
}

func TestSymlink_ReadOnlyFileFingerprintCannotHideLocalLink(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
	write(t, a.Folder, "locked/entry", "original")
	cycle(t, a)
	cycle(t, b)
	c := lifecycleCache(t, b)["locked/entry"]
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("original"), 0o644); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(b.Folder, "locked/entry"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(target, fi.ModTime(), fi.ModTime()); err != nil {
		t.Fatal(err)
	}
	lifecycleRemove(t, b, "locked/entry")
	lifecycleLink(t, b, "locked/entry", target)
	if !b.readOnlyDrifted("locked/entry", filepath.Join(b.Folder, "locked/entry"), c) {
		t.Error("target fingerprint hid a local symlink")
	}
	cycle(t, b)
	lifecycleAssertLink(t, b, "locked/entry (local, not synced)", target)
	fi, err = os.Lstat(filepath.Join(b.Folder, "locked/entry"))
	if err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("regular file not restored: %v, %v", fi, err)
	}
}

func TestSymlink_LocalCopyDeduplicatesWithoutFollowingLinks(t *testing.T) {
	s := newDevice(t, "devb", sharedRemote(t))
	lifecycleLink(t, s, "locked/entry", "missing")
	if err := lifecyclePreserve(s, "locked/entry"); err != nil {
		t.Fatal(err)
	}
	before, err := os.Lstat(filepath.Join(s.Folder, "locked/entry (local, not synced)"))
	if err != nil {
		t.Fatal(err)
	}
	if err := lifecyclePreserve(s, "locked/entry"); err != nil {
		t.Fatal(err)
	}
	after, err := os.Lstat(filepath.Join(s.Folder, "locked/entry (local, not synced)"))
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("identical local link copy was rewritten")
	}
	lifecycleAssertLink(t, s, "locked/entry (local, not synced)", "missing")
}

// QA-331-10: offline concurrent link/link and link/file edits preserve both versions.
func TestSymlink_ConcurrentConflictCopiesPreserveEntryType(t *testing.T) {
	for _, aKind := range []string{"link", "file"} {
		for _, bKind := range []string{"link", "file"} {
			if aKind == "file" && bKind == "file" {
				continue
			}
			t.Run(aKind+"-"+bKind, func(t *testing.T) {
				be := sharedRemote(t)
				a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
				lifecycleLink(t, a, "entry", "base")
				cycle(t, a)
				cycle(t, b)
				for i, s := range []*Session{a, b} {
					lifecycleRemove(t, s, "entry")
					kind := aKind
					if i == 1 {
						kind = bKind
					}
					if kind == "link" {
						lifecycleLink(t, s, "entry", s.Device.ID+"-target")
					} else {
						write(t, s.Folder, "entry", s.Device.ID+" bytes")
					}
					s.Backend = nil
					cycle(t, s)
					s.Backend = be
				}
				cycle(t, a)
				cycle(t, b)
				cycle(t, a)
				for _, s := range []*Session{a, b} {
					found := map[string]bool{}
					all, err := s.Store.AllOps()
					if err != nil {
						t.Fatal(err)
					}
					state := journal.Replay(all)
					for rel := range state {
						if rel != "entry" && !strings.HasPrefix(rel, "entry.bdrive-conflict-") {
							continue
						}
						fi, err := os.Lstat(filepath.Join(s.Folder, rel))
						if err != nil {
							t.Fatal(err)
						}
						if fi.Mode()&os.ModeSymlink != 0 {
							target, err := os.Readlink(filepath.Join(s.Folder, rel))
							if err != nil {
								t.Fatal(err)
							}
							found[target] = true
						} else {
							found[read(t, s.Folder, rel)] = true
						}
					}
					for _, item := range []struct{ kind, device string }{{aKind, "deva"}, {bKind, "devb"}} {
						want := item.device + " bytes"
						if item.kind == "link" {
							want = item.device + "-target"
						}
						if !found[want] {
							t.Errorf("%s lost concurrent %s: %v", s.Device.ID, want, found)
						}
					}
					before := len(journalOps(t, s))
					cycle(t, s)
					if got := len(journalOps(t, s)); got != before {
						t.Errorf("settled conflict duplicated ops: %d -> %d", before, got)
					}
				}
			})
		}
	}
}

// A failed push leaves the losing op pending; the existing link copy must suppress it.
func TestSymlink_ConflictCopyIdempotentWhileUnpushed(t *testing.T) {
	s := newDevice(t, "devb", sharedRemote(t))
	mine := journal.Op{Kind: journal.KindPut, Path: "entry", Link: "mine", Device: s.Device.ID, DeviceName: s.Device.Name, Seq: 1, Lamport: 1}
	peer := journal.Op{Kind: journal.KindPut, Path: "entry", Link: "theirs", Device: "deva", Seq: 1, Lamport: 2}
	state := journal.Replay([]journal.Op{mine, peer})
	st := &store.SyncState{Lamport: 2}
	ops, err := s.conflictCopiesWithState([]journal.Op{mine}, 1, []journal.Op{peer}, st, []journal.Op{mine, peer}, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Link != "mine" || ops[0].Blob != "" {
		t.Fatalf("lost link not preserved: %+v", ops)
	}
	state[ops[0].Path] = journal.FileState{Link: "mine"}
	ops, err = s.conflictCopiesWithState([]journal.Op{mine}, 2, nil, st, nil, state)
	if err != nil || len(ops) != 0 {
		t.Fatalf("link copy duplicated: %+v, %v", ops, err)
	}
}

func TestSymlink_WithdrawnLinkRemainsMaterialized(t *testing.T) {
	be := sharedRemote(t)
	a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
	lifecycleLink(t, a, "entry", "missing")
	cycle(t, a)
	cycle(t, b)
	// A shrinking journal is rejected wholesale. Replace the applied op with
	// an unrelated, newer slot instead: the accepted journal keeps its count,
	// so preserving the withdrawn entry depends on stillHold.
	data, err := json.Marshal(journal.Op{
		Kind: journal.KindPut, Path: "other", Link: "other-missing",
		Device: a.Device.ID, Seq: 2, Lamport: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, '\n')
	if err := be.Put(context.Background(), "journal/deva.jsonl", strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
	cycle(t, b)
	lifecycleAssertLink(t, b, "entry", "missing")
	ops, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 1 || ops[0].Link != "missing" || ops[0].Note != reassertNote {
		t.Fatalf("withdrawn link was not reasserted: %+v", ops)
	}
	cycle(t, b)
	if got := len(journalOps(t, b)); got != 1 {
		t.Fatalf("reassertion duplicated: %d ops", got)
	}
}

func TestSymlink_IdenticalConcurrentLinksHaveNoConflict(t *testing.T) {
	s := newDevice(t, "devb", sharedRemote(t))
	mine := journal.Op{Kind: journal.KindPut, Path: "entry", Link: "same", Device: "devb"}
	peer := journal.Op{Kind: journal.KindPut, Path: "entry", Link: "same", Device: "deva"}
	ops, err := s.conflictCopiesWithState([]journal.Op{mine}, 1, []journal.Op{peer},
		&store.SyncState{}, nil, map[string]journal.FileState{"entry": {Link: "same"}})
	if err != nil || len(ops) != 0 {
		t.Fatalf("identical links conflicted: %+v, %v", ops, err)
	}
	if s.stillHold(mine, map[string]store.CachedFile{"entry": {Link: "different"}}) {
		t.Error("same empty Blob hid a different cached link")
	}
}

func TestSymlink_LocalCopyNeverOverwritesPriorEntry(t *testing.T) {
	for _, kind := range []string{"link", "file"} {
		t.Run(kind, func(t *testing.T) {
			s := newDevice(t, "devb", sharedRemote(t))
			if kind == "link" {
				lifecycleLink(t, s, "locked/entry", "missing")
			} else {
				write(t, s.Folder, "locked/entry", "local bytes")
			}
			outside := t.TempDir()
			write(t, outside, "secret", "outside")
			target := filepath.Join(outside, "secret")
			lifecycleLink(t, s, "locked/entry (local, not synced)", target)
			for range 2 {
				if err := lifecyclePreserve(s, "locked/entry"); err != nil {
					t.Fatal(err)
				}
			}
			lifecycleAssertLink(t, s, "locked/entry (local, not synced)", target)
			if got := read(t, outside, "secret"); got != "outside" {
				t.Fatalf("copy-aside followed existing link: %q", got)
			}
			if kind == "link" {
				lifecycleAssertLink(t, s, "locked/entry (local, not synced 2)", "missing")
			} else {
				if got := read(t, s.Folder, "locked/entry (local, not synced 2)"); got != "local bytes" {
					t.Fatalf("local copy lost: %q", got)
				}
			}
			entries, err := os.ReadDir(filepath.Join(s.Folder, "locked"))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 3 {
				t.Fatalf("local copies not deduplicated: %v", entries)
			}
		})
	}
}

func TestSymlink_CleanEntryTransitionsConverge(t *testing.T) {
	be := sharedRemote(t)
	a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
	lifecycleLink(t, a, "entry", "first-missing")
	cycle(t, a)
	cycle(t, b)
	for _, target := range []string{"next-missing", "", "final-missing"} {
		lifecycleRemove(t, a, "entry")
		if target == "" {
			write(t, a.Folder, "entry", "regular bytes")
		} else {
			lifecycleLink(t, a, "entry", target)
		}
		cycle(t, a)
		cycle(t, b)
		if target != "" {
			lifecycleAssertLink(t, b, "entry", target)
		} else {
			fi, err := os.Lstat(filepath.Join(b.Folder, "entry"))
			if err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("link did not become regular: %v, %v", fi, err)
			}
			if got := read(t, b.Folder, "entry"); got != "regular bytes" {
				t.Fatalf("regular bytes = %q", got)
			}
		}
	}
	lifecycleRemove(t, a, "entry")
	cycle(t, a)
	cycle(t, b)
	lifecycleAssertGone(t, b, "entry")
}

func TestSymlink_ForestCleanReadOnlyUpdatesCreateNoLocalCopies(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
	lifecycleLink(t, a, "locked/entry", "first")
	cycle(t, a)
	cycle(t, b)
	lifecycleRemove(t, a, "locked/entry")
	lifecycleLink(t, a, "locked/entry", "second")
	cycle(t, a)
	res := cycle(t, b)
	lifecycleAssertLink(t, b, "locked/entry", "second")
	if len(res.Reverted) != 0 {
		t.Errorf("clean peer update reported local reverts: %v", res.Reverted)
	}
	entries, err := os.ReadDir(filepath.Join(b.Folder, "locked"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "entry" {
		t.Errorf("clean peer retarget created sidecars: %v", entries)
	}
	lifecycleRemove(t, a, "locked/entry")
	cycle(t, a)
	res = cycle(t, b)
	lifecycleAssertGone(t, b, "locked/entry")
	if len(res.Reverted) != 0 {
		t.Errorf("clean peer delete reported local reverts: %v", res.Reverted)
	}
	_ = filepath.WalkDir(b.Folder, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			t.Error(err)
			return nil
		}
		if strings.Contains(d.Name(), "(local, not synced") {
			t.Errorf("clean peer changes created %s", p)
		}
		return nil
	})
	lifecycleAssertNoReadOnlyOps(t, b)
}

func TestSymlink_ForestLocalCopyCollisionProbingIsBounded(t *testing.T) {
	for _, kind := range []string{"file", "link"} {
		t.Run(kind, func(t *testing.T) {
			be := sharedRemote(t)
			a := newDevice(t, "deva", be)
			b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
			if kind == "link" {
				lifecycleLink(t, a, "locked/entry", "project")
			} else {
				write(t, a.Folder, "locked/entry", "project")
			}
			cycle(t, a)
			cycle(t, b)
			lifecycleRemove(t, b, "locked/entry")
			if kind == "link" {
				lifecycleLink(t, b, "locked/entry", "65th local edit")
			} else {
				write(t, b.Folder, "locked/entry", "65th local edit")
			}
			copies := make(map[string]string)
			for n := 1; n <= 64; n++ {
				rel := "locked/entry (local, not synced)"
				if n > 1 {
					rel = fmt.Sprintf("locked/entry (local, not synced %d)", n)
				}
				value := fmt.Sprintf("kept local edit %d", n)
				copies[rel] = value
				if kind == "link" {
					lifecycleLink(t, b, rel, value)
				} else {
					write(t, b.Folder, rel, value)
				}
			}
			for range 2 {
				res := cycle(t, b)
				if len(res.Reverted) != 0 || res.Materialized != 0 {
					t.Errorf("exhausted preservation still overwrote local state: %+v", res)
				}
				for rel, want := range copies {
					if kind == "link" {
						target, err := os.Readlink(filepath.Join(b.Folder, rel))
						if err != nil || target != want {
							t.Errorf("prior copy %s changed: %q, %v", rel, target, err)
						}
					} else if got := read(t, b.Folder, rel); got != want {
						t.Errorf("prior copy %s changed: %q", rel, got)
					}
				}
				if kind == "link" {
					lifecycleAssertLink(t, b, "locked/entry", "65th local edit")
				} else if got := read(t, b.Folder, "locked/entry"); got != "65th local edit" {
					t.Errorf("65th edit was overwritten: %q", got)
				}
			}
			lifecycleAssertGone(t, b, "locked/entry (local, not synced 65)")
			lifecycleAssertNoReadOnlyOps(t, b)
		})
	}
}

func lifecycleAssertDrift(t *testing.T, s *Session, cache map[string]store.CachedFile, want [3]int) {
	t.Helper()
	added, modified, removed, err := Drift(s.Folder, nil, "", cache)
	if err != nil {
		t.Fatal(err)
	}
	if got := [3]int{added, modified, removed}; got != want {
		t.Errorf("Drift = %v, want %v", got, want)
	}
}

func lifecycleAssertDiscovery(t *testing.T, s *Session, want ...string) {
	t.Helper()
	paths, err := SyncedFiles(s.Folder, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != len(want) {
		t.Errorf("SyncedFiles = %v, want %v", paths, want)
	}
	var wantBytes int64
	for _, rel := range want {
		found := false
		for _, got := range paths {
			if got == rel {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("SyncedFiles omitted %s: %v", rel, paths)
		}
		fi, err := os.Lstat(filepath.Join(s.Folder, rel))
		if err != nil {
			t.Fatal(err)
		}
		wantBytes += fi.Size()
	}
	files, bytes, err := Measure(s.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if files != len(want) || bytes != wantBytes {
		t.Errorf("Measure = %d files, %d bytes; want %d, %d", files, bytes, len(want), wantBytes)
	}
}

func TestSymlink_ForestDiscoveryAndDriftFollowEntryTypes(t *testing.T) {
	s := newDevice(t, "deva", sharedRemote(t))
	write(t, s.Folder, "regular", "bytes")
	lifecycleLink(t, s, "link", "missing-a")
	cycle(t, s)
	cache := lifecycleCache(t, s)
	lifecycleAssertDrift(t, s, cache, [3]int{})
	lifecycleAssertDiscovery(t, s, "regular", "link")

	lifecycleLink(t, s, "added", "another-missing")
	lifecycleAssertDrift(t, s, cache, [3]int{1, 0, 0})
	lifecycleAssertDiscovery(t, s, "regular", "link", "added")
	lifecycleRemove(t, s, "added")

	// A touched/recreated link with the same target is not a content edit.
	c := cache["link"]
	c.MTimeNS--
	cache["link"] = c
	lifecycleAssertDrift(t, s, cache, [3]int{})
	lifecycleRemove(t, s, "link")
	lifecycleLink(t, s, "link", "missing-b")
	fi, err := os.Lstat(filepath.Join(s.Folder, "link"))
	if err != nil {
		t.Fatal(err)
	}
	c.MTimeNS = fi.ModTime().UnixNano() - 1
	cache["link"] = c
	lifecycleAssertDrift(t, s, cache, [3]int{0, 1, 0})
	lifecycleAssertDiscovery(t, s, "regular", "link")

	// Both swaps deliberately match the old cached size/mtime: type matters.
	lifecycleRemove(t, s, "link")
	write(t, s.Folder, "link", "now bytes")
	fi, err = os.Lstat(filepath.Join(s.Folder, "link"))
	if err != nil {
		t.Fatal(err)
	}
	c.Size, c.MTimeNS = fi.Size(), fi.ModTime().UnixNano()
	cache["link"] = c
	lifecycleAssertDrift(t, s, cache, [3]int{0, 1, 0})
	lifecycleAssertDiscovery(t, s, "regular", "link")

	lifecycleRemove(t, s, "regular")
	lifecycleLink(t, s, "regular", "point")
	fi, err = os.Lstat(filepath.Join(s.Folder, "regular"))
	if err != nil {
		t.Fatal(err)
	}
	regular := cache["regular"]
	regular.Size, regular.MTimeNS = fi.Size(), fi.ModTime().UnixNano()
	cache["regular"] = regular
	lifecycleAssertDrift(t, s, cache, [3]int{0, 2, 0})
	lifecycleAssertDiscovery(t, s, "regular", "link")
}

func TestSymlink_ForestNonRegularIgnoreFailsClosedAndRecovers(t *testing.T) {
	for _, existing := range []bool{false, true} {
		for _, kind := range []string{"symlink", "dangling", "fifo", "directory"} {
			t.Run(fmt.Sprintf("existing=%t/%s", existing, kind), func(t *testing.T) {
				be := sharedRemote(t)
				a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
				const secret = "PRIVATE_SECRET_ENV_CONTENT_331"
				secretSHA := fmt.Sprintf("%x", sha256.Sum256([]byte(secret)))
				write(t, a.Folder, "secret.env", secret)
				if existing {
					write(t, a.Folder, "public.md", "old public content")
					write(t, a.Folder, IgnoreFile, "secret.env\n")
					cycle(t, a)
					lifecycleRemove(t, a, IgnoreFile)
				}
				st, err := a.Store.LoadSync()
				if err != nil {
					t.Fatal(err)
				}
				if (st.IgnoreAccepted != "") != existing {
					t.Fatalf("fixture exclusion floor = %q, existing=%t", st.IgnoreAccepted, existing)
				}
				write(t, a.Folder, "public.md", "safe public update")
				target := filepath.Join(t.TempDir(), "shared-ignore")
				if err := os.WriteFile(target, []byte("secret.env\n"), 0o600); err != nil {
					t.Fatal(err)
				}
				switch kind {
				case "symlink":
					lifecycleLink(t, a, IgnoreFile, target)
				case "dangling":
					lifecycleLink(t, a, IgnoreFile, target+"-missing")
				case "fifo":
					if err := syscall.Mkfifo(filepath.Join(a.Folder, IgnoreFile), 0o600); err != nil {
						t.Fatal(err)
					}
				case "directory":
					if err := os.Mkdir(filepath.Join(a.Folder, IgnoreFile), 0o700); err != nil {
						t.Fatal(err)
					}
				}
				before, err := a.Store.DeviceOps(a.Device.ID)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := a.Cycle(context.Background()); !errors.Is(err, store.ErrNotRegular) {
					t.Errorf("Cycle = %v, want a non-regular policy error", err)
				}
				after, err := a.Store.DeviceOps(a.Device.ID)
				if err != nil {
					t.Fatal(err)
				}
				if len(after) != len(before) {
					t.Errorf("failed policy check journaled %d new ops", len(after)-len(before))
				}
				for _, check := range []struct {
					name string
					run  func() error
				}{
					{"SyncedFiles", func() error { _, err := SyncedFiles(a.Folder, nil, st.IgnoreAccepted); return err }},
					{"Explain", func() error { _, _, err := Explain(a.Folder, nil, st.IgnoreAccepted); return err }},
					{"Measure", func() error { _, _, err := Measure(a.Folder, nil); return err }},
					{"Drift", func() error {
						_, _, _, err := Drift(a.Folder, nil, st.IgnoreAccepted, lifecycleCache(t, a))
						return err
					}},
				} {
					if err := check.run(); !errors.Is(err, store.ErrNotRegular) || os.IsNotExist(err) {
						t.Errorf("%s = %v, want a fail-closed policy error", check.name, err)
					}
				}
				assertNoSecret := func() {
					t.Helper()
					ops, err := a.Store.DeviceOps(a.Device.ID)
					if err != nil {
						t.Fatal(err)
					}
					for _, op := range ops {
						if op.Path == "secret.env" {
							t.Errorf("secret journaled locally: %+v", op)
						}
					}
					if a.Store.HasBlob(secretSHA) {
						t.Error("secret captured in the local blob store")
					}
					exists, err := be.Exists(context.Background(), "blobs/"+secretSHA)
					if err != nil {
						t.Fatal(err)
					}
					if exists {
						t.Error("secret blob uploaded")
					}
					objects, err := be.List(context.Background(), "journal/")
					if err != nil {
						t.Fatal(err)
					}
					for _, object := range objects {
						reader, err := be.Get(context.Background(), object.Key)
						if err != nil {
							t.Fatal(err)
						}
						data, readErr := io.ReadAll(reader)
						closeErr := reader.Close()
						if readErr != nil || closeErr != nil {
							t.Fatalf("read remote journal: %v; close: %v", readErr, closeErr)
						}
						ops, err := journal.Parse(data)
						if err != nil {
							t.Fatal(err)
						}
						for _, op := range ops {
							if op.Path == "secret.env" {
								t.Errorf("secret op uploaded: %+v", op)
							}
						}
					}
				}
				assertNoSecret()

				// Correcting the local policy unblocks sync without disclosing secrets.
				lifecycleRemove(t, a, IgnoreFile)
				write(t, a.Folder, IgnoreFile, "secret.env\n")
				cycle(t, a)
				cycle(t, b)
				if got := read(t, b.Folder, "public.md"); got != "safe public update" {
					t.Errorf("public recovery = %q", got)
				}
				if _, err := os.Lstat(filepath.Join(b.Folder, "secret.env")); !os.IsNotExist(err) {
					t.Errorf("secret materialized on peer: %v", err)
				}
				assertNoSecret()
			})
		}
	}
}

func TestSymlink_ForestPeerIgnoreLinkIsNotMaterialized(t *testing.T) {
	for _, cached := range []bool{false, true} {
		t.Run(fmt.Sprint(cached), func(t *testing.T) {
			be := sharedRemote(t)
			a, b := newDevice(t, "deva", be), newDevice(t, "devb", be)
			write(t, a.Folder, "secret.txt", "must sync")
			if cached {
				write(t, a.Folder, IgnoreFile, "ignored/\n")
			}
			cycle(t, a)
			if cached {
				cycle(t, b)
			}
			target := filepath.Join(t.TempDir(), "rules")
			if err := os.WriteFile(target, []byte("secret.txt\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			ops := journalOps(t, a)
			ops = append(ops, journal.Op{Kind: journal.KindPut, Path: IgnoreFile, Link: target,
				Device: a.Device.ID, Seq: int64(len(ops) + 1), Lamport: 100})
			var wire []byte
			for _, op := range ops {
				line, err := json.Marshal(op)
				if err != nil {
					t.Fatal(err)
				}
				wire = append(wire, line...)
				wire = append(wire, '\n')
			}
			if err := be.Put(context.Background(), "journal/deva.jsonl", strings.NewReader(string(wire)), int64(len(wire))); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cycle(t, b)
			}
			if cached {
				fi, err := os.Lstat(filepath.Join(b.Folder, IgnoreFile))
				if err != nil || !fi.Mode().IsRegular() {
					t.Fatalf("peer replaced regular ignore rules with a link: %v, %v", fi, err)
				}
				if got := read(t, b.Folder, IgnoreFile); got != "ignored/\n" {
					t.Errorf("peer link changed local rules: %q", got)
				}
			} else {
				lifecycleAssertGone(t, b, IgnoreFile)
			}
			if got := read(t, b.Folder, "secret.txt"); got != "must sync" {
				t.Errorf("peer rules suppressed data: %q", got)
			}
		})
	}
}

func TestSymlink_ForestRestoreReplacesLinkWithoutTargetPermissions(t *testing.T) {
	for _, live := range []bool{false, true} {
		t.Run(fmt.Sprint(live), func(t *testing.T) {
			s := newDevice(t, "deva", sharedRemote(t))
			sha, _, err := s.Store.PutBlobBytes([]byte("restored bytes"))
			if err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(t.TempDir(), "external")
			if live {
				if err := os.WriteFile(target, []byte("external bytes"), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			lifecycleLink(t, s, "entry", target)
			if err := s.Restore(context.Background(), "entry", sha); err != nil {
				t.Fatal(err)
			}
			fi, err := os.Lstat(filepath.Join(s.Folder, "entry"))
			if err != nil || !fi.Mode().IsRegular() {
				t.Fatalf("restore did not replace the symlink: %v, %v", fi, err)
			}
			if fi.Mode().Perm() != 0o644 {
				t.Errorf("restored mode = %o, want 644 (not target permissions)", fi.Mode().Perm())
			}
			if got := read(t, s.Folder, "entry"); got != "restored bytes" {
				t.Errorf("restored bytes = %q", got)
			}
			if live {
				got, err := os.ReadFile(target)
				if err != nil || string(got) != "external bytes" {
					t.Errorf("restore changed target: %q, %v", got, err)
				}
				targetFi, err := os.Lstat(target)
				if err != nil || targetFi.Mode().Perm() != 0o700 {
					t.Errorf("restore changed target mode: %v, %v", targetFi, err)
				}
			} else if _, err := os.Lstat(target); !os.IsNotExist(err) {
				t.Errorf("restore created missing link target: %v", err)
			}
		})
	}
}

func TestSymlink_ReadOnlyIdenticalRegularFileCreatesNoLocalCopy(t *testing.T) {
	for _, tracked := range []bool{false, true} {
		for _, mode := range []os.FileMode{0o644, 0o600} {
			t.Run(fmt.Sprintf("tracked=%t/mode=%o", tracked, mode), func(t *testing.T) {
				be := sharedRemote(t)
				a := newDevice(t, "deva", be)
				b := newDevice(t, "devb", scopedRemote{Backend: be, readOnly: []string{"locked/"}})
				write(t, a.Folder, "locked/entry", "project bytes")
				cycle(t, a)
				if tracked {
					cycle(t, b)
				} else {
					write(t, b.Folder, "locked/entry", "project bytes")
				}
				abs := filepath.Join(b.Folder, "locked/entry")
				fi, err := os.Lstat(abs)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(abs, mode); err != nil {
					t.Fatal(err)
				}
				if tracked {
					touched := fi.ModTime().Add(time.Second)
					if err := os.Chtimes(abs, touched, touched); err != nil {
						t.Fatal(err)
					}
				}
				res := cycle(t, b)
				if len(res.Reverted) != 0 {
					t.Errorf("identical bytes reported a revert: %v", res.Reverted)
				}
				entries, err := os.ReadDir(filepath.Join(b.Folder, "locked"))
				if err != nil {
					t.Fatal(err)
				}
				if len(entries) != 1 || entries[0].Name() != "entry" {
					t.Errorf("identical bytes created sidecars: %v", entries)
				}
				fi, err = os.Lstat(abs)
				if err != nil {
					t.Fatal(err)
				}
				if fi.Mode().Perm() != 0o644 {
					t.Errorf("mode = %o, want project mode 644", fi.Mode().Perm())
				}
				cache := lifecycleCache(t, b)
				c := cache["locked/entry"]
				if c.Mode != 0o644 || !cachedPathMatches(abs, fi, c) {
					t.Errorf("identical entry cache was not refreshed: %+v", c)
				}
				if got := read(t, b.Folder, "locked/entry"); got != "project bytes" {
					t.Errorf("bytes changed: %q", got)
				}
				lifecycleAssertNoReadOnlyOps(t, b)
				res = cycle(t, b)
				if len(res.Reverted) != 0 || res.Materialized != 0 {
					t.Errorf("adopted identical file did not settle: %+v", res)
				}
			})
		}
	}
}

func TestSymlink_IgnoreFIFOFailsClosed(t *testing.T) {
	folder := t.TempDir()
	abs := filepath.Join(folder, IgnoreFile)
	if err := syscall.Mkfifo(abs, 0o600); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := readRegularIgnoreFile(folder)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, store.ErrNotRegular) || os.IsNotExist(err) {
			t.Fatalf("FIFO ignore rules = %v, want a fail-closed policy error", err)
		}
	case <-time.After(5 * time.Second):
		if writer, err := os.OpenFile(abs, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			writer.Close()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
			}
		}
		t.Fatal("ignore-rule read blocked on a FIFO")
	}
}

func TestSymlink_IgnoreRegularPermissionErrorIsNotHidden(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read mode-000 regular files")
	}
	folder := t.TempDir()
	if err := os.WriteFile(filepath.Join(folder, IgnoreFile), []byte("private/\n"), 0); err != nil {
		t.Fatal(err)
	}
	if _, err := readRegularIgnoreFile(folder); !os.IsPermission(err) {
		t.Fatalf("regular ignore-file permission error = %v", err)
	}
}

func TestSymlink_DeniedRevocationPreservesLocalEditsInPlace(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", hidingRemote{Backend: be, tag: "visible"})
	write(t, a.Folder, "vault/clean.md", "clean peer content")
	write(t, a.Folder, "vault/edited.md", "original peer content")
	write(t, a.Folder, "public.md", "old public content")
	lifecycleLink(t, a, "vault/clean-link", "missing-clean")
	lifecycleLink(t, a, "vault/edited-link", "missing-original")
	cycle(t, a)
	cycle(t, b)

	// B's retained journal must resolve to an older value than its cache
	// after revocation removes A's later version from the filtered fold.
	write(t, b.Folder, "vault/own.md", "older own version")
	cycle(t, b)
	cycle(t, a)
	write(t, a.Folder, "vault/own.md", "newer peer version")
	cycle(t, a)
	cycle(t, b)
	if got := read(t, b.Folder, "vault/own.md"); got != "newer peer version" {
		t.Fatalf("fixture missed newer peer version: %q", got)
	}
	before, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}

	write(t, b.Folder, "vault/edited.md", "private local edited bytes")
	write(t, b.Folder, "vault/own.md", "private local own bytes")
	lifecycleRemove(t, b, "vault/edited-link")
	lifecycleLink(t, b, "vault/edited-link", "private-local-target")
	b.Backend = hidingRemote{Backend: be, hide: []string{"vault/"}, tag: "revoked"}
	write(t, a.Folder, "public.md", "new public content")
	cycle(t, a)

	for range 2 {
		res := cycle(t, b)
		if len(res.Reverted) != 0 {
			t.Errorf("revoked local work was reverted: %v", res.Reverted)
		}
		lifecycleAssertGone(t, b, "vault/clean.md")
		lifecycleAssertGone(t, b, "vault/clean-link")
		for rel, want := range map[string]string{
			"vault/edited.md": "private local edited bytes",
			"vault/own.md":    "private local own bytes",
		} {
			data, err := os.ReadFile(filepath.Join(b.Folder, rel))
			if err != nil || string(data) != want {
				t.Errorf("revocation disturbed %s: %q, %v", rel, data, err)
			}
		}
		target, err := os.Readlink(filepath.Join(b.Folder, "vault/edited-link"))
		if err != nil || target != "private-local-target" {
			t.Errorf("revocation disturbed dirty link: %q, %v", target, err)
		}
		_ = filepath.WalkDir(b.Folder, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				t.Error(err)
				return nil
			}
			if strings.Contains(d.Name(), "(local, not synced") {
				t.Errorf("revocation copied local work aside: %s", p)
			}
			return nil
		})
		if got := read(t, b.Folder, "public.md"); got != "new public content" {
			t.Errorf("visible project stopped syncing: %q", got)
		}
	}
	after, err := b.Store.DeviceOps(b.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("revoked paths generated journal ops: before=%+v after=%+v", before, after)
	}
	cache := lifecycleCache(t, b)
	for _, rel := range []string{"vault/clean.md", "vault/clean-link"} {
		if _, ok := cache[rel]; ok {
			t.Errorf("clean revoked peer entry remains cached: %s", rel)
		}
	}
	cycle(t, a)
	if got := read(t, a.Folder, "vault/edited.md"); got != "original peer content" {
		t.Errorf("private edit reached peer: %q", got)
	}
	if got := read(t, a.Folder, "vault/own.md"); got != "newer peer version" {
		t.Errorf("revocation changed peer's version: %q", got)
	}
	lifecycleAssertLink(t, a, "vault/edited-link", "missing-original")
}

func TestSymlink_DeniedMaterializeSkipsRetainedTargets(t *testing.T) {
	for _, wantKind := range []string{"file", "link"} {
		for _, localKind := range []string{"missing", "file", "link"} {
			t.Run(wantKind+"/"+localKind, func(t *testing.T) {
				s := newDevice(t, "devb", sharedRemote(t))
				s.denied = []string{"vault/"}
				sum, size, err := s.Store.PutBlobBytes([]byte("retained journal bytes"))
				if err != nil {
					t.Fatal(err)
				}
				want := journal.FileState{Blob: sum, Size: size, Mode: 0o644}
				if wantKind == "link" {
					want = journal.FileState{Link: "retained-journal-target"}
				}
				switch localKind {
				case "file":
					write(t, s.Folder, "vault/entry", "private local bytes")
				case "link":
					lifecycleLink(t, s, "vault/entry", "private-local-target")
				}
				cache := map[string]store.CachedFile{}
				wrote, err := s.materializeFile("vault/entry", want, cache)
				if err != nil || wrote || len(cache) != 0 || s.cacheDirty || len(s.reverted) != 0 {
					t.Errorf("denied materialize changed state: wrote=%t err=%v cache=%v dirty=%t reverted=%v", wrote, err, cache, s.cacheDirty, s.reverted)
				}
				switch localKind {
				case "missing":
					lifecycleAssertGone(t, s, "vault/entry")
				case "file":
					if got := read(t, s.Folder, "vault/entry"); got != "private local bytes" {
						t.Errorf("denied local bytes overwritten: %q", got)
					}
				case "link":
					lifecycleAssertLink(t, s, "vault/entry", "private-local-target")
				}
			})
		}
	}
}
