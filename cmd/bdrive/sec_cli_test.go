package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/syncer"
)

// Round 6 — "15 of 22 CLI commands have no security test driving them".
// These attack `export` (the archive command whose default output path is
// chosen by a value that travels with the folder), `scope`/`init --only` (the
// managed .bdriveignore block round 4 proved is a team-wide delete lever), and
// `status` (a command that prints hub-chosen strings straight to a terminal —
// the class round 5 closed for `bdrive log` and only for `bdrive log`).
//
// Helpers are prefixed seccli.

// seccliMount builds an isolated BDRIVE_HOME with one enrolled project folder
// whose .bdrive/config.json carries the given volume name, and a file://
// remote so commands that need a backend get a real one with no hub.
//
// The volume name matters: init writes it from the project NAME the hub
// returned (init.go:218, `Volume: p.Name`), and a project name is chosen by
// whoever created the project — any member of your org — and passes only
// through trimName, which strips \n \r \t and nothing else. So `..`, ESC and
// every other byte survive the hub round trip into this file. The same file
// also simply travels with the folder (a zip, a clone, a colleague's copy),
// which is how rounds 4 and 5 reached Project.ID and Project.Remote.
func seccliMount(t *testing.T, volume string) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	store := t.TempDir()
	if _, err := config.SaveProject(folder, config.Project{
		Volume: volume,
		Remote: "file://" + store,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.ResolveMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

// seccliRun runs one cobra command with args, capturing everything it writes
// to the real stdout as well as to the command's own writers — `status` and
// `init` use fmt.Printf, not cmd.OutOrStdout().
func seccliRun(t *testing.T, cmd interface {
	SetArgs([]string)
	Execute() error
}, args []string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	cmd.SetArgs(args)
	runErr := cmd.Execute()
	os.Stdout = old
	w.Close()
	out, _ := io.ReadAll(r)
	r.Close()
	return string(out), runErr
}

// TestSec_CLI_ExportOutputPathCannotEscapeTheWorkingDirectory
//
// `bdrive export` with no -o builds its destination as
//
//	out = fmt.Sprintf("%s-export-%s.tar.gz", proj.Volume, ...)   // migrate.go:74
//	f, err := os.Create(out)
//
// proj.Volume is read verbatim out of the folder's .bdrive/config.json — the
// file rounds 4 and 5 already established is untrusted input (Project.ID is
// now regex-validated because it was joined onto $BDRIVE_HOME; Project.Remote
// is now origin-bound because it chose where the device token was sent).
// Volume was left out of that sweep, and it is the one field that reaches
// os.Create.
//
// It is not only a file that travels with the folder: init writes it from the
// project name the hub hands back, and project names are not path-validated
// anywhere on the hub (trimName strips only \n \r \t). So an org member who
// names a project `../../../../tmp/pwned` chooses, on every teammate's
// machine, where that teammate's `bdrive export` writes a multi-megabyte file
// — outside the project, outside the working directory, with no prompt and no
// mention of the path until after os.Create has already truncated whatever
// was there.
//
// The secure behavior asserted: a default output path stays in the directory
// the command was run in. -o is the flag for writing anywhere else, and it is
// the user typing it.
func TestSec_CLI_ExportOutputPathCannotEscapeTheWorkingDirectory(t *testing.T) {
	// Control: an ordinary project name writes an archive in the cwd, so the
	// harness is known to reach os.Create at all.
	t.Run("control_ordinary_name", func(t *testing.T) {
		folder := seccliMount(t, "wiki")
		cwd := t.TempDir()
		t.Chdir(cwd)
		if _, err := seccliRun(t, exportCmd(), []string{folder}); err != nil {
			t.Fatalf("export: %v", err)
		}
		if n := len(seccliArchives(t, cwd)); n != 1 {
			t.Fatalf("control wrote %d archives in the working directory, want 1", n)
		}
	})

	t.Run("traversing_name", func(t *testing.T) {
		folder := seccliMount(t, "../../pwned")
		outside := t.TempDir()
		cwd := filepath.Join(outside, "a", "b")
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(cwd)

		out, err := seccliRun(t, exportCmd(), []string{folder})
		if err != nil {
			t.Logf("export returned %v (output: %s)", err, out)
		}
		if escaped := seccliArchives(t, outside); len(escaped) > 0 {
			t.Errorf("export wrote %v — outside the working directory, at a path chosen by "+
				"the project name in .bdrive/config.json", escaped)
		}
	})
}

// seccliArchives lists the .tar.gz files directly in dir.
func seccliArchives(t *testing.T, dir string) []string {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".tar.gz") {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// TestSec_CLI_ScopeRuleCannotOutliveTheScopeThatWroteIt
//
// `bdrive init --only` and `bdrive scope add` both write a managed block into
// .bdriveignore, which SYNCS: its rules apply to every teammate's device.
// cleanScopeDirs (scopefile.go:97) validates each name for `..` and for being
// empty, and then scopeLines renders it as `"!/" + d + "/"` — one line, one
// name, assumed.
//
// A newline in a name is not checked, and a newline is a legal byte in a unix
// directory name. `docs\n# end bdrive scope\n*` renders as three lines, the
// second of which is the block's own END MARKER: the managed block terminates
// early and the injected `*/` rule lands OUTSIDE it. writeScopeDirs removes
// the block by finding its markers, so removing the scope — `bdrive scope rm`,
// `bdrive init --only` with nothing, widening back to the whole folder —
// keeps the injected rule forever. `*/` ignores every directory in the
// project, for the whole team, and no bdrive command can take it out again.
//
// The secure behavior asserted: whatever a scope writes, removing the scope
// puts .bdriveignore back the way it was.
func TestSec_CLI_ScopeRuleCannotOutliveTheScopeThatWroteIt(t *testing.T) {
	for _, name := range []string{
		"docs\n# end bdrive scope\n*",
		"docs\n*",
		"docs\n!/",
	} {
		t.Run(strings.ReplaceAll(name, "\n", "\\n"), func(t *testing.T) {
			folder := t.TempDir()
			ignorePath := filepath.Join(folder, syncer.IgnoreFile)
			before := "node_modules/\n*.log\n"
			if err := os.WriteFile(ignorePath, []byte(before), 0o644); err != nil {
				t.Fatal(err)
			}

			dirs, err := cleanScopeDirs([]string{name})
			if err != nil {
				return // refused at the door: nothing to assert
			}
			if err := writeScopeDirs(folder, dirs); err != nil {
				t.Fatal(err)
			}
			// Now widen back to the whole folder, the way `bdrive scope rm` of
			// the last folder does.
			if err := writeScopeDirs(folder, nil); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(ignorePath)
			if err != nil {
				t.Fatal(err)
			}
			if got := string(data); got != before {
				t.Errorf("removing the scope left rules behind in the synced .bdriveignore\n"+
					"before:\n%q\nafter:\n%q", before, got)
			}
		})
	}
}

// TestSec_CLI_StatusDoesNotRenderHubChosenStringsToTheTerminal
//
// Round 5 closed this class for `bdrive log` and `bdrive restore --list`
// (scoreboard row 21: "every peer-controlled string that reaches a terminal"),
// with one safeField where those rows are assembled. `bdrive status` prints
// two strings from the same trust level with a bare %s:
//
//	fmt.Printf("  project:  %s (%s)\n", mi.Volume, id)   // cmds.go:209
//	fmt.Printf("  remote:   %s\n", mi.Remote)            // cmds.go:211
//
// Both come from .bdrive/config.json, and Volume comes originally from the
// hub's project name — chosen by any member of your org, passed through a
// trimName that strips \n \r \t and leaves ESC, OSC and DEL intact. So a
// project name is a terminal escape sequence on every teammate's `bdrive
// status`: repaint the line, clear the scrollback, set the window title, or
// on terminals with the classic OSC/DECRQSS reporting behaviours make the
// emulator type a reply back onto the shell's stdin.
//
// The secure behavior asserted is the one round 5 already chose for the other
// two surfaces: no C0 control character or DEL reaches the terminal.
func TestSec_CLI_StatusDoesNotRenderHubChosenStringsToTheTerminal(t *testing.T) {
	// Control: an ordinary name is printed, so the assertion is looking at
	// output the command actually produced.
	folder := seccliMount(t, "wiki")
	out, err := seccliRun(t, statusCmd(), []string{folder})
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if !strings.Contains(out, "wiki") {
		t.Fatalf("control: status did not print the project name\n%s", out)
	}

	for _, name := range []string{
		"wiki\x1b[2K\r  project:  totally-fine (p-00000000)",
		"wiki\x1b]0;pwned\x07",
		"wiki\x1b]52;c;cHduZWQ=\x07",
		"wiki\x7f\x7f\x7f\x7f",
	} {
		t.Run(strings.Map(func(r rune) rune {
			if r < 0x20 || r == 0x7f {
				return '.'
			}
			return r
		}, name), func(t *testing.T) {
			folder := seccliMount(t, name)
			out, err := seccliRun(t, statusCmd(), []string{folder})
			if err != nil {
				t.Fatalf("status: %v", err)
			}
			for _, line := range strings.Split(out, "\n") {
				for _, r := range line {
					if r < 0x20 || r == 0x7f {
						t.Errorf("status printed control character %q inside a line: %q", r, line)
					}
				}
			}
		})
	}
}
