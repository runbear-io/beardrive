package autostart

import (
	"encoding/xml"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Round 5 security tests for internal/autostart. The unit this package writes
// is a command line that a service manager executes at EVERY login, before any
// guard runs, so everything interpolated into it and every file operation that
// produces it is a trust boundary.
//
// Helper prefix: secdmn (shared with internal/daemon's round-5 file).

// secdmnChildEnv makes a copy of this test binary act as `bdrive` rather than
// re-running the suite: without it the re-exec below would run every test in
// the package again, recursively.
const secdmnChildEnv = "BDRIVE_SEC_AUTOSTART_CHILD"

func init() {
	if os.Getenv(secdmnChildEnv) == "" {
		return
	}
	// We are the "bdrive" binary the parent copied to a hostile path.
	// os.Executable() — and therefore selfPath() — now reports that path.
	r, err := Install()
	if err != nil {
		io.WriteString(os.Stderr, "install: "+err.Error())
		os.Exit(3)
	}
	io.WriteString(os.Stdout, r.Path)
	os.Exit(0)
}

// TestSec_Autostart_LoginCommandSurvivesAHostileBinaryPath
//
// selfPath() is pasted straight into the registration — the plist's
// <string>, systemd's ExecStart=, the Windows Run value — with no escaping for
// the format it lands in. A path component is allowed to contain almost
// anything on a POSIX filesystem; "&" and "<" in particular are ordinary in
// macOS folder names ("Music & Video", "Docs <archive>").
//
// The registration is the whole job of this package ("writing the file IS the
// registration"), so a file the service manager cannot parse is a silent
// failure of the one thing autostart exists to prevent: a reboot stops sync
// and nothing brings it back. Install() reports success either way.
//
// The assertion is the secure one: whatever the binary path is, the
// registration must round-trip and must name exactly that binary.
func TestSec_Autostart_LoginCommandSurvivesAHostileBinaryPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows Run value is a registry write, not a file; see the report's Suspicions")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}

	// A control run from a boring path proves the fixture works, so a failure
	// on the hostile path is this package's decision and not the harness's.
	t.Run("plain path", func(t *testing.T) {
		path, home := secdmnInstallFrom(t, self, "plainbin")
		secdmnAssertRegistrationNames(t, path, filepath.Join(home, "plainbin", "bdrive"))
	})

	t.Run("path with XML and shell metacharacters", func(t *testing.T) {
		path, home := secdmnInstallFrom(t, self, "a&b<c")
		secdmnAssertRegistrationNames(t, path, filepath.Join(home, "a&b<c", "bdrive"))
	})
}

// secdmnInstallFrom copies the test binary to <tmpHome>/<dir>/bdrive and runs
// it as the child, with HOME (and XDG_CONFIG_HOME) pointed at a fresh temp
// dir, so Install() writes a throwaway registration and selfPath() reports the
// hostile path. Returns the written registration's path and the temp home.
func secdmnInstallFrom(t *testing.T, self, dir string) (string, string) {
	t.Helper()
	home := t.TempDir()
	bindir := filepath.Join(home, dir)
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		t.Fatal(err)
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
		secdmnChildEnv+"=1",
		"HOME="+home,
		"XDG_CONFIG_HOME="+filepath.Join(home, ".config"),
	)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && strings.Contains(string(ee.Stderr), ErrUnsupported.Error()) {
			t.Skipf("autostart unsupported here: %s", ee.Stderr)
		}
		t.Fatalf("child Install: %v (%s)", err, out)
	}
	return strings.TrimSpace(string(out)), home
}

// secdmnAssertRegistrationNames parses the written registration in the format
// its service manager will parse it, and checks it names exactly wantExe.
func secdmnAssertRegistrationNames(t *testing.T, path, wantExe string) {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("registration not written: %v", err)
	}
	// selfPath() resolves symlinks, and macOS's /var is one; compare against
	// the same resolution so the assertion is about escaping, not about /private.
	if resolved, err := filepath.EvalSymlinks(wantExe); err == nil {
		wantExe = resolved
	}
	switch runtime.GOOS {
	case "darwin":
		// launchd parses this as XML. Anything that does not parse is not a
		// registration at all — the job never loads.
		var pl struct {
			Dict struct {
				Keys   []string `xml:"key"`
				Arrays []struct {
					Strings []string `xml:"string"`
				} `xml:"array"`
			} `xml:"dict"`
		}
		if err := xml.Unmarshal(body, &pl); err != nil {
			t.Fatalf("launchd cannot parse the plist this package wrote for %q: %v\n---\n%s",
				wantExe, err, body)
		}
		if len(pl.Dict.Arrays) == 0 || len(pl.Dict.Arrays[0].Strings) == 0 {
			t.Fatalf("plist has no ProgramArguments:\n%s", body)
		}
		if got := pl.Dict.Arrays[0].Strings[0]; got != wantExe {
			t.Fatalf("ProgramArguments[0] = %q, want %q", got, wantExe)
		}
	case "linux":
		// systemd: one ExecStart line, and its first token is the binary.
		var execs []string
		for _, line := range strings.Split(string(body), "\n") {
			if strings.HasPrefix(line, "ExecStart=") {
				execs = append(execs, strings.TrimPrefix(line, "ExecStart="))
			}
		}
		if len(execs) != 1 {
			t.Fatalf("unit has %d ExecStart lines, want exactly 1:\n%s", len(execs), body)
		}
		if got := strings.Fields(execs[0]); len(got) == 0 || got[0] != wantExe {
			t.Fatalf("ExecStart names %q, want the binary %q\n---\n%s", execs[0], wantExe, body)
		}
	}
}

// TestSec_Autostart_TempFileIsNotFollowedThroughASymlink
//
// writeIfDifferent re-implements the repo's atomic write, but with a fixed,
// fully predictable temp name (".bdrive-tmp-" + base) instead of
// store.WriteFileAtomic's random one. os.WriteFile follows a symlink at the
// destination, and os.Rename then renames the LINK, so a symlink planted at
// that predictable name turns "register autostart" into "truncate and
// overwrite an arbitrary file the user owns" — and leaves no registration
// behind either.
//
// Round 4 asserted exactly this property for store.WriteFileAtomic
// (TestSec_Store_AtomicWriteDoesNotFollowASymlinkAtTheDestination); this copy
// of the same idea never got the guard.
func TestSec_Autostart_TempFileIsNotFollowedThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "unit.conf")

	victim := filepath.Join(t.TempDir(), "precious")
	const secret = "do not clobber me\n"
	if err := os.WriteFile(victim, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	tmp := filepath.Join(dir, ".bdrive-tmp-"+filepath.Base(target))
	if err := os.Symlink(victim, tmp); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The write may fail (that is a fine outcome) but it must not reach
	// through the planted link.
	_, _ = writeIfDifferent(target, "[Service]\nExecStart=/usr/bin/bdrive resume\n")

	got, err := os.ReadFile(victim)
	if err != nil {
		t.Fatalf("victim file destroyed: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("writeIfDifferent wrote through the planted temp symlink:\nvictim now = %q\nwant %q",
			got, secret)
	}
}

// TestSec_Autostart_RegistrationIsNotWorldWritable
//
// The file names a binary that runs at every login. Whoever can write it owns
// the next login. Assert the mode is not group- or other-writable.
func TestSec_Autostart_RegistrationIsNotWorldWritable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "beardrive.service")
	if _, err := writeIfDifferent(path, "x\n"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		t.Fatalf("autostart registration mode = %v, must not be group/other writable", fi.Mode().Perm())
	}
}
