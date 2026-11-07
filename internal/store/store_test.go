package store

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

func TestBlobRoundtrip(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	sum, n, err := s.PutBlobBytes([]byte("hello beardrive"))
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len("hello beardrive")) {
		t.Fatalf("size = %d, want %d", n, len("hello beardrive"))
	}
	if !s.HasBlob(sum) {
		t.Fatal("blob not stored")
	}
	// dedupe: same content, same sum, no error
	sum2, _, err := s.PutBlobBytes([]byte("hello beardrive"))
	if err != nil || sum2 != sum {
		t.Fatalf("dedupe failed: %v %v", sum2, err)
	}
	f, err := s.OpenBlob(sum)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	data := make([]byte, 16)
	k, _ := f.Read(data)
	if string(data[:k]) != "hello beardrive" {
		t.Fatalf("content mismatch: %q", data[:k])
	}
}

func TestJournalAndState(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ops := []journal.Op{{Seq: 1, Lamport: 1, Device: "devA", Kind: journal.KindPut, Path: "f", Blob: "b"}}
	if err := s.AppendOps("devA", ops); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOps("devB", []journal.Op{{Seq: 1, Lamport: 2, Device: "devB", Kind: journal.KindDelete, Path: "f"}}); err != nil {
		t.Fatal(err)
	}
	all, err := s.AllOps()
	if err != nil || len(all) != 2 {
		t.Fatalf("AllOps = %v, %v", all, err)
	}
	devs, _ := s.Devices()
	if len(devs) != 2 {
		t.Fatalf("Devices = %v", devs)
	}

	cache := map[string]CachedFile{"f": {Blob: "b", Size: 1, MTimeNS: 42}}
	if err := s.SaveCache("m1", cache); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCache("m1")
	if err != nil || got["f"].MTimeNS != 42 {
		t.Fatalf("cache roundtrip: %v %v", got, err)
	}
	// caches are isolated per mount
	other, err := s.LoadCache("m2")
	if err != nil || len(other) != 0 {
		t.Fatalf("mount caches must be isolated: %v %v", other, err)
	}

	// ReadOnly is here so the round trip covers the slice field too: a
	// SyncState holding a slice can no longer be compared with !=, and the
	// scope a device is holding is exactly the field a silent drop would
	// widen back to "everything is writable".
	st := SyncState{Lamport: 7, PushedOps: 3, ReadOnly: []string{"locked/"}, ScopeTag: "t1"}
	if err := s.SaveSync(st); err != nil {
		t.Fatal(err)
	}
	gotSt, err := s.LoadSync()
	if err != nil || !reflect.DeepEqual(gotSt, st) {
		t.Fatalf("sync state roundtrip: %v %v", gotSt, err)
	}
}

func TestReplayFoldMatchesReplay(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	opsA := []journal.Op{
		{Seq: 1, Lamport: 1, Device: "actual-a", Kind: journal.KindPut, Path: "same", Blob: "not-hex"},
		{Seq: 2, Lamport: 3, Device: "actual-a", Kind: journal.KindDelete, Path: "gone"},
	}
	opsB := []journal.Op{
		{Seq: 1, Lamport: 2, Device: "actual-b", Kind: journal.KindPut, Path: "same", Blob: strings.Repeat("A", 64)},
		{Seq: 2, Lamport: 3, Device: "actual-b", Kind: journal.KindPut, Path: "link", Link: "target"},
	}
	if err := s.AppendOps("file-a", opsA); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendOps("file-b", opsB); err != nil {
		t.Fatal(err)
	}
	all, err := s.AllOps()
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.ReplayFold()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Target, journal.Replay(all)) {
		t.Fatalf("fold target = %#v, replay = %#v", got.Target, journal.Replay(all))
	}
	s.SaveChangedFoldsLocked()
	fresh, err := Open(filepath.Join(t.TempDir(), "fresh"))
	if err != nil {
		t.Fatal(err)
	}
	for _, dev := range []string{"file-a", "file-b"} {
		data, rerr := os.ReadFile(s.JournalPath(dev))
		if rerr != nil {
			t.Fatal(rerr)
		}
		if werr := os.WriteFile(fresh.JournalPath(dev), data, 0o600); werr != nil {
			t.Fatal(werr)
		}
	}
	again, err := fresh.ReplayFold()
	if err != nil || !reflect.DeepEqual(again.Target, got.Target) {
		t.Fatalf("fresh fold = %#v, %v", again.Target, err)
	}
}

func TestSaveCacheStripsForeignKeys(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCache("m", map[string]CachedFile{
		"ok/file":     {Blob: "ok"},
		"../secret":   {Blob: "bad"},
		"/etc/passwd": {Blob: "bad"},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCache("m")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got["ok/file"].Blob != "ok" {
		t.Fatalf("cache = %#v", got)
	}
}

func TestLock(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	if err := unlock(); err != nil {
		t.Fatal(err)
	}
	// re-acquirable after unlock
	unlock2, err := s.Lock()
	if err != nil {
		t.Fatal(err)
	}
	unlock2()
}

func TestWriteFileAtomic(t *testing.T) {
	p := t.TempDir() + "/x.json"
	if err := WriteFileAtomic(p, []byte("data"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "data" {
		t.Fatalf("got %q %v", b, err)
	}
}
