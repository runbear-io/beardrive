package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Round 10 — autostart's Uninstall, and the write path Install shares with it.
// Neither had ever been driven by a TestSec_ test; round 9 recorded the whole
// package as "never exercised by anything, in any round" on the platform halves
// and nothing at all on Uninstall.
//
// Everything here runs on the host's real GOOS. The linux and windows
// implementations sit behind build tags and cannot be reached from a darwin
// run — that is recorded as NOT REACHED, not as clean.

// sec10Home isolates $HOME so Path() resolves under a temp dir.
func sec10Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	return home
}

// sec10Path is Path() or a skip on a platform that has none.
func sec10Path(t *testing.T) string {
	t.Helper()
	p, err := Path()
	if err != nil {
		t.Skipf("no registration path on %s: %v", runtime.GOOS, err)
	}
	return p
}

// sec10Install runs Install, skipping on the platforms that have no
// implementation rather than reporting a failure they cannot have.
func sec10Install(t *testing.T) Result {
	t.Helper()
	res, err := Install()
	if errors.Is(err, ErrUnsupported) {
		t.Skipf("autostart unsupported on %s", runtime.GOOS)
	}
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	return res
}

// ---------------------------------------------------------------------------
// 1. The mode of the file that becomes a login command.
// ---------------------------------------------------------------------------

// TestSec_Autostart_InstallReassertsTheRegistrationMode
//
// The registration IS the login command: launchd (and systemd, and the Run
// key) reads this file and executes what it names, as the user, at every
// login, before any guard beardrive has. So its permissions are the whole
// security boundary around it, and WriteFileAtomic sets 0644 on the way in.
//
// writeIfDifferent short-circuits on CONTENT only:
//
//	if have, err := os.ReadFile(path); err == nil && string(have) == content {
//	        return false, nil
//	}
//
// A registration whose mode has drifted to group- or world-writable — a
// restored backup, an extracted tarball, a permissive umask on an older build,
// another tool — is therefore never corrected. `bdrive init` calls Install on
// every single run and is the documented self-heal; it reports "already
// correct" while any local account can rewrite the command that runs as you at
// the next login.
//
// The delta: Install with DIFFERENT content does go through WriteFileAtomic
// and lands 0644. Same call, same file, same user — only the content
// comparison decides whether the mode is re-asserted.
func TestSec_Autostart_InstallReassertsTheRegistrationMode(t *testing.T) {
	sec10Home(t)
	sec10Install(t)
	path := sec10Path(t)

	// Control: a fresh install lands a mode nobody else can write.
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("fixture: a fresh registration is already %v", fi.Mode().Perm())
	}

	// Somebody widens it. Content is untouched.
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	fi, err = os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm&0o022 != 0 {
		t.Errorf("Install over a world-writable registration left it %v (Changed=%v) — "+
			"%s is a command a service manager runs as this user at every login, and "+
			"`bdrive init` calls Install on every run precisely so a drifted registration "+
			"is repaired", perm, res.Changed, path)
	}
}

// ---------------------------------------------------------------------------
// 2. Uninstall over a symlinked registration path.
// ---------------------------------------------------------------------------

// TestSec_Autostart_UninstallDoesNotFollowASymlinkAtTheRegistrationPath
//
// The assignment's question for this round: can Uninstall remove or truncate
// something it should not. It stats (which FOLLOWS a link) and then removes
// (which does not), so a link planted at the registration path must cost the
// link and never its target.
//
// Asserted rather than assumed: the same file also has to survive Uninstall
// deciding there is "nothing installed", which is the other branch.
func TestSec_Autostart_UninstallDoesNotFollowASymlinkAtTheRegistrationPath(t *testing.T) {
	sec10Home(t)
	path := sec10Path(t)

	victim := filepath.Join(t.TempDir(), "someone-elses.plist")
	if err := os.WriteFile(victim, []byte("not beardrive's\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(victim, path); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if _, err := Uninstall(); err != nil && !errors.Is(err, ErrUnsupported) {
		t.Fatal(err)
	}
	if _, err := os.Stat(victim); err != nil {
		t.Errorf("Uninstall deleted %s, the target of a symlink planted at the registration "+
			"path — it must only ever remove its own registration: %v", victim, err)
	}
}

// TestSec_Autostart_UninstallLeavesNoRegistrationAndIsIdempotent
//
// Partial state is the failure this asks about: after Uninstall nothing may
// remain that a service manager would still load, and a second Uninstall must
// be a clean no-op rather than an error the caller has to distinguish from a
// real failure (bdrive treats autostart as best-effort, so an error here is
// noise on a path the user cannot fix).
func TestSec_Autostart_UninstallLeavesNoRegistrationAndIsIdempotent(t *testing.T) {
	sec10Home(t)
	sec10Install(t)
	path := sec10Path(t)

	if !Installed() {
		t.Fatal("fixture: Install did not register")
	}
	res, err := Uninstall()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Errorf("Uninstall over a live registration reported Changed=false")
	}
	if Installed() {
		t.Errorf("Installed() still true after Uninstall — %s survives", path)
	}
	if _, err := os.Lstat(path); !os.IsNotExist(err) {
		t.Errorf("something remains at %s after Uninstall (lstat err = %v)", path, err)
	}
	// Second run: missing is success.
	res2, err := Uninstall()
	if err != nil {
		t.Errorf("second Uninstall errored: %v", err)
	}
	if res2.Changed {
		t.Errorf("second Uninstall reported Changed=true with nothing to remove")
	}
}

// TestSec_Autostart_UninstallDoesNotEscapeTheRegistrationPath
//
// Path() is built from $HOME. Everything Uninstall removes has to be that one
// file — no walk, no glob, no sibling. Planting decoys around it and in the
// same directory proves the removal is exact.
func TestSec_Autostart_UninstallDoesNotEscapeTheRegistrationPath(t *testing.T) {
	sec10Home(t)
	sec10Install(t)
	path := sec10Path(t)

	dir := filepath.Dir(path)
	decoys := []string{
		filepath.Join(dir, "com.someoneelse.agent.plist"),
		filepath.Join(dir, filepath.Base(path)+".bak"),
		filepath.Join(dir, "beardrive.service"),
	}
	// On Linux the registration IS <unitDir>/beardrive.service, so that third
	// decoy is the file under test: the test would plant its own registration
	// and then assert Uninstall left it alone. Dropped there, not skipped —
	// the other two decoys still measure the property, which is that removal
	// touches exactly Path() and nothing beside it.
	var keep []string
	for _, d := range decoys {
		if d == path {
			continue
		}
		keep = append(keep, d)
	}
	decoys = keep
	for _, d := range decoys {
		if err := os.WriteFile(d, []byte("keep me\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	for _, d := range decoys {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("Uninstall removed %s, which is not beardrive's registration: %v", d, err)
		}
	}
}

// ---------------------------------------------------------------------------
// 3. What the registration is allowed to say.
// ---------------------------------------------------------------------------

// TestSec_Autostart_RegistrationNamesOnlyThisBinaryAndResume
//
// The one thing a login registration must never become is a command somebody
// else chose. loginPath refuses a control character in the binary path, and
// the darwin renderer XML-escapes it; assert from the other end — that what
// lands on disk names the running binary and the single subcommand `resume`,
// with nothing else executable in it.
func TestSec_Autostart_RegistrationNamesOnlyThisBinaryAndResume(t *testing.T) {
	sec10Home(t)
	sec10Install(t)
	path := sec10Path(t)

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	exe, err := selfPath()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(body, "resume") {
		t.Errorf("registration does not name `resume`:\n%s", body)
	}
	// A shell is what turns a registration into an injection surface.
	for _, bad := range []string{"sh -c", "/bin/sh", "bash -c", ";", "&&", "|"} {
		if strings.Contains(body, bad) {
			t.Errorf("registration contains %q — a login command must be an argv, not a shell line:\n%s", bad, body)
		}
	}
	if !strings.Contains(body, filepath.Base(exe)) {
		t.Errorf("registration does not name this binary (%s):\n%s", exe, body)
	}
	for _, r := range body {
		if r < 0x20 && r != '\n' && r != '\t' {
			t.Errorf("registration carries control byte %#x — loginPath refuses these in the path, "+
				"and nothing else in the file should introduce one", r)
			break
		}
	}
}
