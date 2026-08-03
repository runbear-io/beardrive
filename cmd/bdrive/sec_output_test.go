package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/journal"
)

// Round 5, scoreboard row 15 / the "Op.Note reaching bdrive log's terminal
// output" gap carried since round 3.
//
// `bdrive log` is the audit surface: it is what an operator reads to find out
// what a teammate (or a compromised teammate device) did to a shared project.
// Every string it prints — Path, Note, User, UserName, Author, DeviceName —
// is arbitrary JSON off a peer's journal, written straight to a terminal with
// fmt.Fprintln and no escaping at all (cmds.go:304-310).
//
// A terminal is not a text sink. It executes what it is handed: ESC sequences
// move the cursor, repaint, clear the scrollback, set the window title, and on
// terminals with the classic OSC/DECRQSS reporting behaviours they make the
// emulator *type* a reply back onto the shell's stdin. A carriage return
// rewrites the line that was just drawn. A newline forges a whole row. So a
// peer that controls these strings controls the audit trail an operator uses
// to detect the peer.
//
// The secure behavior asserted here is the minimum that makes the output
// trustworthy, and the one every well-behaved CLI already implements: one
// journal entry is one line, and a line carries no C0 control characters. It
// says nothing about *how* (escape, strip, quote) — that is the ciso's call.
//
// Helpers are prefixed secout.

// secoutMount builds an isolated BDRIVE_HOME with one enrolled project folder
// and returns the folder.
func secoutMount(t *testing.T) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678", // never contacted: log is local
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// secoutPlant writes ops into the volume store as device dev's journal —
// byte for byte what `Cycle`'s pull lands there after a peer pushes them.
func secoutPlant(t *testing.T, folder, dev string, ops []journal.Op) {
	t.Helper()
	sess, _, err := openSession(context.Background(), folder, false)
	if err != nil {
		t.Fatal(err)
	}
	defer closeSession(sess)
	if err := sess.Store.AppendOps(dev, ops); err != nil {
		t.Fatal(err)
	}
}

// secoutRun runs `bdrive log` against folder and returns exactly what it
// printed.
func secoutRun(t *testing.T, folder string, extra ...string) string {
	t.Helper()
	c := logCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(append([]string{folder}, extra...))
	if err := c.Execute(); err != nil {
		t.Fatalf("log: %v", err)
	}
	return out.String()
}

// secoutOp is a plausible peer put op; callers overwrite the one field they
// are attacking.
func secoutOp(seq int64, p string) journal.Op {
	return journal.Op{
		Seq: seq, Lamport: seq, Time: time.Now().UTC().Add(-time.Duration(seq) * time.Minute),
		Mtime:  time.Now().UTC().Add(-time.Duration(seq) * time.Minute),
		Device: "attacker", DeviceName: "eve-laptop", Author: "eve@evil.test",
		User: "eve@evil.test", UserName: "Eve",
		Kind: journal.KindPut, Path: p,
		Blob: strings.Repeat("a", 64), Size: 12, Mode: 0o644,
	}
}

// secoutBadRunes reports the control characters a line must never carry. '\n'
// is excluded because it is the row separator the caller splits on.
func secoutBadRunes(line string) []string {
	var found []string
	for _, r := range line {
		if r < 0x20 && r != '\t' || r == 0x7f {
			found = append(found, fmt.Sprintf("%q", r))
		}
	}
	return found
}

// Every hostile shape, one at a time, so a failure names which field and which
// sequence got through.
func TestSec_Output_PeerJournalStringsCannotRewriteTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*journal.Op)
	}{
		{"note clears the screen and the scrollback", func(o *journal.Op) {
			o.Note = "\x1b[2J\x1b[3J\x1b[H harmless note"
		}},
		{"note sets the window title", func(o *journal.Op) {
			o.Note = "\x1b]0;bdrive — everything is fine\x07"
		}},
		{"note writes the system clipboard (OSC 52)", func(o *journal.Op) {
			o.Note = "\x1b]52;c;cm0gLXJmIH4K\x07"
		}},
		{"note asks the terminal to report, which types onto the shell", func(o *journal.Op) {
			o.Note = "\x1bP$q\"q\x1b\\\x1b[6n"
		}},
		{"note colours the row to look like the ones around it", func(o *journal.Op) {
			o.Note = "\x1b[32mverified\x1b[0m"
		}},
		{"carriage return redraws the row as something else", func(o *journal.Op) {
			// The row is printed as a delete; the CR sends the cursor home and
			// the rest of the note paints a put over it.
			o.Kind = journal.KindDelete
			o.Note = "\r2020-01-01 00:00:00  put     secrets.md                                Alice on alice-mac"
		}},
		{"newline in a note forges extra rows", func(o *journal.Op) {
			o.Note = "ok]\n2020-01-01 00:00:00  put     README.md                                 Alice on alice-mac"
		}},
		{"newline in a path forges extra rows", func(o *journal.Op) {
			o.Path = "a.md\n2020-01-01 00:00:00  delete  everything.md                             Alice on alice-mac"
		}},
		{"newline in a user name forges extra rows", func(o *journal.Op) {
			o.UserName = "Eve\n2020-01-01 00:00:00  put     approved.md                               Alice on alice-mac"
		}},
		{"escape in the device name", func(o *journal.Op) {
			o.DeviceName = "\x1b[1;31mALICE-MAC\x1b[0m"
		}},
		{"escape in the author fallback", func(o *journal.Op) {
			o.User, o.UserName = "", ""
			o.Author = "\x1b[8mhidden\x1b[28m"
		}},
		{"a NUL byte in a note", func(o *journal.Op) {
			o.Note = "clean\x00 not clean"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			folder := secoutMount(t)
			// One honest op alongside the hostile one: the control proves the
			// formatter works and that the row count below is meaningful.
			honest := secoutOp(1, "notes/honest.md")
			honest.Note = "a normal note"
			hostile := secoutOp(2, "notes/hostile.md")
			tc.mut(&hostile)
			secoutPlant(t, folder, "attacker", []journal.Op{honest, hostile})

			out := secoutRun(t, folder)
			lines := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
			if len(lines) != 2 {
				t.Errorf("2 journal ops printed %d rows — a peer string forged %d of them:\n%q",
					len(lines), len(lines)-2, out)
			}
			for i, line := range lines {
				if bad := secoutBadRunes(line); len(bad) > 0 {
					t.Errorf("row %d carries control characters %s the terminal will act on:\n%q",
						i, strings.Join(bad, " "), line)
				}
			}
		})
	}
}

// Length is the other way to own the screen: `bdrive log` prints at most -n
// rows (50 by default), so a peer whose single entry is 40 KB of padding
// scrolls every real entry out of the operator's scrollback without any
// escape sequence at all. Bounding the variable parts is what conflictName
// already does for exactly this reason (syncer.go).
func TestSec_Output_OneEntryCannotFillTheScreen(t *testing.T) {
	folder := secoutMount(t)
	honest := secoutOp(1, "notes/honest.md")
	honest.Note = "a normal note"
	huge := secoutOp(2, "notes/huge.md")
	huge.Note = strings.Repeat("padding ", 5000)
	huge.DeviceName = strings.Repeat("D", 5000)
	huge.Path = "notes/" + strings.Repeat("p", 5000) + ".md"
	secoutPlant(t, folder, "attacker", []journal.Op{honest, huge})

	out := secoutRun(t, folder)
	for i, line := range strings.Split(strings.TrimSuffix(out, "\n"), "\n") {
		if len(line) > 1024 {
			t.Errorf("row %d is %d bytes; one peer entry buries the rest of the log", i, len(line))
		}
	}
}

// The same strings reach the other reader of a peer's journal: `bdrive
// restore --list` prints Author/UserName/DeviceName through printVersions
// (restore.go:197). Whatever the fix is, it belongs in one place — this test
// exists so the second surface cannot be forgotten.
func TestSec_Output_RestoreListDoesNotRenderPeerEscapes(t *testing.T) {
	hostile := secoutOp(1, "notes/hostile.md")
	hostile.UserName = "\x1b[2J\x1b[HEve"
	hostile.DeviceName = "eve\rALICE-MAC"

	var out bytes.Buffer
	printVersions(&out, []journal.Op{hostile}, "")
	for i, line := range strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n") {
		if bad := secoutBadRunes(line); len(bad) > 0 {
			t.Errorf("restore --list row %d carries %s:\n%q", i, strings.Join(bad, " "), line)
		}
	}
}
