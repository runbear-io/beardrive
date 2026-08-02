package syncer

// Round 14 — attacking round 13's `Filter.SkipUp` asymmetry.
//
// The rule the fix shipped, in its own words (ignore.go):
//
//	"A pulled rule may narrow what this device uploads. It may not widen it. A
//	 widening takes effect when somebody at this machine authors the rules."
//
// Authorship is decided by two strings in store.SyncState — IgnoreAccepted and
// IgnorePulled — compared once at the top of Cycle:
//
//	if text := string(cur); text != st.IgnorePulled { st.IgnoreAccepted = text }
//
// So everything rests on IgnorePulled being an accurate record of "what a peer
// last wrote here". It is written in exactly ONE place (Cycle step 4), behind
// TWO conditions: the merged state must still CONTAIN .bdriveignore, and this
// cycle must have pulled at least one op. Every other way a peer's edit reaches
// this disk leaves the pair inconsistent, and the next cycle then reads a
// peer's rules as locally authored.
//
// Both tests below are the round-13 scenario with one thing changed, and both
// assert the SECURE behaviour: the victim's local `.env` never leaves the
// machine because somebody else edited a synced file.

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
	"github.com/runbear-io/beardrive/internal/remote"
)

// secfx13Paths lists every path any device has published to the shared remote,
// read back out of the journals the way a hub operator would. Copied in shape
// from TestSec_Onboard_PeerIgnoreRulesCannotWidenAnotherMembersScope's onHub so
// the two tests measure the same thing.
func secfx13Paths(t *testing.T, be remote.Backend) []string {
	t.Helper()
	keys, err := be.List(context.Background(), "journal/")
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, k := range keys {
		rc, err := be.Get(context.Background(), k.Key)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
		ops, err := journal.Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range ops {
			paths = append(paths, op.Path)
		}
	}
	return paths
}

func secfx13Has(paths []string, want string) bool {
	for _, p := range paths {
		if p == want {
			return true
		}
	}
	return false
}

// secfx13Cycle runs one cycle and fails on error. It deliberately does NOT
// assert !res.Offline the way cycle() does: these tests care about what leaves
// the disk, not about the remote leg's mood.
func secfx13Cycle(t *testing.T, s *Session) *Result {
	t.Helper()
	res, err := s.Cycle(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// TestSec_Scope_APeerDeletingTheSharedIgnoreFileCannotWidenAnotherMembersScope.
//
// The mount is a whole repository, narrowed by `.bdriveignore`, which is the
// arrangement INSTALL_FOR_AGENTS.md calls "the one sanctioned way to sync
// inside a repo". `.bdriveignore` is a synced file, so a teammate can change
// it — and round 13 decided a teammate's change may narrow but never widen.
//
// Deleting the file is the maximal widening there is: every rule at once. And
// it walks straight past the guard, because the guard is written in terms of a
// file that still exists:
//
//	if want, ok := target[IgnoreFile]; ok && len(pulled) > 0 {   // syncer.go
//	        ... st.IgnorePulled = string(cur)
//
// A delete removes IgnoreFile from `target`, so that block never runs and
// IgnorePulled keeps naming the text the peer wrote LAST time. materialize's
// delete loop then unlinks the local copy anyway. On the next cycle the top of
// Cycle reads "" off a file that is gone, compares it against a stale non-empty
// IgnorePulled, concludes somebody at this machine emptied the rules, and sets
// IgnoreAccepted = "". The floor is gone, the live rules are gone with it, and
// the whole repository — `.env` included — is scanned and pushed.
//
// Control: with the rules in force `.env` stays off the hub while a docs file
// goes up, so the filter and the rig both work before anything is attacked.
func TestSec_Scope_APeerDeletingTheSharedIgnoreFileCannotWidenAnotherMembersScope(t *testing.T) {
	be := sharedRemote(t)
	victim := newDevice(t, "victim", be)
	peer := newDevice(t, "peer", be)

	// The victim mounts their repo root, narrowed the way the runbook says, and
	// AUTHORS the rules themselves — so this device has accepted them and the
	// round-13 floor is genuinely in place.
	const rulesV1 = "node_modules/\n.env\n.env.*\n"
	write(t, victim.Folder, IgnoreFile, rulesV1)
	write(t, victim.Folder, "docs/readme.md", "hello")
	write(t, victim.Folder, ".env", "AWS_SECRET_ACCESS_KEY=REAL-PRODUCTION-SECRET")
	secfx13Cycle(t, victim)

	if secfx13Has(secfx13Paths(t, be), ".env") {
		t.Fatal("control: .env reached the hub with the seeded rule in force — " +
			"the filter is not working, so nothing below would mean anything")
	}
	if !secfx13Has(secfx13Paths(t, be), "docs/readme.md") {
		t.Fatalf("control: the victim never pushed anything; hub holds %v", secfx13Paths(t, be))
	}

	// A teammate joins and NARROWS the shared rules — the direction round 13
	// explicitly still allows, and the step that makes the victim's IgnorePulled
	// non-empty. Nothing hostile has happened yet.
	secfx13Cycle(t, peer)
	write(t, peer.Folder, IgnoreFile, rulesV1+"build/\n")
	secfx13Cycle(t, peer)
	secfx13Cycle(t, victim) // victim materializes the narrowed rules

	if got := read(t, victim.Folder, IgnoreFile); got != rulesV1+"build/\n" {
		t.Fatalf("fixture: the victim did not receive the peer's narrowed rules, has %q", got)
	}

	// Now the teammate deletes the shared file. One `rm`, in a file the product
	// asks every member to keep in the folder.
	if err := os.Remove(filepath.Join(peer.Folder, IgnoreFile)); err != nil {
		t.Fatal(err)
	}
	secfx13Cycle(t, peer)

	// The victim keeps syncing. Nothing on their machine changed and nobody
	// asked them anything. Once to receive the delete, once to act on it (scan
	// runs before pull).
	for i := 0; i < 2; i++ {
		secfx13Cycle(t, victim)
	}

	if secfx13Has(secfx13Paths(t, be), ".env") {
		st, _ := victim.Store.LoadSync()
		t.Fatalf("a peer DELETED the shared %s and the victim's next cycle uploaded their "+
			"local .env to the hub — a file that had never been shared, with no prompt and "+
			"no local change.\n"+
			"Round 13's asymmetry (\"a pulled rule may narrow what this device uploads, it "+
			"may not widen it\") is written in terms of a file that still EXISTS: the "+
			"IgnorePulled bookkeeping sits behind `if want, ok := target[IgnoreFile]; ok && "+
			"len(pulled) > 0`, so a delete never updates it, and the next cycle reads the "+
			"now-absent file as \"\" against a stale IgnorePulled and calls the widening "+
			"locally authored.\n"+
			"IgnoreAccepted is now %q, IgnorePulled %q.\nhub journal now names: %v",
			IgnoreFile, st.IgnoreAccepted, st.IgnorePulled, secfx13Paths(t, be))
	}
}

// TestSec_Scope_AnUpgradedDeviceDoesNotAdoptAPeersWideningAsItsOwn.
//
// The upgrade path. IgnoreAccepted and IgnorePulled are new fields with
// `omitempty`, so every store.SyncState written by any earlier binary carries
// both as "". On the first cycle after the upgrade the accept test is
//
//	text != st.IgnorePulled     →     <whatever is on disk> != ""
//
// which is true for every device that has a `.bdriveignore` at all. So the
// rules sitting on disk at upgrade time are adopted wholesale as
// locally-authored — including a peer's `!` line that arrived over the wire and
// that this device's user has never seen, which is the exact rule round 13
// exists to refuse.
//
// The window is not theoretical: scan runs BEFORE pull, so a peer's widening
// lands on disk in cycle N and only takes effect in cycle N+1. A device that is
// stopped between those two cycles (`bdrive stop`, a laptop lid, a reboot) and
// upgraded before it next runs is exactly this device — the fix is inert in the
// one window it was written for.
//
// The state is constructed by hand rather than by running an old binary,
// because "a SyncState with neither field set" is the only thing about the old
// binary that matters here, and it is a state the current code must survive
// whatever wrote it.
//
// Control: the same device, the same peer rules, with the fields present —
// `.env` stays put, so the test is measuring the legacy row and not the rules.
func TestSec_Scope_AnUpgradedDeviceDoesNotAdoptAPeersWideningAsItsOwn(t *testing.T) {
	const rulesV1 = "node_modules/\n.env\n.env.*\n"
	const widened = rulesV1 + "!.env\n"

	// run drives the whole scenario and returns whether .env reached the hub.
	// legacy says whether the victim's SyncState is blanked (an upgrade) before
	// the cycle that acts on the peer's widened rules.
	run := func(t *testing.T, legacy bool) bool {
		t.Helper()
		be := sharedRemote(t)
		victim := newDevice(t, "victim", be)
		peer := newDevice(t, "peer", be)

		write(t, victim.Folder, IgnoreFile, rulesV1)
		write(t, victim.Folder, "docs/readme.md", "hello")
		write(t, victim.Folder, ".env", "AWS_SECRET_ACCESS_KEY=REAL-PRODUCTION-SECRET")
		secfx13Cycle(t, victim)
		if secfx13Has(secfx13Paths(t, be), ".env") {
			t.Fatal("control: .env reached the hub with the seeded rule in force")
		}

		// The peer widens the shared rules.
		secfx13Cycle(t, peer)
		write(t, peer.Folder, IgnoreFile, widened)
		secfx13Cycle(t, peer)

		// The victim RECEIVES them (scan ran first, so nothing has acted on them
		// yet) and is then stopped.
		secfx13Cycle(t, victim)
		if got := read(t, victim.Folder, IgnoreFile); got != widened {
			t.Fatalf("fixture: the victim did not receive the peer's rules, has %q", got)
		}

		if legacy {
			// The upgrade: a SyncState written before either field existed.
			// Everything else about the volume is untouched.
			st, err := victim.Store.LoadSync()
			if err != nil {
				t.Fatal(err)
			}
			st.IgnoreAccepted, st.IgnorePulled = "", ""
			if err := victim.Store.SaveSync(st); err != nil {
				t.Fatal(err)
			}
		}

		// The first cycle on the new binary.
		secfx13Cycle(t, victim)
		return secfx13Has(secfx13Paths(t, be), ".env")
	}

	t.Run("control_fields_present", func(t *testing.T) {
		if run(t, false) {
			t.Fatal("control: with IgnoreAccepted/IgnorePulled present the peer's `!.env` " +
				"already widens this device's scan — round 13's fix is not holding at all, " +
				"so the legacy case below proves nothing")
		}
	})

	t.Run("legacy_syncstate", func(t *testing.T) {
		if run(t, true) {
			t.Fatal("a device whose store.SyncState predates IgnoreAccepted/IgnorePulled " +
				"adopts whatever `.bdriveignore` is on disk at its first post-upgrade cycle " +
				"as locally authored — including a peer's `!.env` that arrived over the wire " +
				"and that nobody at this machine has ever seen. Both fields are `omitempty` " +
				"and every earlier binary wrote neither, so `text != st.IgnorePulled` is true " +
				"for every existing device on the day it upgrades. The upload floor round 13 " +
				"added is inert exactly where it was needed: scan runs before pull, so the " +
				"peer's widening always sits on disk for one full cycle before it can act, " +
				"and a device stopped in that window and upgraded is this one.")
		}
	})
}
