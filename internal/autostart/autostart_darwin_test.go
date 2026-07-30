//go:build darwin

package autostart

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every test here redirects HOME (os.UserHomeDir reads it), so nothing touches
// the developer's real ~/Library/LaunchAgents. That isolation is only safe
// because Install writes a file and stops — if it ever shells out to
// launchctl, these tests would register a real login item.
func TestInstallWritesAndIsIdempotent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := filepath.Join(home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist")
	if Installed() {
		t.Fatal("a fresh HOME cannot have the agent installed")
	}

	res, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatal("first Install reported no change")
	}
	if res.Path != want {
		t.Fatalf("path = %s, want %s", res.Path, want)
	}
	if !Installed() {
		t.Fatal("Installed() false right after Install")
	}

	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := os.Executable()
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	for _, frag := range []string{
		"<string>" + exe + "</string>", // the binary that installed it, not $PATH
		"<string>resume</string>",      // one job for every mount, not one per project
		"<key>RunAtLoad</key>",
	} {
		if !strings.Contains(string(body), frag) {
			t.Errorf("plist missing %q:\n%s", frag, body)
		}
	}
	// KeepAlive would respawn `bdrive resume` forever: it starts the daemons
	// and exits, which launchd would read as a crash.
	if strings.Contains(string(body), "KeepAlive") {
		t.Error("plist sets KeepAlive on a command that exits by design")
	}

	// Idempotent: init calls this on every run.
	res, err = Install()
	if err != nil {
		t.Fatalf("second Install: %v", err)
	}
	if res.Changed {
		t.Error("second Install rewrote an identical plist")
	}

	// A stale binary path (Homebrew prefix change, a moved binary) must be
	// corrected rather than left alone.
	if err := os.WriteFile(want, []byte(plist("/old/path/bdrive")), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err = Install()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("Install left a plist pointing at the wrong binary")
	}

	// No temp file may survive the atomic write.
	entries, _ := os.ReadDir(filepath.Dir(want))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bdrive-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

func TestUninstall(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Removing what was never there is success, not an error.
	res, err := Uninstall()
	if err != nil || res.Changed {
		t.Fatalf("Uninstall on a clean HOME = (%+v, %v), want no change and no error", res, err)
	}

	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	res, err = Uninstall()
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Changed {
		t.Error("Uninstall reported no change while a plist existed")
	}
	if Installed() {
		t.Error("plist survived Uninstall")
	}
}

// launchd has to be able to parse it — a plist that only looks like XML would
// fail silently at login, which is the one moment nobody is watching.
func TestPlistIsValid(t *testing.T) {
	if _, err := exec.LookPath("plutil"); err != nil {
		t.Skip("no plutil")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("plutil", "-lint", res.Path).CombinedOutput()
	if err != nil {
		t.Fatalf("plutil -lint failed: %v\n%s", err, out)
	}
}
