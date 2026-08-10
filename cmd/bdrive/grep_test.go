package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// grepMount builds an isolated BDRIVE_HOME with one enrolled project folder.
// Nothing here contacts a hub: grep is a local read.
func grepMount(t *testing.T) string {
	t.Helper()
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	folder, _ = filepath.EvalSymlinks(folder)
	if _, err := config.SaveProject(folder, config.Project{
		Volume: "wiki",
		Remote: "https://hub.example.com/p/p-12345678",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := config.EnrollMount(folder); err != nil {
		t.Fatal(err)
	}
	return folder
}

func grepWrite(t *testing.T, folder, rel, body string) {
	t.Helper()
	abs := filepath.Join(folder, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(abs, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// grepRun drives the real cobra command and returns its combined output plus
// the error it exited with (errNoMatch is grep's status-1 no-match).
func grepRun(t *testing.T, args ...string) (string, error) {
	t.Helper()
	c := grepCmd()
	var out bytes.Buffer
	c.SetOut(&out)
	c.SetErr(&out)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), err
}

func TestGrepPrintsMatchingLines(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "wiki/runbook.md", "intro\nthe retention fold collapses day buckets\ntail\n")
	grepWrite(t, folder, "specs/reads.md", "retention folding happens at boot\n")
	grepWrite(t, folder, "unrelated.md", "nothing to see\n")

	out, err := grepRun(t, "retention.*fold", folder)
	if err != nil {
		t.Fatalf("grep: %v\n%s", err, out)
	}
	for _, want := range []string{
		"wiki/runbook.md:2: the retention fold collapses day buckets",
		"specs/reads.md:1: retention folding happens at boot",
		"2 files, 2 matching lines",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.Contains(out, "unrelated.md") {
		t.Errorf("non-matching file printed:\n%s", out)
	}

	// -l prints bare paths, one per line, and no summary.
	out, err = grepRun(t, "-l", "retention", folder)
	if err != nil {
		t.Fatalf("grep -l: %v\n%s", err, out)
	}
	got := strings.Fields(out)
	if len(got) != 2 || got[0] != "specs/reads.md" || got[1] != "wiki/runbook.md" {
		t.Errorf("-l should print two bare paths, got %q", out)
	}
}

func TestGrepFlags(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "notes.md", "TODO later\ntodo now\nliteral a[b]c here\n")

	// Default is case-sensitive; -i widens it.
	out, _ := grepRun(t, "TODO", folder)
	if strings.Contains(out, "todo now") {
		t.Errorf("case-sensitive match leaked:\n%s", out)
	}
	out, err := grepRun(t, "-i", "TODO", folder)
	if err != nil || !strings.Contains(out, "todo now") {
		t.Errorf("-i should match both cases: %v\n%s", err, out)
	}

	// -F takes the pattern literally: as a regexp a[b]c matches "abc", which
	// is not in the file — only the literal text is.
	if _, err := grepRun(t, "a[b]c", folder); !errors.Is(err, errNoMatch) {
		t.Errorf("regexp a[b]c should not match, got %v", err)
	}
	out, err = grepRun(t, "-F", "a[b]c", folder)
	if err != nil || !strings.Contains(out, "literal a[b]c here") {
		t.Errorf("-F should match literally: %v\n%s", err, out)
	}

	// A bad pattern is a real error, printed, not a silent no-match.
	out, err = grepRun(t, "a(", folder)
	if err == nil || errors.Is(err, errNoMatch) {
		t.Errorf("bad pattern should error, got %v", err)
	}
	if !strings.Contains(out, "bad pattern") {
		t.Errorf("bad pattern should say so:\n%s", out)
	}
}

func TestGrepLimit(t *testing.T) {
	folder := grepMount(t)
	var body strings.Builder
	for i := 0; i < 500; i++ {
		body.WriteString("hit\n")
	}
	grepWrite(t, folder, "many.md", body.String())

	// Default caps at 200 and says so.
	out, err := grepRun(t, "hit", folder)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "many.md:"); n != 200 {
		t.Errorf("default limit should print 200 lines, got %d", n)
	}
	if !strings.Contains(out, "output limited to 200 lines") {
		t.Errorf("truncation should be announced:\n%s", out[len(out)-200:])
	}

	// -n 0 means all.
	out, err = grepRun(t, "-n", "0", "hit", folder)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(out, "many.md:"); n != 500 {
		t.Errorf("-n 0 should print every line, got %d", n)
	}
	if strings.Contains(out, "output limited") {
		t.Errorf("-n 0 must not announce truncation:\n%s", out)
	}
}

// Results match what the project actually syncs: the same .bdriveignore rule
// that keeps a file off the hub keeps it out of search.
func TestGrepRespectsTheSyncFilter(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "keep.md", "needle\n")
	grepWrite(t, folder, "drafts/skip.md", "needle\n")

	out, err := grepRun(t, "needle", folder)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "drafts/skip.md") {
		t.Fatalf("drafts/skip.md should match before the rule:\n%s", out)
	}

	grepWrite(t, folder, ".bdriveignore", "drafts/\n")
	out, err = grepRun(t, "needle", folder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "drafts/skip.md") {
		t.Errorf("an ignored path must not be searchable:\n%s", out)
	}
	if !strings.Contains(out, "keep.md") {
		t.Errorf("keep.md should still match:\n%s", out)
	}
}

// .bdrive/ holds the project's own settings and .bdrive-tmp-* are half-written
// state files. Neither is project content and neither may ever surface.
func TestGrepNeverSearchesBdriveState(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "real.md", "p-12345678\n")
	grepWrite(t, folder, ".bdrive-tmp-half", "p-12345678\n")

	out, err := grepRun(t, "p-12345678", folder)
	if err != nil {
		t.Fatal(err)
	}
	// .bdrive/config.json contains the remote URL, so it would match.
	if strings.Contains(out, ".bdrive/") || strings.Contains(out, ".bdrive-tmp-") {
		t.Errorf("bdrive state leaked into results:\n%s", out)
	}
	if !strings.Contains(out, "real.md") {
		t.Errorf("real.md should match:\n%s", out)
	}
}

func TestGrepSkipsBinaryFiles(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "text.md", "needle here\n")
	// A NUL inside the first 8 KB is the binary rule.
	grepWrite(t, folder, "blob.bin", "needle here\x00 and more\n")
	// A NUL past the sniff window is not: the file reads as text.
	grepWrite(t, folder, "late.bin", strings.Repeat("x\n", 6000)+"needle here\n\x00")

	out, err := grepRun(t, "needle", folder)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "blob.bin") {
		t.Errorf("binary file searched:\n%s", out)
	}
	if !strings.Contains(out, "text.md") || !strings.Contains(out, "late.bin") {
		t.Errorf("text files should match:\n%s", out)
	}
}

// Every matched line is content a teammate wrote and synced — a strictly wider
// version of the surface safeField exists for. A planted file must not be able
// to repaint or reverse the operator's terminal.
func TestGrepOutputCannotRewriteTheTerminal(t *testing.T) {
	folder := grepMount(t)
	hostile := "needle \x1b[2J\x1b[3J\x1b[H\r‮gnitset​\x9b31m\x7f end"
	grepWrite(t, folder, "planted.md", hostile+"\n")
	// The path is attacker-controlled too: a teammate chooses the file name.
	grepWrite(t, folder, "na‮me\x1b[31m.md", "needle\n")

	for _, args := range [][]string{{"needle", folder}, {"-l", "needle", folder}} {
		out, err := grepRun(t, args...)
		if err != nil {
			t.Fatalf("grep %v: %v", args, err)
		}
		for _, bad := range []string{"\x1b", "\r", "‮", "​", "\x9b", "\x7f"} {
			if strings.Contains(out, bad) {
				t.Errorf("grep %v leaked %q into the terminal:\n%q", args, bad, out)
			}
		}
		// One match is one line: no forged rows.
		for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
			for _, r := range line {
				if r < 0x20 && r != '\t' || r == 0x7f {
					t.Errorf("control rune %q survived in %q", r, line)
				}
			}
		}
	}
}

// A read-only query must not enroll this device: LoadProject, never
// ResolveMount. Outside a project it says so, exits non-zero, and the registry
// is untouched.
func TestGrepOutsideAProjectWritesNothing(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	folder := t.TempDir()
	grepWrite(t, folder, "loose.md", "needle\n")

	mounts := filepath.Join(os.Getenv("BDRIVE_HOME"), "mounts.json")
	before, beforeErr := os.ReadFile(mounts)

	out, err := grepRun(t, "needle", folder)
	if err == nil || errors.Is(err, errNoMatch) {
		t.Fatalf("should fail outside a project, got %v", err)
	}
	if !strings.Contains(out, "not a beardrive project") {
		t.Errorf("message should name the problem:\n%s", out)
	}
	after, afterErr := os.ReadFile(mounts)
	if (beforeErr == nil) != (afterErr == nil) || !bytes.Equal(before, after) {
		t.Errorf("the registry was written by a read-only query")
	}
}

// Exit codes are grep's, so `bdrive grep x || …` composes: 0 on match, 1 on
// no match with nothing printed at all.
func TestGrepExitCodes(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "a.md", "hay\n")

	if out, err := grepRun(t, "hay", folder); err != nil {
		t.Errorf("match should exit 0: %v\n%s", err, out)
	}
	out, err := grepRun(t, "needle", folder)
	if !errors.Is(err, errNoMatch) {
		t.Errorf("no match should return errNoMatch, got %v", err)
	}
	if out != "" {
		t.Errorf("no match must print nothing, got %q", out)
	}
}

// grep opens the volume store read-only for one field and never takes the
// volume flock, so a daemon mid-cycle cannot make a search hang.
func TestGrepDoesNotBlockOnTheVolumeLock(t *testing.T) {
	folder := grepMount(t)
	grepWrite(t, folder, "a.md", "needle\n")

	proj, found, err := config.LoadProject(folder)
	if err != nil || !found {
		t.Fatal(err)
	}
	vdir, err := config.VolumeDir(proj.ID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := st.Lock() // stand in for a cycle in progress
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	done := make(chan string, 1)
	go func() {
		out, _ := grepRun(t, "needle", folder)
		done <- out
	}()
	select {
	case out := <-done:
		if !strings.Contains(out, "a.md:1: needle") {
			t.Errorf("grep under the lock should still find it:\n%s", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("grep blocked on the volume lock")
	}
}
