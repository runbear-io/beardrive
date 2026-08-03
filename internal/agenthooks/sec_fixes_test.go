package agenthooks

// Round 3 — attacking round 2's own hook-guard fix.
//
// Round 2 closed one shape: a NEWLINE in $PWD split the `grep -F` pattern, and
// a blank alternative matched every mount, so the guard spawned `bdrive` in a
// folder that is not a BearDrive project. The fix is a single `case "$PWD" in
// *"<LF>"*) exit 0;; esac`.
//
// This file asks what else can reach that grep: the other control characters,
// a $PWD the shell never set, and the hook variants round 2's test never ran.
// It also re-asserts the invariant the fix must not have cost — the guard is
// still pure shell and still spawns nothing before it has decided.
//
// Helpers are prefixed `secfix`; no existing file is touched.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// secfixEnv is a throwaway machine: an isolated HOME whose mount registry
// names a mount unrelated to anything under test, and a fake `bdrive` on PATH
// that records the fact it ran. (sechookSetup in sec_hooks_test.go builds the
// same shape; this one is independent so neither file can break the other.)
type secfixEnv struct{ root, home, bin, flag string }

func secfixSetup(t *testing.T) secfixEnv {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	e := secfixEnv{
		root: root,
		home: filepath.Join(root, "home"),
		bin:  filepath.Join(root, "bin"),
		flag: filepath.Join(root, "spawned"),
	}
	for _, d := range []string{filepath.Join(e.home, ".bdrive"), e.bin} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(e.home, ".bdrive", "mounts.json"),
		[]byte(`{"m-elsewhere":{"path":"/nowhere/that/exists/wiki"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.bin, "bdrive"),
		[]byte("#!/bin/sh\necho spawned >> \""+e.flag+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// fires runs one generated hook command with dir as the session folder, with
// optional extra environment, and reports whether the guard let bdrive run.
func (e secfixEnv) fires(t *testing.T, dir, cmd string, extraEnv ...string) bool {
	t.Helper()
	os.Remove(e.flag)
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = dir
	c.Env = append([]string{
		"HOME=" + e.home,
		"BDRIVE_HOME=" + filepath.Join(e.home, ".bdrive"),
		"PATH=" + e.bin + ":" + os.Getenv("PATH"),
		"PWD=" + dir,
	}, extraEnv...)
	c.Stdin = strings.NewReader(`{"session_id":"s1"}`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("guard script failed: %v: %s", err, out)
	}
	_, err := os.Stat(e.flag)
	return err == nil
}

func secfixMkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// secfixHooks is every inline command Install can write. Round 2's regression
// test covered hookCommand and readHookCommand; hookPullCommand — the
// UserPromptSubmit hook, the one that runs on every single turn — was not in
// it, and it is the only variant whose stdout is NOT discarded.
func secfixHooks() []struct{ label, cmd string } {
	return []struct{ label, cmd string }{
		{"sync hook (PostToolUse)", hookCommand("claude-code")},
		{"pull hook (UserPromptSubmit)", hookPullCommand("claude-code")},
		{"read hook (Read/Grep/Bash)", readHookCommand()},
	}
}

// ---- the grep pattern, revisited ----

// $PWD is interpolated raw into `grep -qF "\"$PWD/"`. Round 2 walled off the
// one character that splits a -F pattern into alternatives. Every other
// control character and whitespace form must also leave the guard closed: a
// folder that is not a mount must never spawn anything, whatever it is called.
func TestSec_Hooks_GuardStaysClosedForEveryControlCharacterInPWD(t *testing.T) {
	e := secfixSetup(t)

	names := map[string]string{
		"plain":                   "plain",
		"carriage return":         "a\rb",
		"vertical tab":            "a\vb",
		"form feed":               "a\fb",
		"backspace":               "a\bb",
		"NUL-adjacent SOH":        "a\x01b",
		"escape":                  "a\x1bb",
		"DEL":                     "a\x7fb",
		"tab":                     "a\tb",
		"CR then LF":              "a\r\nb",
		"lone CR at the end":      "trailing\r",
		"spaces":                  "a b  c",
		"quote":                   `a"b`,
		"backslash":               `a\b`,
		"grep metacharacters":     `a.*[b]^$c`,
		"a JSON fragment":         `x","path":"/nowhere/that/exists/wiki`,
		"the registry's own path": "nowhere",
	}

	for label, name := range names {
		dir := secfixMkdir(t, filepath.Join(e.root, "cases", name))
		for _, h := range secfixHooks() {
			if e.fires(t, dir, h.cmd) {
				t.Errorf("%s: the %s spawned bdrive in a folder that is not a BearDrive project\n"+
					"  dir      = %q\n  registry = only /nowhere/that/exists/wiki",
					label, h.label, dir)
			}
		}
	}
}

// A hook inherits whatever environment the agent hands it. If $PWD arrives
// unset, empty, or stale, the guard's own `cd` is what has to make it true
// again — because everything after that line trusts $PWD as the literal the
// grep pattern is built from. An empty $PWD makes that pattern `"/`, which is
// a substring of every absolute path in mounts.json.
func TestSec_Hooks_GuardDoesNotTrustAnInheritedPWD(t *testing.T) {
	e := secfixSetup(t)
	plain := secfixMkdir(t, filepath.Join(e.root, "plain"))

	for _, tc := range []struct{ label, pwd string }{
		{"empty PWD", ""},
		{"stale PWD naming a mount's parent", "/nowhere/that/exists"},
		{"stale PWD naming the root", "/"},
		{"stale PWD naming a nonexistent path", "/no/such/place"},
	} {
		for _, h := range secfixHooks() {
			// PWD is passed last so it wins over fires()'s own entry.
			if e.fires(t, plain, h.cmd, "PWD="+tc.pwd) {
				t.Errorf("%s: the %s spawned bdrive in %q, which is not a BearDrive project",
					tc.label, h.label, plain)
			}
		}
	}
}

// CLAUDE.md: "the guard is pure shell (a couple of stats, at most one grep of
// mounts.json) and never spawns the binary outside a mount". Round 2 added a
// line to it; assert the shape did not drift. Nothing before the mount
// decision may substitute a command or reach for a helper binary.
func TestSec_Hooks_GuardIsStillPureShell(t *testing.T) {
	g := mountGuard()
	for _, banned := range []string{"$(", "`", "jq", "awk", "sed", "python", "node", "find ", "xargs", "eval"} {
		if strings.Contains(g, banned) {
			t.Errorf("the mount guard contains %q — it runs on every tool call on the machine, "+
				"inside and outside BearDrive folders:\n  %s", banned, g)
		}
	}
	// The binary itself is only ever *looked up* in the guard (`command -v`),
	// never run: running it is what the guard exists to prevent.
	if strings.Contains(g, "bdrive ") && !strings.Contains(g, "command -v bdrive ") {
		t.Errorf("the mount guard invokes bdrive before it has decided:\n  %s", g)
	}
	// Every hook command must open with the guard, unmodified: a variant that
	// merely resembles it is a variant that can be fixed in one place and
	// stay broken in another.
	for _, h := range secfixHooks() {
		if !strings.HasPrefix(h.cmd, `sh -c '`+g) {
			t.Errorf("the %s does not open with the shared mount guard:\n  %s", h.label, h.cmd)
		}
	}
}

// The control that makes the tests above mean something: inside a real mount,
// every hook variant does fire. Without this, a guard that exited 0
// unconditionally would score a perfect round.
func TestSec_Hooks_GuardStillFiresInsideARealMount(t *testing.T) {
	e := secfixSetup(t)
	mount := secfixMkdir(t, filepath.Join(e.root, "wiki"))
	if err := os.MkdirAll(filepath.Join(mount, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mount, ".bdrive", "config.json"), []byte(`{"mount_id":"m1"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	inside := secfixMkdir(t, filepath.Join(mount, "docs", "deep"))
	for _, dir := range []string{mount, inside} {
		for _, h := range secfixHooks() {
			if !e.fires(t, dir, h.cmd) {
				t.Errorf("the %s did NOT run inside a real mount (%s) — the guard is broken shut", h.label, dir)
			}
		}
	}
}
