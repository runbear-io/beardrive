package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Round 6, internal/store: the exported functions no TestSec_* named. The
// PutBlob* family is the client's own ingest door — the third one, after the
// hub (round 2) and the archive importer (round 4) — so the question is the
// same: does what it stores hash to the key it stores it under?

func secapiStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "vol"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func secapiSum(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// A blob store is only content-addressed if every door computes the address
// from the bytes it is about to write. All three PutBlob entry points must
// agree with sha256 of what actually landed — no caller-supplied key, no
// trusting a name that came in with the content.
func TestSec_Store_EveryBlobDoorAddressesTheBytesItStored(t *testing.T) {
	s := secapiStore(t)
	body := []byte("salaries: alice 1, bob 2\n")
	want := secapiSum(body)

	file := filepath.Join(t.TempDir(), "src.txt")
	if err := os.WriteFile(file, body, 0o600); err != nil {
		t.Fatal(err)
	}

	doors := map[string]func() (string, int64, error){
		"PutBlobBytes":  func() (string, int64, error) { return s.PutBlobBytes(body) },
		"PutBlobReader": func() (string, int64, error) { return s.PutBlobReader(bytes.NewReader(body)) },
		"PutBlobFile":   func() (string, int64, error) { return s.PutBlobFile(file) },
	}
	for name, put := range doors {
		sum, n, err := put()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if sum != want {
			t.Errorf("%s returned %s for content hashing to %s", name, sum, want)
		}
		if n != int64(len(body)) {
			t.Errorf("%s returned size %d, want %d", name, n, len(body))
		}
		got, err := os.ReadFile(s.BlobPath(sum))
		if err != nil {
			t.Fatalf("%s: blob not readable at its own path: %v", name, err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("%s stored %q under the address of %q", name, got, body)
		}
		if secapiSum(got) != sum {
			t.Errorf("%s: stored bytes hash to %s, stored under %s", name, secapiSum(got), sum)
		}
	}

	// A second, different body must never be reachable under the first key —
	// dedupe is by address, so it can only ever collapse identical content.
	other := []byte("salaries: alice 9999\n")
	sum2, _, err := s.PutBlobBytes(other)
	if err != nil {
		t.Fatal(err)
	}
	if sum2 == want {
		t.Fatalf("two different bodies landed on one address")
	}
	got, err := os.ReadFile(s.BlobPath(want))
	if err != nil || !bytes.Equal(got, body) {
		t.Errorf("the first blob's content changed under it: %q (%v)", got, err)
	}
}

// Blob files ARE the private project's file contents, sitting in a 0755
// $BDRIVE_HOME on a shared machine. Round 4 hardened the journals, state cache
// and sync state; round 5 added the read spool. The blob store itself was named
// as unasserted in both gap lists and is the one that holds the actual bytes.
func TestSec_Store_BlobContentIsNotWorldReadable(t *testing.T) {
	s := secapiStore(t)
	sum, _, err := s.PutBlobBytes([]byte("board minutes\n"))
	if err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.BlobPath(sum))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("blob %s is mode %04o — readable by every account on the machine", sum, fi.Mode().Perm())
	}
}

// The session note is transient provenance ("claude-code session <id>") that
// gets stamped into every op the next scan commits. Same file class as the
// journals round 4 closed.
func TestSec_Store_SessionNoteIsNotWorldReadable(t *testing.T) {
	s := secapiStore(t)
	if err := s.SaveNote("claude-code session 7f3a-secret", time.Hour); err != nil {
		t.Fatal(err)
	}
	if got := s.LoadNote(); got != "claude-code session 7f3a-secret" {
		t.Fatalf("control: LoadNote = %q", got)
	}
	fi, err := os.Stat(s.notePath())
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		t.Errorf("note.json is mode %04o — readable by every account on the machine", fi.Mode().Perm())
	}
	if err := s.ClearNote(); err != nil {
		t.Fatal(err)
	}
	if s.LoadNote() != "" {
		t.Error("ClearNote left the note readable")
	}
}

// state-<mount>.json decides which paths the next cycle believes it is
// tracking, and its keys are joined onto the working folder by both the scan's
// delete pass and materialize's. Neither of those joins is guarded (writeFile
// is; the delete side is not), so the guard has to be here, where the keys are
// read: a cache key that is not a clean path inside the mount is not a path
// this device may act on.
func TestSec_Store_CacheKeysCannotNameAPathOutsideTheVolume(t *testing.T) {
	s := secapiStore(t)

	// Control: an ordinary cache round-trips, so the plant below is read the
	// same way a real one is.
	if err := s.SaveCache("m1", map[string]CachedFile{"notes/hello.md": {Blob: "x", Size: 2}}); err != nil {
		t.Fatal(err)
	}
	if c, err := s.LoadCache("m1"); err != nil || len(c) != 1 {
		t.Fatalf("control: LoadCache = %v, %v", c, err)
	}

	hostile := map[string]CachedFile{
		"../../../../etc/beardrive-owned": {Size: 1},
		"/etc/beardrive-absolute":         {Size: 1},
		".bdrive/config.json":             {Size: 1},
		"..":                              {Size: 1},
	}
	if err := s.SaveCache("m2", hostile); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadCache("m2")
	if err != nil {
		return // refusing to load it at all is a fine answer
	}
	for rel := range got {
		clean := filepath.ToSlash(filepath.Clean(rel))
		if rel == "" || strings.HasPrefix(rel, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			t.Errorf("LoadCache handed back %q — a key the cycle joins onto the working folder "+
				"and neither the scan's nor materialize's delete pass re-checks", rel)
		}
	}
}
