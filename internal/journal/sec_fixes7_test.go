package journal

// Round 8 — journal.SafePath, round 7's collapse of three path rules into one
// exported predicate. It is now the single point of failure guarding the hub's
// journal door (webapp.journalOps), the hub's browser door (the core of
// webapp.cleanUploadPath), the device door (syncer.unsafeRel) and the template
// door (templates.SafePath), so it is worth attacking on its own terms rather
// than only through the four callers.
//
// Helpers are prefixed secfx7.

import (
	"path"
	"path/filepath"
	"strings"
	"testing"
)

// TestSec_Path_SafePathRefusesEveryEscapeAnOpCouldName is the exhaustive
// refusal table. Every entry is a spelling that must never become an Op.Path:
// the empty and self-referential ones, every ".." variant, every absolute
// spelling, everything path.Clean does not fix-point, and every C0/DEL byte
// (the clause that was missing from unsafeRel and is the reason this predicate
// exists at all).
func TestSec_Path_SafePathRefusesEveryEscapeAnOpCouldName(t *testing.T) {
	bad := []string{
		// empty and self-referential
		"", ".", "./", "/.", "..", "../", "/..",
		// parent escapes, at every depth and in both orders
		"../etc/passwd", "../../../../../../etc/shadow", "a/../../b", "a/b/../../../c",
		"docs/..", "docs/../..", "..//x", ".././x", "a/./../..",
		// absolute, and the two spellings Go treats differently
		"/", "//", "/etc/passwd", "//etc/passwd", "///a", "/a/../b",
		// not Clean-stable: normalizing instead of refusing would land two
		// journal paths on one file
		"a//b", "./a", "a/", "a/./b", "a/b/", "a/.", "./", "a//",
		// C0 and DEL, which the metadata backends disagree about
		"nul\x00.md", "bell\x07.md", "tab\tx.md", "nl\nx.md", "cr\rx.md",
		"esc\x1b[31m.md", "del\x7f.md", "\x00", "\x1f", "a/\x00/b",
		// a control character hidden mid-path, at depth
		"docs/notes\x00/deep/file.md",
	}
	for _, p := range bad {
		if SafePath(p) {
			t.Errorf("SafePath(%q) = true; every ingest door in the product now delegates to "+
				"this one predicate, so a path it accepts is journaled by the hub, joined onto "+
				"every teammate's working folder and stored as a metadata row", p)
		}
	}
}

// The other direction: the ordinary paths a real project is made of must keep
// working. A rule that refused these would be found by users, not attackers,
// but it is the reason a fix must not simply tighten the predicate blindly.
func TestSec_Path_SafePathStillAcceptsOrdinaryProjectPaths(t *testing.T) {
	good := []string{
		"a", "a.md", "docs/notes.md", "a/b/c/d/e.txt", ".bdriveignore",
		"weird but legal.md", "unicode-café.md", "emoji-🐻.md",
		"dot.in.the.middle.md", "-leading-dash.md", "_under.md",
		strings.Repeat("deep/", 100) + "leaf.md",
	}
	for _, p := range good {
		if !SafePath(p) {
			t.Errorf("SafePath(%q) = false, refusing an ordinary path", p)
		}
	}
}

// SafePath's contract is "refused, never normalized". This pins it: for every
// path it accepts, Clean is the identity — so no accepted path has a second
// spelling that is also accepted, which is what stops two journal entries
// resolving to one file.
func TestSec_Path_EveryAcceptedPathIsItsOwnCleanForm(t *testing.T) {
	for _, p := range []string{
		"a", "docs/notes.md", "a/b/c", ".bdriveignore", "x.md", "a b/c d.md",
		"..foo", "foo..", "a/..b", "a/b..c", "...", "....",
	} {
		if !SafePath(p) {
			continue
		}
		if got := path.Clean(p); got != p {
			t.Errorf("SafePath(%q) = true but path.Clean gives %q — two spellings of one path "+
				"are both acceptable, which is exactly what refusing rather than normalizing "+
				"was supposed to prevent", p, got)
		}
		if filepath.IsAbs(p) || path.IsAbs(p) {
			t.Errorf("SafePath(%q) = true for an absolute path", p)
		}
	}
}

// The three delegating call sites must BE this predicate, not a copy of it.
// Round 7's own finding was that three spellings of one rule had drifted, so
// the standing regression is that they still answer identically on the whole
// table above. templates.SafePath and syncer.unsafeRel live in other packages
// and are covered by their own suites; what can be pinned here is that the
// predicate is total — it never panics and never depends on anything but its
// argument, which is what makes a single shared rule safe to share.
func TestSec_Path_SafePathIsTotalOverArbitraryBytes(t *testing.T) {
	// Every single byte as a whole path, and as a segment, must produce an
	// answer rather than a panic — Op.Path is arbitrary JSON off a peer.
	for b := 0; b < 256; b++ {
		s := string([]byte{byte(b)})
		got := SafePath(s)
		if b < 0x20 || b == 0x7f {
			if got {
				t.Errorf("SafePath(%q) = true for control byte %#x", s, b)
			}
			continue
		}
		if b == '/' || b == '.' {
			if got {
				t.Errorf("SafePath(%q) = true", s)
			}
			continue
		}
		if !got {
			t.Errorf("SafePath(%q) = false for ordinary byte %#x", s, b)
		}
		if SafePath("docs/"+s+"/leaf.md") != true {
			t.Errorf("SafePath of byte %#x as a middle segment = false", b)
		}
	}
}
