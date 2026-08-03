//go:build linux

package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// Round 11 — booted(), the one line round 10 executed for the first time and
// then left undecided: "the hacker could not spoof it without root and did not
// try."
//
// booted() is the gate on the whole Linux registration:
//
//	func booted() bool {
//		fi, err := os.Stat("/run/systemd/system")
//		return err == nil && fi.IsDir()
//	}
//
// Both wrong answers are real. A false NEGATIVE makes Install() return
// ErrUnsupported on a machine systemd would have honoured, so sync never
// resumes after a reboot and `bdrive init` says nothing about it. A false
// POSITIVE makes Install() write a unit and a wants symlink, report success,
// and register nothing that will ever run — the failure the package doc says
// this check exists to prevent.
//
// Deciding it properly means answering one question: can an UNPRIVILEGED
// process make it answer wrong? These tests answer it with evidence rather
// than with reasoning, and pin the answer.
//
// Helpers are prefixed sec11. Run them on Linux:
//
//	GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go test -c -o /tmp/autostart.linux.test ./internal/autostart/
//	docker run --rm -v /tmp:/w node:22-slim sh -c 'cd /tmp && /w/autostart.linux.test -test.run TestSec_Booted -test.v'

// sec11Sysd is the one absolute path booted() consults.
const sec11Sysd = "/run/systemd/system"

// sec11Env sets every environment variable that could plausibly be read as a
// filesystem root, each pointing at a directory that DOES contain a
// run/systemd/system. If booted() consults any of them, it flips.
func sec11Env(t *testing.T) string {
	t.Helper()
	fake := t.TempDir()
	if err := os.MkdirAll(filepath.Join(fake, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{
		"HOME", "XDG_CONFIG_HOME", "XDG_RUNTIME_DIR", "XDG_DATA_HOME",
		"TMPDIR", "ROOT", "SYSTEMD_ROOT", "PREFIX", "DESTDIR",
	} {
		t.Setenv(k, fake)
	}
	t.Setenv("PATH", filepath.Join(fake, "bin")+":"+os.Getenv("PATH"))
	return fake
}

// TestSec_Booted_IsNotSteerableByTheEnvironment
//
// The environment is the only channel an unprivileged process reliably owns
// on someone else's behalf: a login shell's profile, a wrapper script, a
// desktop launcher, an agent session's env block. If booted() resolved its
// path through any of them — a chroot-ish ROOT, a relative path, an XDG root
// — then "is systemd the init system" would be a question the caller's
// environment answers, and the gate on writing a login command would be
// caller-controlled.
//
// The secure behavior asserted: booted() is a fixed absolute path and returns
// the same answer whatever the environment says, and Install()'s refusal
// tracks it.
func TestSec_Booted_IsNotSteerableByTheEnvironment(t *testing.T) {
	before := booted()

	fake := sec11Env(t)
	if got := booted(); got != before {
		t.Errorf("booted() went %v → %v when HOME/XDG_*/TMPDIR/ROOT were pointed at %s, which "+
			"contains run/systemd/system — whether a login command gets registered is decided "+
			"by the caller's environment", before, got, fake)
	}

	// And the decision Install() makes from it moves in step: ErrUnsupported
	// is returned exactly when booted() is false.
	sec10Home(t) // isolate the unit directory before Install may write to it
	_, err := Install()
	if errors.Is(err, ErrUnsupported) && booted() {
		t.Error("Install() returned ErrUnsupported while booted() is true")
	}
	if !errors.Is(err, ErrUnsupported) && !booted() {
		t.Errorf("Install() did not refuse (%v) while booted() is false — the registration is "+
			"written on a machine that will never read it", err)
	}
}

// TestSec_Booted_CannotBeTurnedOnWithoutPrivilege
//
// The evidence half. /run is a root-owned tmpfs mounted 0755 on every systemd
// and non-systemd Linux alike, so an unprivileged process cannot create
// /run/systemd/system — and if it cannot, there is no unprivileged path to a
// false positive, and this line is safe to keep as the gate.
//
// Asserted, not argued: try it, and require the kernel to refuse.
//
// Skipped as root (where it is trivially possible and means nothing) and on a
// real systemd box (where the directory already exists and there is nothing to
// create).
func TestSec_Booted_CannotBeTurnedOnWithoutPrivilege(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root: creating /run/systemd/system proves nothing about an attacker")
	}
	if booted() {
		t.Skip("systemd is booted here: /run/systemd/system already exists")
	}

	if err := os.MkdirAll(sec11Sysd, 0o755); err == nil {
		t.Errorf("an unprivileged process created %s — booted() is spoofable, so Install() can "+
			"be made to write a systemd unit and a default.target.wants symlink and report "+
			"success on a machine where nothing will ever read them", sec11Sysd)
	} else if !os.IsPermission(err) {
		// ENOENT on a missing /run, EROFS on a read-only one — both are the
		// kernel refusing. Anything else means it got further than it should.
		t.Logf("mkdir %s refused with %v (not EACCES, still a refusal)", sec11Sysd, err)
	}
	if booted() {
		t.Errorf("booted() reports systemd after an unprivileged mkdir attempt")
	}

	// Nor may the path be resolved relative to a working directory, which
	// every process controls: `bdrive init` runs wherever the user is standing.
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "run", "systemd", "system"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(dir)
	if booted() {
		t.Errorf("booted() answered true from the working directory %s, which holds a "+
			"run/systemd/system — the path is resolved relative to the caller's cwd", dir)
	}
}

// TestSec_Booted_AnUnbootedSystemGetsNoRegistrationAtAll
//
// The fail-closed half, and the one that has to hold whichever way booted()
// answers. Install() checks booted() FIRST and returns before it computes a
// path — so on a machine where systemd is not the init system nothing may be
// created: no unit file, no default.target.wants directory, no symlink. A
// half-written registration on an Alpine/runit box or in a slim container is a
// file that outlives the check and gets picked up by whatever reads that
// directory later.
func TestSec_Booted_AnUnbootedSystemGetsNoRegistrationAtAll(t *testing.T) {
	if booted() {
		t.Skip("systemd is booted here; this asserts the refusal path")
	}
	home := sec10Home(t)

	res, err := Install()
	if !errors.Is(err, ErrUnsupported) {
		t.Fatalf("Install() on a machine without systemd returned (%+v, %v), want ErrUnsupported", res, err)
	}
	if res.Path != "" || res.Changed {
		t.Errorf("a refused Install() still reported %+v", res)
	}

	// Nothing under $HOME/.config may exist as a result.
	var found []string
	_ = filepath.Walk(home, func(p string, fi os.FileInfo, err error) error {
		if err == nil && p != home {
			found = append(found, p)
		}
		return nil
	})
	if len(found) != 0 {
		t.Errorf("a refused Install() created %q under %s — the unit and its wants symlink are "+
			"exactly what the booted() check exists to not write", found, home)
	}
	if Installed() {
		t.Error("Installed() reports a registration after a refused Install()")
	}
}
