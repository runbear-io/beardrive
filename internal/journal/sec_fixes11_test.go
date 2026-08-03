package journal

import "testing"

// Round 12, attacking round 11's SafeText.
//
// SafeText is the rule "a rendered row must not lie about what it says". Its
// own doc comment gives the criterion twice: SafePath refuses C0 and DEL
// because "the C0s render as nothing, so 'notes\x7f.md' and 'notes.md' are two
// indistinguishable entries in one tree", and SafeText refuses the bidi format
// controls because they reorder a rendered row.
//
// It refuses U+200E and U+200F — LRM and RLM, which are zero-width marks — and
// admits their four immediate neighbours in the same block, U+200B..U+200D,
// plus U+FEFF. Those render as nothing in every browser, terminal and file
// listing, so they produce exactly the tree the C0 clause exists to prevent:
// two entries that are one entry to a reader.
//
// The consequence is concrete on this hub. A path is a sync-level identity —
// a member pushes "READ\u200bME.md", the tree and the history feed show a
// second "README.md" next to the real one, a share link on it reads as a share
// of the real file, and no reader can tell the two rows apart. The same string
// in a note or an author is the same lie one column over.
//
// This is the narrow claim: characters whose rendered width is zero, not
// homoglyphs generally. U+200E/U+200F are already refused on precisely this
// ground.
func TestSec_Path_SafeTextRefusesTheZeroWidthCharactersThatHideADuplicate(t *testing.T) {
	// Control: the neighbours round 11 DID refuse. If these ever pass, the
	// fixture is wrong, not the finding.
	for _, r := range []rune{0x200e, 0x200f, 0x202e} {
		if SafeText("READ" + string(r) + "ME.md") {
			t.Fatalf("control: SafeText admits U+%04X, which round 11 refuses", r)
		}
	}

	for _, tc := range []struct {
		r    rune
		name string
	}{
		{0x200b, "ZERO WIDTH SPACE"},
		{0x200c, "ZERO WIDTH NON-JOINER"},
		{0x200d, "ZERO WIDTH JOINER"},
		{0xfeff, "ZERO WIDTH NO-BREAK SPACE (BOM)"},
	} {
		p := "READ" + string(tc.r) + "ME.md"
		if SafeText(p) {
			t.Errorf("SafeText(%q) = true: U+%04X %s renders as nothing, so this path and "+
				"README.md are two rows a reader cannot tell apart — the same tree the C0 clause "+
				"of this function exists to make unreachable, and the same rendering property "+
				"U+200E/U+200F are refused for.", p, tc.r, tc.name)
		}
		// SafePath deliberately diverges from SafeText for the two JOINERS.
		// U+200C is orthographically required in Persian and several Indic
		// scripts ("\u0645\u06cc\u200c\u0631\u0648\u0645" is a word, not an attack) and U+200D builds
		// most multi-person emoji, so refusing them in a FILENAME does not
		// harden a hub — it makes those users' files unsyncable. The
		// confusability they buy is also already reachable without them:
		// a Cyrillic homoglyph produces the identical "two rows, one reader"
		// tree and is allowed, so this clause was paying an i18n cost for a
		// partial mitigation. A note has no orthography, so SafeText above
		// still refuses all four.
		wantPath := tc.r == 0x200c || tc.r == 0x200d
		if SafePath(p) != wantPath {
			t.Errorf("SafePath(%q) = %v, want %v (U+%04X %s)", p, !wantPath, wantPath, tc.r, tc.name)
		}
	}

	// And nothing ordinary may break: the rule is about zero-width formats,
	// not about non-ASCII.
	for _, p := range []string{"README.md", "notes/café.md", "docs/日本語.md", "a b/c-d_e.txt"} {
		if !SafePath(p) {
			t.Errorf("SafePath(%q) = false, refusing an ordinary path", p)
		}
	}
}
