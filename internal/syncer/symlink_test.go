package syncer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

/* Symlinks used to be dropped at the door.

   walkFolder skipped anything that was not a regular file, so a symlink in a
   mount was never journaled: invisible on the hub, absent on every peer, and
   silently so — the scan reported nothing. The journal had no field for a
   link target, so there was nothing a scan could have recorded even if it
   had wanted to.

   A symlink is now an op like any other, carrying its target string and no
   blob. Materialize recreates it on peers as a symlink. The TARGET is never
   resolved by anything in this package: a link that points outside the mount
   is a perfectly ordinary op, and the only thing that ever follows it is the
   user's own filesystem, exactly as before. */

func TestSymlinkSyncsAsASymlink(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)
	write(t, a.Folder, "docs/real.md", "the real file\n")
	if err := os.Symlink("real.md", filepath.Join(a.Folder, "docs", "alias.md")); err != nil {
		t.Fatal(err)
	}
	res := cycle(t, a)
	if res.LocalOps != 2 {
		t.Fatalf("expected 2 ops (the file and the link), got %+v", res)
	}

	b := newDevice(t, "devb", be)
	cycle(t, b)
	abs := filepath.Join(b.Folder, "docs", "alias.md")
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatalf("the symlink never arrived on the peer: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("arrived as a regular file, not a symlink: mode %v — the link was flattened", fi.Mode())
	}
	target, err := os.Readlink(abs)
	if err != nil {
		t.Fatal(err)
	}
	if target != "real.md" {
		t.Fatalf("target = %q, want %q", target, "real.md")
	}
	// And through the link the peer reads the real file, because the peer's
	// own filesystem resolves it — not because sync copied any bytes for it.
	if got := read(t, b.Folder, "docs/alias.md"); got != "the real file\n" {
		t.Fatalf("reading through the link on the peer: %q", got)
	}
}

// A target outside the mount syncs as a LINK, never as its content. Copying
// the bytes would ship whatever the link pointed at on the author's machine
// (~/.ssh/id_rsa is one Symlink call away) to every peer, as a file.
func TestSymlinkOutsideTheMountSyncsAsALinkNotItsContent(t *testing.T) {
	// A real file:// remote, whose backing dir this test scans by hand: on one
	// machine both devices share a filesystem, so "can the peer read through
	// the link" cannot tell a leak from the peer's own disk resolving the same
	// absolute path. The wire is the remote store — so the leak test is whether
	// the secret bytes ever landed there.
	remoteDir := t.TempDir()
	be, err := remote.Open(context.Background(), "file://"+remoteDir)
	if err != nil {
		t.Fatal(err)
	}
	a := newDevice(t, "deva", be)
	const secretBody = "NOT-FOR-SYNC\n"
	secret := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(secret, []byte(secretBody), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, filepath.Join(a.Folder, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	res := cycle(t, a)
	if res.LocalOps != 1 {
		t.Fatalf("expected 1 op (the link), got %+v", res)
	}
	for _, op := range journalOps(t, a) {
		if op.Path == "escape.txt" && op.Blob != "" {
			t.Fatalf("the link's TARGET CONTENT was journaled as blob %s — a symlink to a "+
				"secret would have shipped the secret to every peer", op.Blob[:12])
		}
	}
	// The secret bytes must appear nowhere in the remote store — not as a blob,
	// not inside any journal line.
	filepath.WalkDir(remoteDir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if data, err := os.ReadFile(p); err == nil && strings.Contains(string(data), secretBody) {
			t.Fatalf("the target's content crossed the wire: found it in the remote store at %s", p)
		}
		return nil
	})

	b := newDevice(t, "devb", be)
	cycle(t, b)
	abs := filepath.Join(b.Folder, "escape.txt")
	fi, err := os.Lstat(abs)
	if err != nil {
		t.Fatalf("the link never arrived: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("arrived as a regular file: the target's bytes were copied across")
	}
	if target, err := os.Readlink(abs); err != nil || target != secret {
		t.Fatalf("the link was not reproduced verbatim: target %q err %v", target, err)
	}
}

// Replay must carry the target: a FileState with no Link is a link that
// materialize cannot recreate.
func TestReplayCarriesTheLinkTarget(t *testing.T) {
	ops := []journal.Op{{Seq: 1, Lamport: 1, Device: "d", Kind: journal.KindPut, Path: "a", Link: "b"}}
	st := journal.Replay(ops)
	if st["a"].Link != "b" {
		t.Fatalf("Replay dropped the link target: %+v", st["a"])
	}
}

// journalOps reads back every op this device has committed.
func journalOps(t *testing.T, s *Session) []journal.Op {
	t.Helper()
	data, err := os.ReadFile(s.Store.JournalPath(s.Device.ID))
	if err != nil {
		t.Fatal(err)
	}
	ops, err := journal.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return ops
}
