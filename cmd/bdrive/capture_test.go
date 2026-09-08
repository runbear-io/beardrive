package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
	"github.com/runbear-io/beardrive/internal/store"
)

// captureMount is grepMount's sibling: an isolated BDRIVE_HOME with one
// enrolled project, and the working directory inside it — capture resolves
// its mount by walking up from cwd.
func captureMount(t *testing.T) string {
	t.Helper()
	folder := grepMount(t)
	t.Chdir(folder)
	return folder
}

// captureRun drives the real cobra command with stdin wired to in, and
// returns stdout, stderr and the error it exited with.
func captureRun(t *testing.T, in string, args ...string) (string, string, error) {
	t.Helper()
	c := captureCmd()
	var out, errOut bytes.Buffer
	c.SetIn(strings.NewReader(in))
	c.SetOut(&out)
	c.SetErr(&errOut)
	c.SetArgs(args)
	err := c.Execute()
	return out.String(), errOut.String(), err
}

// inboxFiles lists the capture files in a mount's inbox/, or nil when the
// directory was never created.
func inboxFiles(t *testing.T, folder string) []string {
	t.Helper()
	ents, err := os.ReadDir(filepath.Join(folder, "inbox"))
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		names = append(names, e.Name())
	}
	return names
}

func TestCaptureWritesInboxFile(t *testing.T) {
	folder := captureMount(t)

	out, _, err := captureRun(t, "hi\n")
	if err != nil {
		t.Fatalf("capture: %v\n%s", err, out)
	}
	rel := strings.TrimSpace(out)
	if !strings.HasPrefix(rel, "inbox/") || !strings.HasSuffix(rel, ".md") {
		t.Fatalf("stdout should be the mount-relative path, got %q", out)
	}
	body, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("the printed path does not exist: %v", err)
	}
	if string(body) != "hi\n" {
		t.Errorf("bytes were not written verbatim: %q", body)
	}
	// The path is the whole contract for a script reading this.
	if len(strings.Split(strings.TrimSpace(out), "\n")) != 1 {
		t.Errorf("stdout should hold nothing but the path:\n%s", out)
	}
}

// Run from a subdirectory, the capture still lands in the MOUNT ROOT's inbox/
// — that is what pairs with the hub path teammates see.
func TestCaptureFromSubdirLandsAtRoot(t *testing.T) {
	folder := captureMount(t)
	sub := filepath.Join(folder, "wiki", "docs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(sub)

	out, _, err := captureRun(t, "from a subdir\n")
	if err != nil {
		t.Fatalf("capture: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(out); !strings.HasPrefix(got, "inbox/") {
		t.Fatalf("path should be root-relative, got %q", got)
	}
	if _, err := os.Stat(filepath.Join(sub, "inbox")); err == nil {
		t.Error("capture created an inbox/ in the subdirectory")
	}
	if len(inboxFiles(t, folder)) != 1 {
		t.Errorf("expected one file in the root inbox/, got %v", inboxFiles(t, folder))
	}
}

// A TTY on stdin means nobody piped anything and the command would otherwise
// look frozen. Refuse, and say what a pipe looks like.
func TestCaptureRefusesTTY(t *testing.T) {
	folder := captureMount(t)
	orig := isInteractive
	isInteractive = func() bool { return true }
	t.Cleanup(func() { isInteractive = orig })

	out, _, err := captureRun(t, "ignored")
	if err == nil {
		t.Fatalf("capture with a TTY on stdin succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "pipe something in") {
		t.Errorf("error should tell the user to pipe: %v", err)
	}
	if f := inboxFiles(t, folder); f != nil {
		t.Errorf("a refused capture wrote %v", f)
	}
}

func TestCaptureRefusesEmptyStdin(t *testing.T) {
	folder := captureMount(t)

	out, _, err := captureRun(t, "")
	if err == nil {
		t.Fatalf("capture of empty stdin succeeded:\n%s", out)
	}
	if f := inboxFiles(t, folder); f != nil {
		t.Errorf("empty stdin still wrote %v", f)
	}
}

// Two captures inside the same second must both survive: the second gets -2.
func TestCaptureCollisionSuffix(t *testing.T) {
	folder := captureMount(t)

	first, _, err := captureRun(t, "one\n")
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := captureRun(t, "two\n")
	if err != nil {
		t.Fatal(err)
	}
	a, b := strings.TrimSpace(first), strings.TrimSpace(second)
	if a == b {
		t.Fatalf("both captures claimed %q", a)
	}
	// Same second in practice; if the clock ticked between them the names
	// differ anyway and the only thing worth asserting is that both survived.
	if strings.HasPrefix(b, strings.TrimSuffix(a, ".md")) && !strings.HasSuffix(b, "-2.md") {
		t.Errorf("a same-second collision should suffix -2, got %q after %q", b, a)
	}
	for path, want := range map[string]string{a: "one\n", b: "two\n"} {
		got, err := os.ReadFile(filepath.Join(folder, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if string(got) != want {
			t.Errorf("%s holds %q, want %q — one capture clobbered the other", path, got, want)
		}
	}
}

// A project narrowed with `bdrive scope add docs` excludes inbox/, so a
// capture there would sit on this laptop forever without ever syncing. Refuse
// before writing — and leave no empty inbox/ behind either.
func TestCaptureRefusesOutOfScope(t *testing.T) {
	folder := captureMount(t)
	if err := os.WriteFile(filepath.Join(folder, ".bdriveignore"), []byte("/*\n!/docs/\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _, err := captureRun(t, "would never sync\n")
	if err == nil {
		t.Fatalf("capture into an out-of-scope inbox/ succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "bdrive scope add inbox") {
		t.Errorf("the refusal should name the fix: %v", err)
	}
	if f := inboxFiles(t, folder); f != nil {
		t.Errorf("a refused capture wrote %v", f)
	}
	if _, err := os.Stat(filepath.Join(folder, "inbox")); err == nil {
		t.Error("a refused capture left an empty inbox/ behind")
	}
}

// The scope gate reads the rules this device ACCEPTED, not just the live
// file: a teammate's pulled `!inbox/` widening rule is not what the cycle
// uploads by, so it must not read as "syncs".
func TestCaptureScopeGateUsesAcceptedRules(t *testing.T) {
	folder := captureMount(t)
	// Live rules say inbox/ is back in scope...
	if err := os.WriteFile(filepath.Join(folder, ".bdriveignore"), []byte("/*\n!/docs/\n!/inbox/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// ...but this device accepted the narrower set, which is the floor the
	// scan applies. Nothing widens until this machine's user authors it.
	proj, _, err := config.LoadProject(folder)
	if err != nil {
		t.Fatal(err)
	}
	writeAccepted(t, proj.ID, "/*\n!/docs/\n")

	out, _, err := captureRun(t, "widened only on their machine\n")
	if err == nil {
		t.Fatalf("capture succeeded on a rule this device never accepted:\n%s", out)
	}
	if f := inboxFiles(t, folder); f != nil {
		t.Errorf("a refused capture wrote %v", f)
	}
}

func TestCaptureOutsideProject(t *testing.T) {
	t.Setenv("BDRIVE_HOME", t.TempDir())
	t.Chdir(t.TempDir())

	out, _, err := captureRun(t, "nowhere to put this\n")
	if err == nil {
		t.Fatalf("capture outside a project succeeded:\n%s", out)
	}
	if !strings.Contains(err.Error(), "bdrive init") {
		t.Errorf("expected the not-a-project error, got: %v", err)
	}
}

// writeAccepted records the ignore rules a device has accepted, which is what
// the scan (and so the scope gate) applies as its floor.
func writeAccepted(t *testing.T, projID, rules string) {
	t.Helper()
	vdir, err := config.VolumeDir(projID)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(vdir)
	if err != nil {
		t.Fatal(err)
	}
	sync, err := st.LoadSync()
	if err != nil {
		t.Fatal(err)
	}
	sync.IgnoreAccepted = rules
	if err := st.SaveSync(sync); err != nil {
		t.Fatal(err)
	}
}
