//go:build windows

package autostart

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows/registry"
)

// NOTE: these have never been executed — they are written and compile-checked
// (GOOS=windows go test -c) from a non-Windows machine. They run for real the
// first time the suite runs on Windows, or the day CI grows a windows runner.
//
// Unlike the macOS and Linux tests, these cannot be isolated with a temp HOME:
// the registration lives in HKCU, so they touch the real Run key of whoever
// runs them. Each test therefore restores the previous value, and they refuse
// to clobber a value they did not write.

// saveAndRestore snapshots the Run value so a test run leaves the user's
// logon behaviour exactly as it found it, pass or fail.
func saveAndRestore(t *testing.T) {
	t.Helper()
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE|registry.SET_VALUE)
	if err != nil {
		t.Skipf("cannot open %s: %v", runKey, err)
	}
	prev, _, prevErr := key.GetStringValue(valueName)
	key.Close()
	t.Cleanup(func() {
		key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
		if err != nil {
			return
		}
		defer key.Close()
		if prevErr == nil {
			key.SetStringValue(valueName, prev)
		} else {
			key.DeleteValue(valueName)
		}
	})
}

func TestInstallWritesRunValue(t *testing.T) {
	saveAndRestore(t)
	if _, err := Uninstall(); err != nil {
		t.Fatal(err)
	}
	if Installed() {
		t.Fatal("Installed() true after Uninstall")
	}

	res, err := Install()
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !res.Changed {
		t.Error("first Install reported no change")
	}
	if !strings.HasPrefix(res.Path, `HKCU\`) {
		t.Errorf("Path = %q, want the HKCU location for display", res.Path)
	}
	if !Installed() {
		t.Fatal("Installed() false right after Install")
	}

	key, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer key.Close()
	got, _, err := key.GetStringValue(valueName)
	if err != nil {
		t.Fatal(err)
	}
	exe, _ := selfPath()
	if want := command(exe); got != want {
		t.Errorf("Run value = %q, want %q", got, want)
	}
	// Explorer parses this as a command line, so an unquoted Program Files
	// path would silently run the wrong thing.
	if !strings.HasPrefix(got, `"`) {
		t.Errorf("executable is not quoted: %q", got)
	}
	if !strings.Contains(got, "resume") {
		t.Errorf("Run value does not run resume: %q", got)
	}

	// Idempotent: init calls this on every run.
	if res, err = Install(); err != nil || res.Changed {
		t.Errorf("second Install = (%+v, %v), want no change", res, err)
	}
}

// A moved or upgraded bdrive.exe must be corrected, not left pointing at a
// path that no longer exists.
func TestInstallRewritesStalePath(t *testing.T) {
	saveAndRestore(t)
	key, _, err := registry.CreateKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := key.SetStringValue(valueName, `"C:\old\bdrive.exe" resume --quiet`); err != nil {
		t.Fatal(err)
	}
	key.Close()

	res, err := Install()
	if err != nil {
		t.Fatal(err)
	}
	if !res.Changed {
		t.Error("Install left a Run value pointing at the wrong binary")
	}
}

func TestUninstall(t *testing.T) {
	saveAndRestore(t)
	if _, err := Install(); err != nil {
		t.Fatal(err)
	}
	res, err := Uninstall()
	if err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !res.Changed {
		t.Error("Uninstall reported no change while a value existed")
	}
	if Installed() {
		t.Error("Run value survived Uninstall")
	}
	// Removing what is not there is success, not an error.
	if res, err = Uninstall(); err != nil || res.Changed {
		t.Errorf("second Uninstall = (%+v, %v), want no change and no error", res, err)
	}
}

// Paths with spaces are the common case on Windows (Program Files).
func TestCommandQuotesSpacedPath(t *testing.T) {
	got := command(`C:\Program Files\BearDrive\bdrive.exe`)
	if got != `"C:\Program Files\BearDrive\bdrive.exe" resume --quiet` {
		t.Errorf("command = %q", got)
	}
}
