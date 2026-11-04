package syncer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

func targetedSafetyCycle(t *testing.T, s *Session, hint *ScanHint) *Result {
	t.Helper()
	res, err := s.CycleWithScanHint(context.Background(), hint)
	if err != nil {
		t.Fatal(err)
	}
	if res.Offline {
		t.Fatalf("unexpected offline cycle: %v", res.OfflineErr)
	}
	return res
}

func targetedSafetyOps(t *testing.T, s *Session) []journal.Op {
	t.Helper()
	ops, err := s.Store.DeviceOps(s.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}

func targetedSafetyAssertOps(t *testing.T, s *Session, before int, want map[string]string) {
	t.Helper()
	ops := targetedSafetyOps(t, s)
	if len(ops) < before {
		t.Fatalf("own journal shrank from %d to %d ops", before, len(ops))
	}
	ops = ops[before:]
	if len(ops) != len(want) {
		t.Errorf("new journal ops = %+v, want %v", ops, want)
	}
	seen := make(map[string]bool, len(ops))
	for _, op := range ops {
		kind, ok := want[op.Path]
		if !ok || op.Kind != kind || seen[op.Path] {
			t.Errorf("unexpected or repeated journal op: %+v; want %v", op, want)
		}
		seen[op.Path] = true
	}
	for path := range want {
		if !seen[path] {
			t.Errorf("missing journal op for %q; got %+v", path, ops)
		}
	}
}

func TestTargetedDirectoryRenameAndDeletePreservePrefixSibling(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	files := map[string]string{
		"top.txt":             "top-level document",
		"nested/deep.txt":     "nested document",
		"nested/more/end.txt": "deepest document",
	}
	for path, body := range files {
		write(t, a.Folder, "docs/"+path, body)
	}
	const sibling = "docs-old/keep.txt"
	write(t, a.Folder, sibling, "unrelated prefix sibling")
	cycle(t, a)
	cycle(t, b)

	cacheBefore, err := a.Store.LoadCache(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	siblingBefore := make([]os.FileInfo, 2)
	for i, s := range []*Session{a, b} {
		siblingBefore[i], err = os.Stat(filepath.Join(s.Folder, filepath.FromSlash(sibling)))
		if err != nil {
			t.Fatal(err)
		}
	}
	assertSibling := func() {
		t.Helper()
		cache, err := a.Store.LoadCache(a.mountID())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(cache[sibling], cacheBefore[sibling]) {
			t.Errorf("unrelated sibling cache changed: before %+v, after %+v", cacheBefore[sibling], cache[sibling])
		}
		for i, s := range []*Session{a, b} {
			if got := read(t, s.Folder, sibling); got != "unrelated prefix sibling" {
				t.Errorf("%s sibling content = %q", s.Device.ID, got)
			}
			after, err := os.Stat(filepath.Join(s.Folder, filepath.FromSlash(sibling)))
			if err != nil {
				t.Fatal(err)
			}
			if !os.SameFile(siblingBefore[i], after) || !siblingBefore[i].ModTime().Equal(after.ModTime()) {
				t.Errorf("%s rewrote the unrelated sibling", s.Device.ID)
			}
		}
	}

	before := len(targetedSafetyOps(t, a))
	if err := os.Rename(filepath.Join(a.Folder, "docs"), filepath.Join(a.Folder, "renamed")); err != nil {
		t.Fatal(err)
	}
	res := targetedSafetyCycle(t, a, &ScanHint{Events: []ScanEvent{
		{Path: "docs", Kind: ScanRename, Directory: true},
		{Path: "renamed", Kind: ScanCreate, Directory: true},
	}})
	if res.LocalOps != 2*len(files) {
		t.Errorf("rename LocalOps = %d, want %d", res.LocalOps, 2*len(files))
	}
	want := make(map[string]string, 2*len(files))
	for path := range files {
		want["docs/"+path] = journal.KindDelete
		want["renamed/"+path] = journal.KindPut
	}
	targetedSafetyAssertOps(t, a, before, want)
	cycle(t, b)
	for _, s := range []*Session{a, b} {
		cache, err := s.Store.LoadCache(s.mountID())
		if err != nil {
			t.Fatal(err)
		}
		if len(cache) != len(files)+1 {
			t.Errorf("%s rename cache has %d paths, want %d: %v", s.Device.ID, len(cache), len(files)+1, cache)
		}
		for path, body := range files {
			if _, ok := cache["docs/"+path]; ok {
				t.Errorf("%s still caches renamed descendant docs/%s", s.Device.ID, path)
			}
			if _, ok := cache["renamed/"+path]; !ok {
				t.Errorf("%s lacks renamed descendant %s in cache", s.Device.ID, path)
			}
			if got := read(t, s.Folder, "renamed/"+path); got != body {
				t.Errorf("%s renamed/%s = %q, want %q", s.Device.ID, path, got, body)
			}
			if _, err := os.Lstat(filepath.Join(s.Folder, "docs", filepath.FromSlash(path))); !os.IsNotExist(err) {
				t.Errorf("%s old descendant docs/%s survives: %v", s.Device.ID, path, err)
			}
		}
	}
	assertSibling()

	before = len(targetedSafetyOps(t, a))
	if err := os.RemoveAll(filepath.Join(a.Folder, "renamed")); err != nil {
		t.Fatal(err)
	}
	res = targetedSafetyCycle(t, a, &ScanHint{Events: []ScanEvent{
		{Path: "renamed", Kind: ScanRemove, Directory: true},
	}})
	if res.LocalOps != len(files) {
		t.Errorf("directory deletion LocalOps = %d, want %d", res.LocalOps, len(files))
	}
	want = make(map[string]string, len(files))
	for path := range files {
		want["renamed/"+path] = journal.KindDelete
	}
	targetedSafetyAssertOps(t, a, before, want)
	cycle(t, b)
	for _, s := range []*Session{a, b} {
		cache, err := s.Store.LoadCache(s.mountID())
		if err != nil {
			t.Fatal(err)
		}
		if len(cache) != 1 {
			t.Errorf("%s cache after directory deletion = %v, want only %s", s.Device.ID, cache, sibling)
		}
		for path := range files {
			if _, err := os.Lstat(filepath.Join(s.Folder, "renamed", filepath.FromSlash(path))); !os.IsNotExist(err) {
				t.Errorf("%s deleted descendant %s survives: %v", s.Device.ID, path, err)
			}
		}
	}
	assertSibling()
}

func TestTargetedLeafBelowSymlinkDoesNotJournalTargetBytes(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	// The target stays inside this disposable mount, under an excluded directory.
	// A full scan may sync the link itself, but never this synthetic payload.
	const body = "harmless synthetic payload that must not cross the symlink boundary"
	const target = ".bdrive/target-fixture"
	write(t, a.Folder, target+"/payload.txt", body)
	if err := os.Symlink(target, filepath.Join(a.Folder, "linked")); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	before := len(targetedSafetyOps(t, a))
	res := targetedSafetyCycle(t, a, &ScanHint{Events: []ScanEvent{
		{Path: "linked/payload.txt", Kind: ScanWrite},
	}})
	if res.LocalOps != 0 {
		t.Errorf("hinted leaf through a symlink journaled %d local ops", res.LocalOps)
	}
	targetedSafetyAssertOps(t, a, before, nil)
	for _, op := range targetedSafetyOps(t, a) {
		if strings.HasPrefix(op.Path, "linked/") || strings.HasPrefix(op.Path, target+"/") {
			t.Errorf("symlink target was journaled through a descendant path: %+v", op)
		}
		if op.Path == "linked" && (op.Link != target || op.Blob != "") {
			t.Errorf("link was not journaled solely as its target: %+v", op)
		}
	}
	sum := sha256.Sum256([]byte(body))
	blob := hex.EncodeToString(sum[:])
	if a.Store.HasBlob(blob) {
		t.Error("hinted leaf stored the symlink target's bytes as a local blob")
	}
	if exists, err := be.Exists(context.Background(), "blobs/"+blob); err != nil {
		t.Fatal(err)
	} else if exists {
		t.Error("hinted leaf uploaded the symlink target's bytes")
	}
	cycle(t, b)
	if got, err := os.Readlink(filepath.Join(b.Folder, "linked")); err != nil || got != target {
		t.Errorf("peer link = %q, err %v; want target %q", got, err, target)
	}
	if b.Store.HasBlob(blob) {
		t.Error("peer downloaded bytes that should exist only behind the symlink")
	}
}

// targetedSafetyPullBarrier pauses a real remote pull after local discovery has
// finished. The test changes disk only after the pull enters List, then
// releases that same cycle to materialize the peer's update.
type targetedSafetyPullBarrier struct {
	remote.Backend
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *targetedSafetyPullBarrier) wait(ctx context.Context) error {
	b.once.Do(func() { close(b.entered) })
	select {
	case <-b.release:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (b *targetedSafetyPullBarrier) List(ctx context.Context, prefix string) ([]remote.Object, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.Backend.List(ctx, prefix)
}

func (b *targetedSafetyPullBarrier) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	if err := b.wait(ctx); err != nil {
		return nil, err
	}
	return b.Backend.Get(ctx, key)
}

func targetedSafetyMutateDuringPull(t *testing.T, s *Session, mutate func()) *Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	be := s.Backend
	barrier := &targetedSafetyPullBarrier{
		Backend: be,
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	s.Backend = barrier
	type outcome struct {
		res *Result
		err error
	}
	done := make(chan outcome, 1)
	joined := make(chan struct{})
	go func() {
		defer close(joined)
		res, err := s.CycleWithScanHint(ctx, &ScanHint{})
		done <- outcome{res: res, err: err}
	}()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(barrier.release) }) }
	defer func() {
		unblock()
		cancel()
		<-joined
		s.Backend = be
	}()
	select {
	case <-barrier.entered:
	case result := <-done:
		t.Fatalf("cycle finished before the remote-pull barrier: result %+v, err %v", result.res, result.err)
	case <-ctx.Done():
		t.Fatal("cycle did not reach the remote-pull barrier")
	}
	mutate()
	unblock()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.res.Offline {
			t.Fatalf("blocked pull unexpectedly went offline: %v", result.res.OfflineErr)
		}
		return result.res
	case <-ctx.Done():
		t.Fatal("cycle did not finish after releasing the remote-pull barrier")
		return nil
	}
}

func TestEmptyHintPullPreservesWritableLocalDeletionUntilTargetedRemove(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	const path = "report.txt"
	write(t, a.Folder, path, "base")
	cycle(t, a)
	cycle(t, b)
	write(t, a.Folder, path, "a newer peer version awaiting pull")
	cycle(t, a)
	before := len(targetedSafetyOps(t, b))
	res := targetedSafetyMutateDuringPull(t, b, func() {
		if err := os.Remove(filepath.Join(b.Folder, path)); err != nil {
			t.Fatal(err)
		}
	})
	if res.LocalOps != 0 || res.PulledOps == 0 {
		t.Errorf("empty-hint pull = %+v, want peer ops without local journaling", res)
	}
	if _, err := os.Lstat(filepath.Join(b.Folder, path)); !os.IsNotExist(err) {
		t.Errorf("pull resurrected a writable file deleted after discovery: %v", err)
	}
	targetedSafetyAssertOps(t, b, before, nil)
	res = targetedSafetyCycle(t, b, &ScanHint{Events: []ScanEvent{
		{Path: path, Kind: ScanRemove},
	}})
	if res.LocalOps != 1 {
		t.Errorf("targeted Remove LocalOps = %d, want 1 deletion", res.LocalOps)
	}
	targetedSafetyAssertOps(t, b, before, map[string]string{path: journal.KindDelete})
	cycle(t, a)
	for _, s := range []*Session{a, b} {
		if _, err := os.Lstat(filepath.Join(s.Folder, path)); !os.IsNotExist(err) {
			t.Errorf("%s did not converge on the local deletion: %v", s.Device.ID, err)
		}
	}
}

func TestEmptyHintPullPreservesWritableSymlinkEditUntilTargetedWrite(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	const path = "shortcut"
	const localTarget = "local-target-written-during-pull"
	if err := os.Symlink("base-target", filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	if err := os.Remove(filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("new-peer-target", filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	before := len(targetedSafetyOps(t, b))
	res := targetedSafetyMutateDuringPull(t, b, func() {
		if err := os.Remove(filepath.Join(b.Folder, path)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(localTarget, filepath.Join(b.Folder, path)); err != nil {
			t.Fatal(err)
		}
	})
	if res.LocalOps != 0 || res.PulledOps == 0 {
		t.Errorf("empty-hint pull = %+v, want peer ops without local journaling", res)
	}
	if got, err := os.Readlink(filepath.Join(b.Folder, path)); err != nil || got != localTarget {
		t.Errorf("pull clobbered a writable symlink changed after discovery: target %q, err %v", got, err)
	}
	targetedSafetyAssertOps(t, b, before, nil)
	res = targetedSafetyCycle(t, b, &ScanHint{Events: []ScanEvent{
		{Path: path, Kind: ScanWrite},
	}})
	if res.LocalOps != 1 {
		t.Errorf("targeted Write LocalOps = %d, want 1 symlink put", res.LocalOps)
	}
	targetedSafetyAssertOps(t, b, before, map[string]string{path: journal.KindPut})
	for _, op := range targetedSafetyOps(t, b)[before:] {
		if op.Link != localTarget || op.Blob != "" {
			t.Errorf("local symlink edit was not journaled as its new target alone: %+v", op)
		}
	}
	cycle(t, a)
	for _, s := range []*Session{a, b} {
		if got, err := os.Readlink(filepath.Join(s.Folder, path)); err != nil || got != localTarget {
			t.Errorf("%s did not converge on the local symlink target: %q, err %v", s.Device.ID, got, err)
		}
	}
}

func TestEmptyHintPullMaterializesRegularToSymlinkTransition(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	const path = "shortcut"
	const target = "peer-target"
	write(t, a.Folder, path, "regular before link transition")
	cycle(t, a)
	cycle(t, b)
	before := len(targetedSafetyOps(t, b))
	if err := os.Remove(filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)

	res := targetedSafetyMutateDuringPull(t, b, func() {})
	if res.LocalOps != 0 || res.PulledOps == 0 {
		t.Fatalf("empty-hint regular-to-symlink pull = %+v, want peer op without local journaling", res)
	}
	targetedSafetyAssertOps(t, b, before, nil)
	fi, err := os.Lstat(filepath.Join(b.Folder, path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("regular-to-symlink transition left a non-link at %s: mode %v", path, fi.Mode())
	}
	if got, err := os.Readlink(filepath.Join(b.Folder, path)); err != nil || got != target {
		t.Errorf("peer symlink target = %q, err %v; want %q", got, err, target)
	}
}

func TestEmptyHintPullMaterializesDanglingSymlinkToRegularTransition(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	const path = "shortcut"
	const target = "missing-target"
	const body = "regular after dangling link transition"
	if err := os.Symlink(target, filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	cycle(t, a)
	cycle(t, b)
	before := len(targetedSafetyOps(t, b))
	if err := os.Remove(filepath.Join(a.Folder, path)); err != nil {
		t.Fatal(err)
	}
	write(t, a.Folder, path, body)
	cycle(t, a)

	res := targetedSafetyMutateDuringPull(t, b, func() {})
	if res.LocalOps != 0 || res.PulledOps == 0 {
		t.Fatalf("empty-hint dangling-symlink-to-regular pull = %+v, want peer op without local journaling", res)
	}
	targetedSafetyAssertOps(t, b, before, nil)
	if got := read(t, b.Folder, path); got != body {
		t.Errorf("peer regular content = %q, want %q", got, body)
	}
	fi, err := os.Lstat(filepath.Join(b.Folder, path))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("dangling-symlink-to-regular transition left a symlink at %s", path)
	}
}

func TestEmptyHintPullPreservesUntrackedSymlinksAtPeerRegularPaths(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	const (
		danglingPath   = "dangling-shortcut"
		danglingTarget = "missing-local-target"
		livePath       = "live-shortcut"
		liveTarget     = "local-target.txt"
		body           = "peer regular content"
	)
	write(t, a.Folder, danglingPath, body)
	write(t, a.Folder, livePath, body)
	cycle(t, a)
	peerBlob := sha(body)
	for _, key := range []string{"blobs/" + peerBlob, "journal/deva.jsonl"} {
		exists, err := be.Exists(context.Background(), key)
		if err != nil {
			t.Fatal(err)
		}
		if !exists {
			t.Fatalf("remote object %s is missing", key)
		}
	}

	res := targetedSafetyMutateDuringPull(t, b, func() {
		write(t, b.Folder, liveTarget, "local target content")
		if err := os.Symlink(danglingTarget, filepath.Join(b.Folder, danglingPath)); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(liveTarget, filepath.Join(b.Folder, livePath)); err != nil {
			t.Fatal(err)
		}
	})
	if res.LocalOps != 0 || res.PulledOps == 0 {
		t.Fatalf("empty-hint untracked-symlink pull = %+v, want peer ops without local journaling", res)
	}
	targetedSafetyAssertOps(t, b, 0, nil)

	for path, wantTarget := range map[string]string{
		danglingPath: danglingTarget,
		livePath:     liveTarget,
	} {
		abs := filepath.Join(b.Folder, filepath.FromSlash(path))
		fi, err := os.Lstat(abs)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode()&os.ModeSymlink == 0 {
			t.Errorf("%s became a regular file: mode %v", path, fi.Mode())
		}
		if got, err := os.Readlink(abs); err != nil || got != wantTarget {
			t.Errorf("%s target = %q, err %v; want %q", path, got, err, wantTarget)
		}
	}
	if _, err := os.Stat(filepath.Join(b.Folder, danglingPath)); !os.IsNotExist(err) {
		t.Errorf("dangling symlink path resolves to local content: %v", err)
	}
	if got := read(t, b.Folder, liveTarget); got != "local target content" {
		t.Errorf("non-dangling symlink target content = %q, want local target content", got)
	}

	if !b.Store.HasBlob(peerBlob) {
		t.Fatalf("peer regular blob %s was not fetched", peerBlob)
	}
	if got, err := os.ReadFile(b.Store.BlobPath(peerBlob)); err != nil {
		t.Fatal(err)
	} else if string(got) != body {
		t.Errorf("fetched peer blob = %q, want %q", got, body)
	}
	if exists, err := be.Exists(context.Background(), "blobs/"+peerBlob); err != nil {
		t.Fatal(err)
	} else if !exists {
		t.Fatalf("peer regular blob %s disappeared from remote", peerBlob)
	}
}

func TestTargetedUnknownDeletionDoesNotDeleteUnrelatedFiles(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	b := newDevice(t, "devb", be)
	files := map[string]string{
		"docs/keep.txt":     "inside docs",
		"docs-old/keep.txt": "prefix sibling",
		"other.txt":         "unrelated root file",
	}
	for path, body := range files {
		write(t, a.Folder, path, body)
	}
	cycle(t, a)
	cycle(t, b)
	cacheBefore, err := a.Store.LoadCache(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	before := len(targetedSafetyOps(t, a))
	res := targetedSafetyCycle(t, a, &ScanHint{Events: []ScanEvent{
		{Path: "doc", Kind: ScanRemove, Directory: true},
		{Path: "docs/missing", Kind: ScanRemove},
	}})
	if res.LocalOps != 0 {
		t.Errorf("unknown targeted deletion journaled %d ops", res.LocalOps)
	}
	targetedSafetyAssertOps(t, a, before, nil)
	cacheAfter, err := a.Store.LoadCache(a.mountID())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cacheBefore, cacheAfter) {
		t.Errorf("unknown deletion changed unrelated cache entries: before %v, after %v", cacheBefore, cacheAfter)
	}
	cycle(t, b)
	for _, s := range []*Session{a, b} {
		for path, body := range files {
			if got := read(t, s.Folder, path); got != body {
				t.Errorf("%s unrelated %s = %q, want %q", s.Device.ID, path, got, body)
			}
		}
	}
}

func targetedReferenceMatch(line, rel string) bool {
	line = trimRuleSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	if strings.HasPrefix(line, "!") {
		line = trimRuleSpace(line[1:])
	}
	anchored := strings.HasPrefix(line, "/")
	dirOnly := strings.HasSuffix(line, "/")
	line = strings.Trim(line, "/")
	if line == "" {
		return false
	}
	anchored = anchored || strings.Contains(line, "/")
	var b strings.Builder
	if anchored {
		b.WriteString("^")
	} else {
		b.WriteString("(^|.*/)")
	}
	for i := 0; i < len(line); i++ {
		switch line[i] {
		case '\\':
			if i+1 < len(line) {
				i++
			}
			b.WriteString(regexp.QuoteMeta(line[i : i+1]))
		case '*':
			if i+1 < len(line) && line[i+1] == '*' {
				b.WriteString(".*")
				i++
			} else {
				b.WriteString("[^/]*")
			}
		case '?':
			b.WriteString("[^/]")
		default:
			b.WriteString(regexp.QuoteMeta(line[i : i+1]))
		}
	}
	if dirOnly {
		b.WriteString("/.*$")
	} else {
		b.WriteString("(/.*)?$")
	}
	re, err := regexp.Compile(b.String())
	return err == nil && re.MatchString(rel)
}

func TestFilterFastEquivalenceAndLargeScale(t *testing.T) {
	rules := []string{
		"build/",
		"/root.txt",
		"docs/*.md",
		"deep/**/x?",
		"literal\\*",
		"trail\\ ",
		"*.tmp",
		"!keep.txt",
		"foo*",
		"foo*/",
		"bar**/baz",
	}
	patterns := compileRules(strings.Join(rules, "\n"))
	paths := []string{
		"build/main.go", "src/build/main.go", "build",
		"root.txt", "src/root.txt", "docs/readme.md", "docs/sub/readme.md",
		"deep/x1", "deep/a/x2", "deep/a/b/x3", "deep/a/b/x",
		"literal*", "literal/star", "trail ", "trail",
		"scratch.tmp", "src/scratch.tmp", "keep.txt", "src/keep.txt",
		"foo1", "sub/foo2", "notfoo", "foo_dir/file.txt",
		"sub/foo_dir/file.txt", "foo_dir", "bar/baz", "bar123/a/baz",
	}
	for i, p := range patterns {
		single := p
		single.negate = false
		for _, rel := range paths {
			got := (&Filter{ignore: []pattern{single}}).Skip(rel)
			if want := targetedReferenceMatch(rules[i], rel); got != want {
				t.Fatalf("fast match mismatch for rule %q and %q: got %v, regex %v", rules[i], rel, got, want)
			}
		}
	}
	filter := &Filter{ignore: patterns}
	accepted := strings.Join(rules, "\n")
	filter.AcceptRules(accepted)
	for _, rel := range paths {
		wantIgnored := false
		for i := len(patterns) - 1; i >= 0; i-- {
			if targetedReferenceMatch(rules[i], rel) {
				wantIgnored = !patterns[i].negate
				break
			}
		}
		if got := filter.Skip(rel); got != wantIgnored {
			t.Errorf("Skip(%q) = %v, want %v", rel, got, wantIgnored)
		}
		if got, want := filter.SkipUp(rel), wantIgnored; got != want {
			t.Errorf("SkipUp(%q) = %v, want %v", rel, got, want)
		}
	}
	floor := vouchedFloor("/*\n!keep.txt\n!never.txt\n", []string{"keep.txt"})
	if strings.Contains(floor, "!never.txt") || !strings.Contains(floor, "!keep.txt") {
		t.Fatalf("vouchedFloor = %q", floor)
	}
	pruneFilter := &Filter{ignore: compileRules("build/")}
	if !pruneFilter.PruneDir("build") || !pruneFilter.PruneDir("src/build") || pruneFilter.PruneDir("other") {
		t.Fatal("PruneDir disagrees with the reference directory rules")
	}

	benchRules := []string{
		"node_modules/", "dist/", "build/", "target/", "out/", "coverage/",
		"__pycache__/", "*.pyc", ".venv/", "venv/", ".next/", ".cache/",
		"*.mp4", "*.mov", "*.zip", "*.tar.gz", "Library/", ".DS_Store",
		"*.log", ".env.*",
	}
	benchText := strings.Join(benchRules, "\n")
	benchFilter := &Filter{ignore: compileRules(benchText)}
	benchFilter.AcceptRules(benchText)
	start := time.Now()
	for range 5000 {
		rel := "projects/searchd/pods/engagement/runs/2026-11-05-session/notes/item.md"
		_ = benchFilter.Skip(rel)
		_ = benchFilter.SkipUp(rel)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("fast filter workload took %v", elapsed)
	}
}

func TestMaterializeAndBackfillSkipFilterForConvergedCache(t *testing.T) {
	s := newDevice(t, "dev", sharedRemote(t))
	const rel = "locked/report.txt"
	write(t, s.Folder, rel, "base")
	targetedSafetyCycle(t, s, nil)
	cache, err := s.Store.LoadCache(s.mountID())
	if err != nil {
		t.Fatal(err)
	}
	target := journal.Replay(targetedSafetyOps(t, s))
	filter := &Filter{root: s.Folder}
	if picked, _ := s.backfillOps(target, cache, filter); len(picked) != 0 {
		t.Fatalf("converged backfill picked %v", picked)
	}
	before := read(t, s.Folder, rel)
	if n, err := s.materialize(target, cache, filter); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("converged materialize changed %d paths", n)
	}
	if len(filter.nestedMiss) != 0 {
		t.Fatalf("converged backfill/materialize evaluated Filter.Skip on cached path: nestedMiss=%v", filter.nestedMiss)
	}
	if got := read(t, s.Folder, rel); got != before {
		t.Fatalf("converged content changed from %q to %q", before, got)
	}

	s.readOnly = []string{"locked/"}
	write(t, s.Folder, rel, "local drift")
	if n, err := s.materialize(target, cache, filter); err != nil {
		t.Fatal(err)
	} else if n != 1 {
		t.Fatalf("read-only drift materialized %d paths, want 1", n)
	}
	if got := read(t, s.Folder, rel); got != before {
		t.Fatalf("read-only drift was not reverted: %q", got)
	}
	write(t, s.Folder, rel, "ignored local drift")
	ignored := &Filter{ignore: compileRules("locked/")}
	if n, err := s.materialize(target, cache, ignored); err != nil {
		t.Fatal(err)
	} else if n != 0 {
		t.Fatalf("ignored read-only drift materialized %d paths", n)
	}
	if got := read(t, s.Folder, rel); got != "ignored local drift" {
		t.Fatalf("ignored read-only drift changed to %q", got)
	}
}

func TestTargetedScanRecordsRetryPathsOnTransientReadFailure(t *testing.T) {
	s := newDevice(t, "dev", sharedRemote(t))
	const rel = "retry.txt"
	write(t, s.Folder, rel, "first")
	targetedSafetyCycle(t, s, nil)
	write(t, s.Folder, rel, "second content")
	abs := filepath.Join(s.Folder, filepath.FromSlash(rel))
	if err := os.Chmod(abs, 0); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(abs, 0o644)
	if _, err := os.ReadFile(abs); err == nil {
		t.Skip("filesystem permits reading mode-000 files")
	}
	res := targetedSafetyCycle(t, s, &ScanHint{Events: []ScanEvent{{Path: rel, Kind: ScanWrite}}})
	found := false
	for _, got := range res.RetryPaths {
		if got == rel {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("RetryPaths = %v, want %q", res.RetryPaths, rel)
	}
}
