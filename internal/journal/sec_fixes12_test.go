package journal

import (
	"testing"
	"unicode"
)

// Round 13 — attacking round 12's SafeText additions.
//
// Round 12 added four code points (U+200B-200D, U+FEFF) on a stated rule:
// "they render as nothing, so 'READ<ZWSP>ME.md' and 'README.md' are two
// indistinguishable entries in one tree — and one tree row, one history row and
// one share link is all a reader gets to tell them apart."
//
// It enumerated the neighbours of the code points already listed rather than
// applying that rule. The Unicode general category for exactly this — a
// character that carries no glyph — is Cf (format), and most of Cf is still
// admitted. Two of the gaps matter more than the confusability the rule names:
//
//   - U+00AD SOFT HYPHEN is invisible except at a line break. It is the oldest
//     and most widely supported invisible character there is.
//   - U+E0020..U+E007F, the tag block, encodes the whole of printable ASCII with
//     no glyph at all. A path is not only rendered to a person on this hub; it is
//     read out to a coding agent (the gated-link formula, the read heatmap,
//     `bdrive status`). Tag characters are how arbitrary text is carried past a
//     human reader and into that agent's context, and SafeText is the only thing
//     between a peer's journal op and both.
func TestSec_Journal_SafeTextRefusesTheInvisibleFormatCharacters(t *testing.T) {
	// The rule as round 12 implemented it, on the code points it listed:
	for _, r := range []rune{0x200b, 0x200c, 0x200d, 0xfeff, 0x200e, 0x061c} {
		if SafeText("READ" + string(r) + "ME.md") {
			t.Fatalf("fixture: U+%04X should already be refused by round 12", r)
		}
	}

	gaps := []struct {
		r    rune
		name string
	}{
		{0x00ad, "SOFT HYPHEN — invisible except at a line break"},
		{0x2060, "WORD JOINER — added to Unicode as the replacement for U+FEFF, which IS refused"},
		{0x2061, "FUNCTION APPLICATION — invisible math operator"},
		{0x180e, "MONGOLIAN VOWEL SEPARATOR — Cf and zero-width since Unicode 6.3"},
		{0xfff9, "INTERLINEAR ANNOTATION ANCHOR"},
		{0xfffb, "INTERLINEAR ANNOTATION TERMINATOR"},
		{0xe0001, "LANGUAGE TAG"},
		{0xe0041, "TAG LATIN CAPITAL A — the block that encodes all of ASCII with no glyph"},
	}
	for _, g := range gaps {
		if !unicode.Is(unicode.Cf, g.r) {
			t.Fatalf("fixture: U+%04X is not category Cf; check the premise", g.r)
		}
		path := "README" + string(g.r) + ".md"
		if SafeText(path) {
			t.Errorf("SafeText admits U+%04X (%s): %q renders identically to README.md in every "+
				"tree row, history row and share link, and for the tag block it also carries "+
				"arbitrary hidden ASCII into whatever agent is told to read this path",
				g.r, g.name, path)
		}
	}
}
