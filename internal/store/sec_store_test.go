package store

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Security tests for the local volume store. The store's inputs are not all
// local: a blob key is `Op.Blob` copied verbatim out of a journal a peer
// pushed, and a mount id is read verbatim out of a folder's
// .bdrive/config.json — a file that travels with the folder. Both reach a
// filesystem path here.

// secpkgStore opens a store under a fresh temp dir and returns (store, parent
// dir). The parent holds the "secret" a path escape is trying to reach.
func secpkgStore(t *testing.T) (*Store, string) {
	t.Helper()
	parent := t.TempDir()
	s, err := Open(filepath.Join(parent, "vol"))
	if err != nil {
		t.Fatal(err)
	}
	return s, parent
}

// secpkgUnder reports whether path is inside root.
func secpkgUnder(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// TestSec_Store_BlobKeyCannotEscapeTheBlobDir: BlobPath does
// filepath.Join(dir, "blobs", sum[:2], sum) with no check that sum is a
// sha256. A journal op's Blob field is whatever a peer wrote, so
// `"blob":"../secret.txt"` makes HasBlob answer true for a file outside the
// store and OpenBlob hand back its contents — which syncer.writeFile then
// copies into the working folder as the file's content.
func TestSec_Store_BlobKeyCannotEscapeTheBlobDir(t *testing.T) {
	s, parent := secpkgStore(t)
	secret := "device token: s3cr3t-not-a-blob"
	if err := os.WriteFile(filepath.Join(parent, "secret.txt"), []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	blobs := filepath.Join(s.Dir(), "blobs")
	for _, hostile := range []string{
		"../secret.txt",
		"../../" + filepath.Base(parent) + "/secret.txt",
		"..%2fsecret.txt",
		strings.Repeat("../", 12) + "etc/passwd",
	} {
		t.Run(hostile, func(t *testing.T) {
			if p := s.BlobPath(hostile); !secpkgUnder(blobs, p) {
				t.Errorf("BlobPath(%q) = %q, which is outside the blob dir %q", hostile, p, blobs)
			}
			if s.HasBlob(hostile) {
				t.Errorf("HasBlob(%q) = true: a non-sha key must never name an existing blob", hostile)
			}
			f, err := s.OpenBlob(hostile)
			if err == nil {
				b, _ := io.ReadAll(io.LimitReader(f, 60))
				f.Close()
				t.Errorf("OpenBlob(%q) succeeded and returned %q…", hostile, string(b))
			}
		})
	}
}

// TestSec_Store_ShortBlobKeyIsRefusedNotFatal: BlobPath slices sum[:2]
// unconditionally. An op with a one-character (or empty) Blob panics the
// process — and the daemon runs this loop unattended.
func TestSec_Store_ShortBlobKeyIsRefusedNotFatal(t *testing.T) {
	s, _ := secpkgStore(t)
	for _, sum := range []string{"", "a"} {
		t.Run("len"+string(rune('0'+len(sum))), func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("BlobPath(%q) panicked: %v", sum, r)
				}
			}()
			_ = s.BlobPath(sum)
			_ = s.HasBlob(sum)
			if f, err := s.OpenBlob(sum); err == nil {
				f.Close()
				t.Errorf("OpenBlob(%q) succeeded", sum)
			}
		})
	}
}

// TestSec_Store_MountIdCannotEscapeTheVolumeDir: cachePath builds
// "state-"+mountID+".json" and Joins it onto the volume dir. mountID is
// Project.ID, read verbatim from a folder's .bdrive/config.json — a file that
// arrives with the folder (a zip, a clone, a colleague's copy). A crafted id
// writes the materialization cache anywhere the user can write.
func TestSec_Store_MountIdCannotEscapeTheVolumeDir(t *testing.T) {
	s, parent := secpkgStore(t)
	for _, id := range []string{
		"/../../pwn",
		"/../pwn",
		"x/../../../pwn",
	} {
		t.Run(id, func(t *testing.T) {
			if err := s.SaveCache(id, map[string]CachedFile{"a.md": {Blob: "deadbeef"}}); err != nil {
				return // refusing is the secure outcome
			}
			// It wrote something. Prove it landed inside the volume store.
			var strays []string
			err := filepath.WalkDir(parent, func(p string, _ os.DirEntry, err error) error {
				if err != nil {
					return nil
				}
				if strings.HasSuffix(p, ".json") && !secpkgUnder(s.Dir(), p) {
					strays = append(strays, p)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(strays) > 0 {
				t.Errorf("SaveCache(%q) wrote outside the volume store: %v", id, strays)
			}
		})
	}
}

// TestSec_Store_AtomicWriteDoesNotFollowASymlinkAtTheDestination asserts the
// refused case: WriteFileAtomic renames over the destination name, so a
// symlink planted there is replaced, not written through.
func TestSec_Store_AtomicWriteDoesNotFollowASymlinkAtTheDestination(t *testing.T) {
	dir := t.TempDir()
	outside := filepath.Join(dir, "outside.txt")
	if err := os.WriteFile(outside, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, "state.json")
	if err := os.Symlink(outside, dst); err != nil {
		t.Skip("symlinks unavailable")
	}
	if err := WriteFileAtomic(dst, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "original" {
		t.Errorf("WriteFileAtomic wrote through the symlink: outside file is now %q", b)
	}
}

// TestSec_Store_VolumeJournalsAreNotWorldReadable: the volume store lives in
// $BDRIVE_HOME, whose directories are 0755. journal.Append opens the journal
// 0644 and WriteFileAtomic chmods state files 0644, so every local account can
// read the full path list, authorship and signed-in email addresses of a
// private project.
func TestSec_Store_VolumeJournalsAreNotWorldReadable(t *testing.T) {
	s, _ := secpkgStore(t)
	if err := s.SaveSync(SyncState{Lamport: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveCache("m-1234abcd", map[string]CachedFile{"secret-project/plan.md": {Blob: "ab"}}); err != nil {
		t.Fatal(err)
	}
	ops := []journal.Op{{
		Seq: 1, Lamport: 1, Time: time.Now().UTC(), Device: "dev-1",
		Kind: journal.KindPut, Path: "secret-project/plan.md",
		User: "alice@acme.example", Blob: strings.Repeat("ab", 32),
	}}
	if err := s.AppendOps("dev-1", ops); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{
		s.JournalPath("dev-1"),
		filepath.Join(s.Dir(), "state-m-1234abcd.json"),
		filepath.Join(s.Dir(), "sync.json"),
	} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is mode %04o: every local account can read this project's paths and authorship", p, fi.Mode().Perm())
		}
	}
}
