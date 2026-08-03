package store

// Round 9, row 17 (internal/store) — replacement tests for guards that
// survived a hand reversion with the WHOLE TestSec suite green. Each one is
// written so that only the guard under test can produce the refusal.
//
// Helpers are prefixed secaud4; secpkgStore (sec_store_test.go) and
// secapiStore (sec_api_test.go) are reused.

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// TestSec_Store_HasBlobRefusesANonShaKeyOnItsOwn.
//
// BlobPath collapses every key that is not 64 hex characters onto one name
// inside the blob dir — "blobs/invalid" — on the stated premise that
// PutBlobReader never writes it, so HasBlob and OpenBlob "fail naturally".
// That premise made HasBlob's OWN check invisible to every existing test: the
// fallback name never exists in a fixture, so deleting the check changed no
// answer anywhere in the suite.
//
// It is reachable in the threat model this package already reasons about. The
// volume store lives in $BDRIVE_HOME, and LoadCache's own comment says the
// files there are chosen by "anything running as the user (an agent session,
// an install script, an older bdrive)". One file at blobs/invalid re-arms the
// whole class round 4 closed:
//
//	syncer.pull:         if ... || s.Store.HasBlob(op.Blob) { continue }
//	syncer.materializeFile: if !s.Store.HasBlob(want.Blob) { skip }
//
// so a peer op carrying any non-sha Blob reads as content already held — the
// fetch AND the `sum != op.Blob` content-address check behind it are both
// skipped — and writeFile then copies the planted bytes into the working
// folder as that path's content, on every device that pulls the line.
func TestSec_Store_HasBlobRefusesANonShaKeyOnItsOwn(t *testing.T) {
	s, parent := secpkgStore(t)

	// Control: a real blob is found under its own address, so a `false` below
	// means the guard refused rather than the fixture being empty.
	sum, _, err := s.PutBlobBytes([]byte("real content\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !s.HasBlob(sum) {
		t.Fatalf("control: HasBlob(%s) = false for a blob this store just wrote", sum)
	}

	const secret = "device token: s3cr3t-outside-the-store"
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	// The name BlobPath resolves every invalid key to. Nothing in the product
	// writes it; anything running as the user can.
	if err := os.WriteFile(filepath.Join(s.Dir(), "blobs", "invalid"), []byte("planted bytes"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, hostile := range []string{
		"../secret.txt",
		"../../" + filepath.Base(parent) + "/secret.txt",
		"",
		"a",
		strings.Repeat("a", 63), // one short of a sha256
		strings.Repeat("a", 65), // one long
		strings.ToUpper(sum),    // right bytes, wrong case
		sum[:63] + "g",          // right length, not hex
	} {
		t.Run(hostile, func(t *testing.T) {
			if s.HasBlob(hostile) {
				t.Errorf("HasBlob(%q) = true. A key that is not a sha256 must be refused by "+
					"HasBlob itself, not by the accident that BlobPath's fallback name happens "+
					"to have no file: pull skips both the fetch and the content-address check "+
					"for a blob it believes it already holds, and materialize then writes "+
					"whatever sits at that path into the working folder", hostile)
			}
			// OpenBlob has no check of its own — every risky caller is gated
			// on HasBlob above. What must hold for it is containment: a key
			// that is not a sha256 must never resolve to a file outside the
			// blob dir, whatever it spells.
			f, err := s.OpenBlob(hostile)
			if err != nil {
				return
			}
			defer f.Close()
			b := make([]byte, len(secret))
			n, _ := f.Read(b)
			if string(b[:n]) == secret {
				t.Errorf("OpenBlob(%q) handed back the contents of a file outside the volume "+
					"store, which writeFile copies into the working folder as that path's "+
					"content", hostile)
			}
		})
	}
}

// TestSec_Store_TheVolumeStoreDirectoriesAreNotGroupOrWorldWritable.
//
// Rounds 4-6 closed the FILE modes in the volume store one call site at a time,
// on the grounds that other local accounts must not read a private project's
// paths, authorship and content. Nothing asserts the mode of the DIRECTORIES
// those files sit in, and a directory's write bit is the stronger permission:
// with blobs/ group- or world-writable another local account replaces the
// content behind any address it can name (materialize opens the blob by path
// and does not re-hash it), and with journal/ writable it replaces a cached
// peer journal outright — both landing in every teammate's working folder on
// the next cycle, under this device's identity and signature.
//
// The umask is dropped for the duration on purpose. Under the usual 022 the
// permission bits Open asks MkdirAll for are masked, so the literal in the
// source is not what decides the mode and widening it changes nothing an
// ordinary test can see — the protection is the operator's umask, not the
// code's. A daemon started from a context with a permissive umask (a launchd
// job, a container entrypoint, a CI runner) gets exactly what Open asks for,
// so that is the condition this asserts under. Not parallel: umask is
// process-wide.
func TestSec_Store_TheVolumeStoreDirectoriesAreNotGroupOrWorldWritable(t *testing.T) {
	old := syscall.Umask(0)
	t.Cleanup(func() { syscall.Umask(old) })

	s, _ := secpkgStore(t)
	if _, _, err := s.PutBlobBytes([]byte("board minutes\n")); err != nil {
		t.Fatal(err)
	}
	seen := 0
	err := filepath.Walk(s.Dir(), func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.IsDir() {
			return nil
		}
		seen++
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s is mode %04o — another local account can add, replace or unlink the "+
				"blobs and journals this device materializes into the working folder",
				p, fi.Mode().Perm())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if seen < 4 { // the volume dir plus blobs/, journal/, tmp/
		t.Fatalf("fixture wrong: only %d directories under %s", seen, s.Dir())
	}
}
