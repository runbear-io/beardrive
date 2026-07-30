//go:build linux

package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests redirect HOME (and sometimes XDG_CONFIG_HOME), so nothing
// touches the developer's real ~/.config/systemd/user. That isolation only
// holds because Install writes files and stops — if it ever shells out to
// `systemctl --user`, it would talk to the real session bus.
//
// systemd must look booted for Install to do anything (see booted()); a
// container without /run/systemd/system exercises the other branch, in
// TestInstallNeedsSystemd.
func requireSystemd(t *testing.T) {
	t.Helper()
	if !booted() {
		t.Skip("no /run/systemd/system: this environment is covered by TestInstallNeedsSystemd")
	}
}

func TestInstallWritesUnitAndEnablesIt(t *testing.T) {
	requireSystemd(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	if Installed() {
		t.Fatal("a fresh HOME cannot have the unit installed")
	}
	res, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Fatal("first Install reported no change")
	}
	wantPath := filepath.Join(home, ".config", "systemd", "user", "beardrive.service")
	if res.Path != wantPath {
		t.Fatalf("path = %s, want %s", res.Path, wantPath)
	}
	if !Installed() {
		t.Fatal("Installed() false right after Install")
	}

	body, err := os.ReadFile(wantPath)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := selfPath()
	for _, frag := range []string{
		"ExecStart=" + exe + " resume --quiet", // one job for every mount
		"Type=oneshot",                         // resume exits; that is not a failure
		"WantedBy=default.target",              // what the enable symlink answers
	} {
		if !strings.Contains(string(body), frag) {
			t.Errorf("unit missing %q:\n%s", frag, body)
		}
	}
	// Restart=/network-online would either fight the by-design exit or delay
	// local scanning for a hub that sync already retries on its own.
	for _, unwanted := range []string{"Restart=", "network-online"} {
		if strings.Contains(string(body), unwanted) {
			t.Errorf("unit should not mention %q:\n%s", unwanted, body)
		}
	}

	// Enabled means a symlink systemd will read, not just a unit on disk.
	link := filepath.Join(filepath.Dir(wantPath), "default.target.wants", "beardrive.service")
	target, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("unit was written but never enabled: %v", err)
	}
	if target != filepath.Join("..", "beardrive.service") {
		t.Errorf("enable symlink points at %q", target)
	}
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil || resolved != wantPath {
		t.Errorf("enable symlink resolves to %q (err %v), want %s", resolved, err, wantPath)
	}

	// Idempotent: init calls this on every run.
	if res, err = Install(); err != nil || res.Changed {
		t.Errorf("second Install = (%+v, %v), want no change", res, err)
	}

	// No temp file may survive the atomic write.
	entries, _ := os.ReadDir(filepath.Dir(wantPath))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".bdrive-tmp-") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// A unit nothing wants never starts, so a missing or wrong symlink has to be
// repaired — and must not count as installed while it is broken.
func TestEnableSymlinkIsRepaired(t *testing.T) {
	requireSystemd(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	link := filepath.Join(filepath.Dir(path), "default.target.wants", "beardrive.service")

	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if Installed() {
		t.Error("a unit with no enable symlink counts as installed")
	}
	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("Install did not report re-enabling the unit")
	}
	if !Installed() {
		t.Fatal("symlink not restored")
	}

	// A link pointing somewhere else (an older layout, a hand edit) is replaced.
	os.Remove(link)
	if err := os.Symlink("/nonexistent/beardrive.service", link); err != nil {
		t.Fatal(err)
	}
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	if target, _ := os.Readlink(link); target != filepath.Join("..", "beardrive.service") {
		t.Errorf("wrong symlink survived: %q", target)
	}
}

// A stale ExecStart (binary moved or upgraded) must be rewritten, not skipped.
func TestInstallRewritesStaleBinaryPath(t *testing.T) {
	requireSystemd(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	path, _ := Path()
	if err := os.WriteFile(path, []byte(unit("/old/path/bdrive")), 0o644); err != nil {
		t.Fatal(err)
	}
	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("Install left a unit pointing at the wrong binary")
	}
	body, _ := os.ReadFile(path)
	if strings.Contains(string(body), "/old/path/bdrive") {
		t.Error("stale ExecStart survived")
	}
}

func TestUninstallRemovesBoth(t *testing.T) {
	requireSystemd(t)
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")

	// Removing what was never there is success, not an error.
	if res, err := Uninstall(); err != nil || res.Changed {
		t.Fatalf("Uninstall on a clean HOME = (%+v, %v), want no change and no error", res, err)
	}
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall()
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Changed {
		t.Error("Uninstall reported no change while a unit existed")
	}
	path, _ := Path()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("unit file survived Uninstall")
	}
	link := filepath.Join(filepath.Dir(path), "default.target.wants", "beardrive.service")
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Error("enable symlink survived Uninstall")
	}
	if Installed() {
		t.Error("Installed() true after Uninstall")
	}
}

// Desktops and distros that relocate XDG_CONFIG_HOME must still get a unit
// systemd will read.
func TestHonorsXDGConfigHome(t *testing.T) {
	requireSystemd(t)
	home := t.TempDir()
	cfg := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", cfg)

	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(cfg, "systemd", "user", "beardrive.service")
	if res.Path != want {
		t.Fatalf("path = %s, want %s", res.Path, want)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); err == nil {
		t.Error("wrote under ~/.config despite XDG_CONFIG_HOME")
	}
}

// Without systemd a unit file is inert decoration: reporting "registered"
// would be a lie, so Install must decline instead.
func TestInstallNeedsSystemd(t *testing.T) {
	if booted() {
		t.Skip("systemd present: the supported path is covered by the other tests")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	if _, err := Install(); err != ErrUnsupported {
		t.Fatalf("Install on a non-systemd machine = %v, want ErrUnsupported", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "systemd")); err == nil {
		t.Error("Install wrote a unit that nothing would ever start")
	}
}
