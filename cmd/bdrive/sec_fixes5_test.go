package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 6, row 21 — attacking round 5's own fix, safeField (cmds.go).
//
// safeField strips exactly two things: runes below 0x20, and 0x7f. That is the
// C0 set plus DEL. It is the right start, and it closes every sequence round 5
// tested, because every one of those started with ESC (0x1b).
//
// It does not close the two families that need no ESC at all:
//
//   - the Unicode bidirectional format controls (U+202A..U+202E, U+2066..
//     U+2069, U+200E/U+200F, U+061C). These are the "Trojan Source"
//     characters (CVE-2021-42574): they are not control characters by
//     Unicode's own C0/C1 definition, they survive every C0 filter, and a
//     single U+202E makes every glyph after it render right-to-left. The
//     audit row's own columns then read in the wrong order — a delete of
//     evil.exe can be made to display as something else entirely, and the
//     actor columns come after the path on the same line.
//   - the C1 controls, U+0080..U+009F. In a UTF-8 terminal these arrive as
//     two bytes (C2 80 .. C2 9F) and xterm and its many descendants decode
//     them back to 8-bit controls: U+009B IS CSI, U+009D IS OSC, U+0090 IS
//     DCS, U+0085 IS NEL. So the whole escape vocabulary round 5 closed off
//     is reachable again without ever emitting an ESC byte.
//
// The property asserted is the one row 21 already stands for: a peer's journal
// strings must not be able to change how the audit row RENDERS. Helpers are
// prefixed secfx5.

// secfx5BidiControls are the format characters that reorder rendered text.
var secfx5BidiControls = []rune{
	'؜', // ARABIC LETTER MARK
	'‎', // LEFT-TO-RIGHT MARK
	'‏', // RIGHT-TO-LEFT MARK
	'‪', // LEFT-TO-RIGHT EMBEDDING
	'‫', // RIGHT-TO-LEFT EMBEDDING
	'‬', // POP DIRECTIONAL FORMATTING
	'‭', // LEFT-TO-RIGHT OVERRIDE
	'‮', // RIGHT-TO-LEFT OVERRIDE
	'⁦', // LEFT-TO-RIGHT ISOLATE
	'⁧', // RIGHT-TO-LEFT ISOLATE
	'⁨', // FIRST STRONG ISOLATE
	'⁩', // POP DIRECTIONAL ISOLATE
}

// secfx5Rendering reports every rune in a printed row that a terminal treats
// as something other than a printable glyph.
func secfx5Rendering(line string) []string {
	var found []string
	for _, r := range line {
		if r >= 0x80 && r <= 0x9f {
			found = append(found, fmt.Sprintf("C1 control %U", r))
			continue
		}
		for _, b := range secfx5BidiControls {
			if r == b {
				found = append(found, fmt.Sprintf("bidi control %U", r))
			}
		}
	}
	return found
}

// TestSec_Output_PeerStringsCannotReorderOrReintroduceControlsInTheAuditRow
// runs the same drill round 5 ran, with the sequences that do not start with
// ESC.
func TestSec_Output_PeerStringsCannotReorderOrReintroduceControlsInTheAuditRow(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*journal.Op)
	}{
		{"path flips the rest of the row right-to-left (U+202E)", func(o *journal.Op) {
			// Everything after the override, including the columns that name
			// the actor and the device, is drawn in reverse.
			o.Path = "invoices/‮gnp.eciovni_dekcah"
		}},
		{"path isolates and overrides so the extension lies", func(o *journal.Op) {
			o.Path = "setup⁧‮exe.⁩sh"
		}},
		{"device name overrides the trailing columns", func(o *journal.Op) {
			o.DeviceName = "‮potpal-ecila"
		}},
		{"note carries an 8-bit CSI (U+009B) instead of ESC[", func(o *journal.Op) {
			o.Note = "2J3JH all clear"
		}},
		{"note carries an 8-bit OSC (U+009D) to write the clipboard", func(o *journal.Op) {
			o.Note = "52;c;cm0gLXJmIH4K"
		}},
		{"user name carries an 8-bit DCS (U+0090)", func(o *journal.Op) {
			o.UserName = "Alice$q\"q"
		}},
		{"path carries NEL (U+0085), a line break to many terminals", func(o *journal.Op) {
			o.Path = "notes.md2026-01-01 12:00:00  delete  everything.md"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := secoutMount(t)
			op := secoutOp(1, "ordinary.md")
			tc.mut(&op)
			secoutPlant(t, folder, "attacker", []journal.Op{op})

			out := secoutRun(t, folder)
			if strings.TrimSpace(out) == "" || strings.Contains(out, "no history yet") {
				t.Fatalf("control failed: bdrive log printed no rows:\n%q", out)
			}
			for _, line := range strings.Split(out, "\n") {
				if bad := secfx5Rendering(line); len(bad) > 0 {
					t.Errorf("bdrive log printed a row a peer can re-render: %v\nrow: %q\n"+
						"safeField strips C0 and DEL only, so nothing above U+007F is filtered",
						bad, line)
				}
			}
		})
	}
}

// TestSec_Output_RestoreListDoesNotReorderTheVersionTable is the same property
// on the other surface round 5 fixed, `bdrive restore --list`: the columns
// there are the ones that say WHO produced each version.
func TestSec_Output_RestoreListDoesNotReorderTheVersionTable(t *testing.T) {
	op := secoutOp(1, "plan.md")
	op.UserName = "‮ecila"
	op.DeviceName = "31m eve-laptop"

	var sb strings.Builder
	printVersions(&sb, []journal.Op{op}, "")
	out := sb.String()
	if strings.TrimSpace(out) == "" {
		t.Fatalf("control failed: printVersions printed nothing")
	}
	if bad := secfx5Rendering(out); len(bad) > 0 {
		t.Errorf("restore --list printed a row a peer can re-render: %v\nrow: %q", bad, out)
	}
}
