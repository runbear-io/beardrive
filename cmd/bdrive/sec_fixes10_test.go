package main

import (
	"testing"
)

// Round 11 attacks round 10's fixes in cmd/bdrive: syncer.EscapeIgnore, the
// rule `bdrive forget` writes, and EnrollMount.
//
// Helper prefix: secfx10. The fixtures are round 10's own (sec10Project,
// sec10Touch, sec10Filter, sec10ForgetRule) — reused, not copied.

// TestSec_Forget_AFilenameCannotRetargetTheRuleAtASibling.
//
// Round 10 gave `bdrive forget` an escaping function so a filename means
// itself: EscapeIgnore escapes `\`, `*`, `?`, `!` and `#`. It does not escape
// whitespace, and compile (ignore.go:162) begins with
//
//	line = strings.TrimSpace(line)
//
// so a filename with a leading or trailing space is not the rule that was
// written — it is the rule with the space removed, which names a DIFFERENT
// file. `bdrive forget` prunes the hub in the same command, so the file the
// user named keeps syncing and the sibling they did not name is deleted from
// the hub for the whole team, both reported as success.
//
// Trailing spaces are legal filenames on every filesystem beardrive runs on,
// and filenames in a synced project are chosen by whoever syncs into it. This
// is the same shape as round 10's three findings — a filename that is read as
// something other than itself — through the one metacharacter class the escape
// does not cover. Note the asymmetry that shows it is an oversight rather than
// a decision: a DIRECTORY named "a " works, because its rule ends in "/" and
// TrimSpace has nothing to take.
func TestSec_Forget_AFilenameCannotRetargetTheRuleAtASibling(t *testing.T) {
	root := sec10Project(t)
	sec10Touch(t, root, "notes/a ") // the file the user means to forget
	sec10Touch(t, root, "notes/a")  // a teammate's file, one character shorter

	rule := sec10ForgetRule(t, root, "notes/a ")
	f := sec10Filter(t, root)

	if !f.Skip("notes/a ") {
		t.Errorf("`bdrive forget 'notes/a '` wrote the rule %q, which does not match the file it "+
			"names — compile TrimSpaces the line, so the path is excluded from nothing and the "+
			"command reports success while the file keeps syncing", rule)
	}
	if f.Skip("notes/a") {
		t.Errorf("`bdrive forget 'notes/a '` wrote the rule %q, which excludes notes/a — a "+
			"different file, a teammate's, which the same command then deletes from the hub "+
			"for everyone", rule)
	}
}

// TestSec_Forget_TheEscapeStillCoversWhatRound10Closed re-measures the escape's
// existing coverage so a fix for the whitespace case cannot quietly drop it.
func TestSec_Forget_TheEscapeStillCoversWhatRound10Closed(t *testing.T) {
	for _, name := range []string{"a*", "a?", "#draft.md", `back\slash`} {
		t.Run(name, func(t *testing.T) {
			root := sec10Project(t)
			sec10Touch(t, root, name)
			sec10Touch(t, root, "alpha.md")
			rule := sec10ForgetRule(t, root, name)
			f := sec10Filter(t, root)
			if !f.Skip(name) {
				t.Errorf("rule %q does not cover the file it names", rule)
			}
			if f.Skip("alpha.md") {
				t.Errorf("rule %q also excludes a bystander", rule)
			}
		})
	}
}
