//go:build linux

package autostart

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 10, row 20's Linux half.
//
// autostart_linux.go is behind //go:build linux, so nine rounds of sweeps run
// on a darwin host never compiled unitArg, enable() or booted() — a reverted
// guard in any of them had nowhere to land, and round 5's "a path with a
// newline could inject a second ExecStart=" suspicion sat untestable for five
// rounds. These run on Linux: the repo's sandbox/ container, any Linux CI, or
// `GOOS=linux go test -c` run under docker.
//
// The pure-function tests need nothing but Linux. The two that exercise
// Install() need systemd to look booted (see booted()) and skip otherwise —
// in a container, `mkdir -p /run/systemd/system` is enough.
//
// All helpers are prefixed sec10.

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// sec10ExecStart pulls the one ExecStart= value out of a rendered unit, and
// insists there is exactly one and no sibling command directive. A second
// command directive is the whole injection story: systemd runs ExecStartPre=
// before ExecStart=, at every login, as the user.
func sec10ExecStart(t *testing.T, body string) string {
	t.Helper()
	var found []string
	for _, line := range strings.Split(body, "\n") {
		l := strings.ToLower(strings.TrimSpace(line)) // directive names are case-insensitive
		for _, d := range []string{"execstartpre=", "execstartpost=", "execstop=", "execstoppost=", "execreload=", "execcondition="} {
			if strings.HasPrefix(l, d) {
				t.Errorf("unit carries an extra command directive %q:\n%s", strings.TrimSpace(line), body)
			}
		}
		if strings.HasPrefix(l, "execstart=") {
			found = append(found, strings.TrimSpace(line)[len("ExecStart="):])
		}
	}
	if len(found) != 1 {
		t.Fatalf("unit has %d ExecStart= lines, want exactly 1:\n%s", len(found), body)
	}
	return found[0]
}

// sec10Specifiers is the subset of systemd.unit(5)'s specifier table these
// tests need. systemd resolves specifiers when it LOADS the unit, before the
// value is parsed as a command line — so an unescaped '%' in a path is not a
// literal '%', it is an instruction. The documented literal is '%%'.
var sec10Specifiers = map[byte]string{
	'h': "/home/victim",   // user home directory
	't': "/run/user/1000", // user runtime directory — writable by the session
	'u': "victim",         // user name
}

// sec10ExpandSpecifiers is systemd's specifier pass. It also reports whether
// every '%' it met was one systemd knows: an unknown specifier is a unit
// systemd refuses to load, which is its own silent failure (Install says
// "registered", nothing starts at login).
func sec10ExpandSpecifiers(s string) (out string, allKnown bool) {
	allKnown = true
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' || i+1 >= len(s) {
			b.WriteByte(s[i])
			continue
		}
		c := s[i+1]
		i++
		switch {
		case c == '%':
			b.WriteByte('%')
		default:
			if v, ok := sec10Specifiers[c]; ok {
				b.WriteString(v)
			} else {
				allKnown = false
				b.WriteByte('%')
				b.WriteByte(c)
			}
		}
	}
	return b.String(), allKnown
}

// sec10FirstWord is systemd's command-line split: whitespace separates
// arguments, double quotes group, a backslash escapes the next character. Only
// the first word — the binary systemd will actually exec — matters here.
func sec10FirstWord(s string) string {
	var b strings.Builder
	inQuote := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\\' && i+1 < len(s):
			i++
			b.WriteByte(s[i])
		case c == '"':
			inQuote = !inQuote
		case (c == ' ' || c == '\t') && !inQuote:
			return b.String()
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

// ---------------------------------------------------------------------------
// the pure-function properties — these are the regression tests
// ---------------------------------------------------------------------------

// TestSec_Autostart_UnitArgRendersExactlyTheBinaryThatWasInstalled
//
// unitArg's own doc says it "renders a path as one systemd command argument".
// It quotes for whitespace, double quotes and backslashes — and stops there.
// systemd's other metacharacter is '%': every specifier in systemd.unit(5) is
// expanded in ExecStart= before the line is parsed as a command, and a literal
// percent must be written '%%'.
//
// So a bdrive whose resolved path contains a directory named "%t" registers a
// login command that execs /run/user/1000/bdrive — a directory the user's own
// session can write to — rather than the binary that was installed. That turns
// "can write into my own runtime dir" into "runs at every login", and the
// login registration is meant to be the only thing this package creates.
//
// Asserted: whatever the path, systemd's own parse of the generated ExecStart
// (specifiers expanded, then quotes removed) yields back exactly the path that
// was installed.
func TestSec_Autostart_UnitArgRendersExactlyTheBinaryThatWasInstalled(t *testing.T) {
	for _, exe := range []string{
		"/usr/local/bin/bdrive",  // ordinary
		"/opt/My Tools/bdrive",   // a space: two arguments unless quoted
		`/opt/we"ird/bdrive`,     // a double quote
		`/opt/back\slash/bdrive`, // a backslash
		"/opt/50%/bdrive",        // a bare percent
		"/opt/%t/bdrive",         // %t = the user-writable runtime dir
		"/home/victim/%h/bdrive", // %h = home
		"/opt/100%%/bdrive",      // a path that already looks escaped
		"/opt/a b%tc/bdrive",     // both problems at once
	} {
		t.Run(exe, func(t *testing.T) {
			arg := sec10ExecStart(t, unit(exe))
			expanded, allKnown := sec10ExpandSpecifiers(arg)
			if !allKnown {
				t.Errorf("ExecStart=%s carries an unknown %%-specifier: systemd refuses to load the "+
					"unit, so Install reports success and nothing starts at login", arg)
			}
			if got := sec10FirstWord(expanded); got != exe {
				t.Errorf("installed %q, but systemd will exec %q\n  ExecStart=%s\n"+
					"  unitArg quotes for space/quote/backslash and never escapes '%%' as '%%%%'",
					exe, got, arg)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the same question end to end: Install() from a hostile directory
// ---------------------------------------------------------------------------

// sec10InstallFrom is secdmnInstallFrom (round 5, sec_autostart_test.go) with
// one difference: a REFUSAL is a result here, not a fatal. loginPath is
// supposed to refuse some of these paths, so the helper that measures it must
// be able to say "it refused" instead of failing the run.
//
// It copies this test binary into a directory named dir and runs it; the
// package's existing child hook (secdmnChildEnv, set by round 5's init()) makes
// that copy call Install() and print the registration path. os.Executable() —
// and so selfPath() — then reports the hostile path, which is the only honest
// way to ask what Install writes for a bdrive that lives there.
func sec10InstallFrom(t *testing.T, dir string) (exe, unitBody, refusal string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(self)
	if err != nil {
		t.Skipf("cannot read the test binary to re-exec it: %v", err)
	}
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
		t.Fatalf("mkdir %q: %v", dir, err)
	}
	exe = filepath.Join(home, dir, "bdrive")
	if err := os.WriteFile(exe, src, 0o755); err != nil {
		t.Fatal(err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved // selfPath resolves symlinks; compare like for like
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), secdmnChildEnv+"=1",
		"HOME="+home, "XDG_CONFIG_HOME="+filepath.Join(home, ".config"))
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return exe, "", strings.TrimSpace(string(ee.Stderr))
		}
		t.Fatalf("re-exec: %v (%s)", err, out)
	}
	body, err := os.ReadFile(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatalf("child reported a registration it did not write: %v", err)
	}
	return exe, string(body), ""
}

// TestSec_Autostart_InstallRegistersOnlyTheBinaryItInstalled
//
// The end-to-end form of the two attacks on this file's login registration,
// run by actually being a bdrive at each hostile path:
//
//   - a directory named "%t": systemd expands the specifier, so the registered
//     command is not the binary that ran Install;
//   - a directory name containing a newline: the path would end the ExecStart=
//     line and begin a directive of the attacker's choosing. loginPath is the
//     guard — Install must refuse, and write nothing.
//
// Round 5 raised the newline half as an untestable-from-macOS suspicion and it
// sat for five rounds. This is that test.
func TestSec_Autostart_InstallRegistersOnlyTheBinaryItInstalled(t *testing.T) {
	if !booted() {
		t.Skip("no /run/systemd/system: Install declines here by design (see TestInstallNeedsSystemd)")
	}

	t.Run("systemd specifier in the path", func(t *testing.T) {
		exe, body, errText := sec10InstallFrom(t, "%t")
		if errText != "" {
			return // refusing outright is also secure
		}
		arg := sec10ExecStart(t, body)
		expanded, _ := sec10ExpandSpecifiers(arg)
		if got := sec10FirstWord(expanded); got != exe {
			t.Errorf("bdrive at %q registered a login command systemd will resolve to %q\n  ExecStart=%s",
				exe, got, arg)
		}
	})

	t.Run("newline in the path", func(t *testing.T) {
		exe, body, errText := sec10InstallFrom(t, "hostile\nExecStartPre=!/bin/sh -c 'id > /tmp/sec10'\n#")
		if errText != "" {
			return // loginPath refused: correct
		}
		arg := sec10ExecStart(t, body) // fails inside on a second command directive
		if sec10FirstWord(arg) != exe {
			t.Errorf("bdrive at %q registered ExecStart=%s", exe, arg)
		}
	})
}

// ---------------------------------------------------------------------------
// enable()/Installed()/Uninstall() — the other never-compiled guards
// ---------------------------------------------------------------------------

// TestSec_Autostart_EnableSymlinkIsHonest
//
// Installed() answers `bdrive autostart`, and the package doc names the exact
// failure this package exists to prevent: "Install reported success and sync
// silently never resumed after a reboot". Installed() Lstats the wants entry,
// so ANY entry there — a dangling symlink, a symlink to something else, a
// regular file — reads as "registered".
//
// Asserted: "registered" means the wants entry actually resolves to the unit
// file this package wrote.
func TestSec_Autostart_EnableSymlinkIsHonest(t *testing.T) {
	if !booted() {
		t.Skip("no /run/systemd/system: Install declines here by design")
	}
	for _, tc := range []struct {
		name string
		set  func(t *testing.T, link string)
	}{
		{"dangling symlink", func(t *testing.T, link string) {
			os.Remove(link)
			if err := os.Symlink("/nonexistent/beardrive.service", link); err != nil {
				t.Fatal(err)
			}
		}},
		{"symlink to another unit", func(t *testing.T, link string) {
			other := filepath.Join(filepath.Dir(filepath.Dir(link)), "other.service")
			if err := os.WriteFile(other, []byte("[Service]\nExecStart=/bin/true\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			os.Remove(link)
			if err := os.Symlink(other, link); err != nil {
				t.Fatal(err)
			}
		}},
		{"regular file", func(t *testing.T, link string) {
			os.Remove(link)
			if err := os.WriteFile(link, []byte("[Unit]\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			if _, err := Install(); err != nil {
				t.Fatal(err)
			}
			path, _ := Path()
			link := filepath.Join(filepath.Dir(path), wantsDir, Unit)
			tc.set(t, link)
			if !Installed() {
				return // honest: it says not registered
			}
			resolved, err := filepath.EvalSymlinks(link)
			if err != nil || resolved != path {
				t.Errorf("Installed() reports registered, but %s/%s resolves to %q (err %v), not %s\n"+
					"  `bdrive autostart` prints \"registered\" for a machine where nothing starts at login",
					wantsDir, Unit, resolved, err, path)
			}
		})
	}
}

// TestSec_Autostart_UninstallLeavesNothingThatRunsAtLogin
//
// Uninstall's contract is "nothing will start at the next login". Whatever
// shape the enable entry is in when the user opts out — the ordinary relative
// symlink, an absolute one an older version wrote, or a stray file — it must
// be gone afterwards, and Installed() must agree.
func TestSec_Autostart_UninstallLeavesNothingThatRunsAtLogin(t *testing.T) {
	if !booted() {
		t.Skip("no /run/systemd/system: Install declines here by design")
	}
	for _, shape := range []string{"relative", "absolute", "regular file"} {
		t.Run(shape, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			t.Setenv("XDG_CONFIG_HOME", "")
			if _, err := Install(); err != nil {
				t.Fatal(err)
			}
			path, _ := Path()
			link := filepath.Join(filepath.Dir(path), wantsDir, Unit)
			switch shape {
			case "absolute":
				os.Remove(link)
				if err := os.Symlink(path, link); err != nil {
					t.Fatal(err)
				}
			case "regular file":
				os.Remove(link)
				if err := os.WriteFile(link, []byte("[Unit]\nDescription=x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := Uninstall(); err != nil {
				t.Fatalf("Uninstall: %v", err)
			}
			if _, err := os.Lstat(link); !os.IsNotExist(err) {
				t.Errorf("a %s enable entry survived Uninstall (%v): systemd still starts it at login", shape, err)
			}
			if _, err := os.Lstat(path); !os.IsNotExist(err) {
				t.Error("the unit file survived Uninstall")
			}
			if Installed() {
				t.Error("Installed() true after Uninstall")
			}
		})
	}
}
