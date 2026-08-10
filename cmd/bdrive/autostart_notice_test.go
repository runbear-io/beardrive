package main

import (
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestInstallAutostartWarnsBeforeTheFirstMacOSWrite
//
// Writing ~/Library/LaunchAgents/*.plist is what makes macOS pop "Background
// Items Added". Nothing suppresses that notice, so the only thing keeping it
// from reading as malware is our own line arriving first — and it must arrive
// only when a write is actually about to happen, or it becomes noise every
// init prints and nobody reads.
func TestInstallAutostartWarnsBeforeTheFirstMacOSWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("the notice exists only on macOS")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)

	// Non-interactive: no prompt, just the heads-up. (The TTY branch needs a
	// terminal survey can drive, which a test does not have.)
	say := func() string {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		old := os.Stdout
		os.Stdout = w
		installAutostart(false)
		os.Stdout = old
		w.Close()
		out, _ := io.ReadAll(r)
		r.Close()
		return string(out)
	}

	first := say()
	if !strings.Contains(first, "Background Items Added") {
		t.Errorf("first install said nothing about the macOS notice:\n%s", first)
	}
	plist := filepath.Join(home, "Library", "LaunchAgents", "ai.beardrive.daemon.plist")
	if _, err := os.Stat(plist); err != nil {
		t.Fatalf("no agent written: %v", err)
	}

	// Already registered: no second write, so no notice to explain.
	if second := say(); strings.Contains(second, "Background Items Added") {
		t.Errorf("re-running warned again about a notice that will not fire:\n%s", second)
	}
}
