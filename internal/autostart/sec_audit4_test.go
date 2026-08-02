package autostart

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Round 9 — the coverage audit of scoreboard row 20's autostart half.
//
// Two guards here survive being deleted with the whole TestSec suite green:
// loginPath's refusal of a control character in the binary path, and
// writeIfDifferent going through store.WriteFileAtomic rather than
// os.WriteFile. Round 5's test covers the atomic write's TEMP name; the
// DESTINATION — the thing os.WriteFile actually follows — was never asserted.
//
// Helper prefix: secaud4.

// secaud4ChildEnv makes a copy of this test binary act as `bdrive` rather than
// re-running the suite, so os.Executable() (and therefore selfPath) reports
// whatever hostile path the parent copied it to.
const secaud4ChildEnv = "BDRIVE_SEC_AUDIT4_CHILD"

func init() {
	if os.Getenv(secaud4ChildEnv) == "" {
		return
	}
	r, err := Install()
	if err != nil {
		io.WriteString(os.Stderr, "install: "+err.Error())
		os.Exit(3)
	}
	io.WriteString(os.Stdout, r.Path)
	os.Exit(0)
}

// TestSec_Autostart_ABinaryPathWithAControlCharacterIsNeverRegistered
//
// The registration is a command line a service manager runs at EVERY login,
// before any guard of ours. Neither format escapes control characters: a
// newline in the path ends systemd's `ExecStart=` line and everything after it
// is read as a NEW unit directive — another ExecStart, an Environment=, a
// User= — none of which the person running `bdrive init` ever saw. loginPath
// therefore refuses the path outright rather than writing a login command
// nobody reviewed.
//
// Nothing asserted that. Round 5's hostile-path test uses "&" and "<", which
// exercise the plist's XML escaping, not this refusal; deleting the loop leaves
// every test in the repo green.
//
// The secure behaviour: Install fails loudly and writes no registration at all.
func TestSec_Autostart_ABinaryPathWithAControlCharacterIsNeverRegistered(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows Run value is a registry write, not a file")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// Control: from an ordinary path the same fixture registers successfully,
	// so a refusal below is loginPath's decision and not a broken child.
	out, home, err := secaud4Install(t, self, "plainbin")
	if err != nil {
		if strings.Contains(err.Error(), ErrUnsupported.Error()) {
			t.Skipf("autostart unsupported here: %v", err)
		}
		t.Fatalf("control: Install from a plain path failed: %v (%s)", err, out)
	}
	if _, err := os.Stat(out); err != nil {
		t.Fatalf("control: no registration written at %s: %v", out, err)
	}
	_ = home

	// A directory whose name carries a newline — legal on any POSIX
	// filesystem, and the one character that starts a new unit directive.
	out, home, err = secaud4Install(t, self, "a\nb")
	if err == nil {
		t.Errorf("Install registered a login command for a binary path containing a newline; "+
			"it wrote %s", out)
	}
	if reg := secaud4RegistrationPath(home); reg != "" {
		if body, rerr := os.ReadFile(reg); rerr == nil {
			t.Errorf("a registration was written anyway at %s:\n%s", reg, body)
		}
	}
}

// secaud4Install copies the test binary to <tmpHome>/<dir>/bdrive and runs it
// as the child with an isolated HOME, so Install() writes a throwaway
// registration. Returns the child's stdout (the registration path on success),
// the temp home, and the child's error.
func secaud4Install(t *testing.T, self, dir string) (string, string, error) {
	t.Helper()
	home := t.TempDir()
	bindir := filepath.Join(home, dir)
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Skipf("cannot create a directory named %q: %v", dir, err)
	}
	exe := filepath.Join(bindir, "bdrive")
	src, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(exe, src, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(),
		secaud4ChildEnv+"=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), home, err
}

// secaud4RegistrationPath is where this platform's registration would land
// under the given home.
func secaud4RegistrationPath(home string) string {
	switch runtime.GOOS {
	case "darwin":
		return filepath.Join(home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist")
	case "linux":
		return filepath.Join(home, ".config", "systemd", "user", "beardrive.service")
	}
	return ""
}

// TestSec_Autostart_TheRegistrationIsNotWrittenThroughASymlinkAtItsOwnPath
//
// writeIfDifferent goes through store.WriteFileAtomic, which writes a temp file
// and renames it over the destination — so a symlink AT the destination is
// replaced, not followed. Plain os.WriteFile follows it and truncates whatever
// it names.
//
// Round 5 asserted the temp-name half of this (a symlink planted at the
// predictable ".bdrive-tmp-" name). The destination half is the bigger one:
// ~/Library/LaunchAgents/ai.beardrive.daemon.plist and
// ~/.config/systemd/user/beardrive.service are fixed, world-known paths in the
// user's own home, and `bdrive init` writes them unprompted. A link left at
// either name turns "register autostart" into "truncate and overwrite that
// file with our unit text" — and leaves no registration behind either, so
// sync silently never resumes after a reboot.
//
// The secure behaviour: the file the link points at is untouched.
func TestSec_Autostart_TheRegistrationIsNotWrittenThroughASymlinkAtItsOwnPath(t *testing.T) {
	dir := t.TempDir()
	reg := filepath.Join(dir, "beardrive.service")

	victim := filepath.Join(t.TempDir(), "authorized_keys")
	const secret = "ssh-ed25519 AAAA... the user's own key\n"
	if err := os.WriteFile(victim, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, reg); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	const content = "[Service]\nExecStart=/usr/bin/bdrive resume\n"
	// The write may fail (a fine outcome) but it must not reach through the link.
	if _, err := writeIfDifferent(reg, content); err != nil {
		t.Logf("writeIfDifferent returned %v", err)
	}

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("the file the link named is gone: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("writeIfDifferent followed the symlink at the registration path and "+
			"overwrote %s:\nnow  = %q\nwant = %q", victim, got, secret)
	}

	// Control: with no link in the way, the same call does write the file —
	// so the assertion above is about the link, not about a no-op writer.
	plain := filepath.Join(t.TempDir(), "beardrive.service")
	if _, err := writeIfDifferent(plain, content); err != nil {
		t.Fatalf("control: %v", err)
	}
	if body, err := os.ReadFile(plain); err != nil || string(body) != content {
		t.Fatalf("control: writeIfDifferent did not write the registration: %q, %v", body, err)
	}
}
