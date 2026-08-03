package agenthooks

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Round 9 — the coverage audit of scoreboard row 13.
//
// Row 13 reads "fixed + clean" off seven tests. Reverting each of this
// package's guards one at a time showed five of them can be removed with the
// whole suite still green: the guard fires in $BDRIVE_HOME itself, it fires in
// a folder that merely spells a prefix of a mount path, samePath's os.SameFile
// resolution can go back to a string compare, gitRootOf can walk above $HOME
// again, and the hook groups can stack up (or freeze) on every reinstall.
//
// Each test below is written so that ONLY the guard it names can produce the
// refusal: every one carries a control that fires (or converges) through the
// same fixture, so a failure is this package's decision and not the harness's.
//
// Helper prefix: secaud4.

// secaud4Env is a throwaway machine: an isolated HOME whose .bdrive directory
// holds a mount registry (and, like the real one, NO config.json), plus a fake
// `bdrive` on PATH that records the fact it ran.
type secaud4Env struct{ root, home, bin, flag string }

func secaud4Setup(t *testing.T) secaud4Env {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	e := secaud4Env{
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
	e.setMounts(t, `{"m-elsewhere":{"path":"/nowhere/that/exists/wiki"}}`)
	if err := os.WriteFile(filepath.Join(e.bin, "bdrive"),
		[]byte("#!/bin/sh\necho spawned >> \""+e.flag+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return e
}

// setMounts rewrites the registry the guard greps.
func (e secaud4Env) setMounts(t *testing.T, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(e.home, ".bdrive", "mounts.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// fires runs one generated hook command with dir as the session folder and
// reports whether the guard let `bdrive` run.
func (e secaud4Env) fires(t *testing.T, dir, cmd string) bool {
	t.Helper()
	os.Remove(e.flag)
	c := exec.Command("/bin/sh", "-c", cmd)
	c.Dir = dir
	c.Env = []string{
		"HOME=" + e.home,
		"BDRIVE_HOME=" + filepath.Join(e.home, ".bdrive"),
		"PATH=" + e.bin + ":" + os.Getenv("PATH"),
		"PWD=" + dir,
	}
	c.Stdin = strings.NewReader(`{"session_id":"s1"}`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("guard script failed: %v: %s", err, out)
	}
	_, err := os.Stat(e.flag)
	return err == nil
}

// secaud4Hooks is every inline command Install writes.
func secaud4Hooks() []struct{ label, cmd string } {
	return []struct{ label, cmd string }{
		{"sync hook (PostToolUse)", hookCommand("claude-code")},
		{"pull hook (UserPromptSubmit)", hookPullCommand("claude-code")},
		{"read hook (Read/Grep/Bash)", readHookCommand()},
	}
}

func secaud4Mkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// secaud4Mount makes dir look like a BearDrive project.
func secaud4Mount(t *testing.T, dir string) string {
	t.Helper()
	secaud4Mkdir(t, filepath.Join(dir, ".bdrive"))
	if err := os.WriteFile(filepath.Join(dir, ".bdrive", "config.json"),
		[]byte(`{"id":"m-1","volume":"wiki"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// TestSec_Hooks_TheBdriveHomeIsNeverItselfAMount
//
// The walk-up half of the guard matches on `$d/.bdrive/config.json`, not on the
// `.bdrive` DIRECTORY, and the package comment says why: "$BDRIVE_HOME
// (~/.bdrive, which has no config.json) [must not] read as a mount".
//
// Nothing asserted it. Matching the directory instead makes $HOME itself — and
// therefore EVERY folder under it, since the walk goes up — a mount for every
// session on the machine: `bdrive` is spawned on every UserPromptSubmit and
// every Read/Grep/Bash/Write tool call in any folder the user works in, which
// is the one thing CLAUDE.md's invariant list forbids outright ("it must never
// spawn bdrive (or anything else) outside a BearDrive project").
//
// The secure behaviour: a directory holding a .bdrive that is the bdrive HOME
// (mounts.json, no config.json) is not a project.
func TestSec_Hooks_TheBdriveHomeIsNeverItselfAMount(t *testing.T) {
	e := secaud4Setup(t)

	// Control: a real mount fires every hook through this same fixture, so a
	// non-firing hook below is the guard's decision and not a broken PATH.
	mount := secaud4Mount(t, secaud4Mkdir(t, filepath.Join(e.root, "realmount")))
	for _, h := range secaud4Hooks() {
		if !e.fires(t, mount, h.cmd) {
			t.Fatalf("control: the %s did not fire inside a real mount", h.label)
		}
	}

	for _, dir := range []string{
		e.home, // $HOME, which holds ~/.bdrive
		secaud4Mkdir(t, filepath.Join(e.home, "Documents", "taxes")), // any folder under it
		secaud4Mkdir(t, filepath.Join(e.home, ".bdrive", "volumes")), // inside $BDRIVE_HOME itself
	} {
		for _, h := range secaud4Hooks() {
			if e.fires(t, dir, h.cmd) {
				t.Errorf("the %s spawned bdrive in %s — a folder that is not a project; "+
					"$BDRIVE_HOME's .bdrive directory read as a mount", h.label, dir)
			}
		}
	}
}

// TestSec_Hooks_AFolderThatOnlyPREFIXESAMountPathIsNotAMount
//
// The registry half of the guard is `grep -qF "\"$PWD/" mounts.json`, and both
// anchors carry weight: the leading quote pins the match to the START of a
// registered path, and the trailing slash means "a mount lives strictly BELOW
// this folder". Drop them and a folder whose path is a SUBSTRING of any
// registered mount path answers "yes, a mount lives here" — /work/proj matches
// the registered /work/project, and $PWD=/ matches everything.
//
// The consequence is the invariant again: bdrive is spawned on every tool call
// in a folder that is not a BearDrive project.
//
// The secure behaviour: only a folder that IS a mount, or that has one below
// it, may fire.
func TestSec_Hooks_AFolderThatOnlyPREFIXESAMountPathIsNotAMount(t *testing.T) {
	e := secaud4Setup(t)
	mount := secaud4Mount(t, secaud4Mkdir(t, filepath.Join(e.root, "work", "project")))
	e.setMounts(t, `{"m-1":{"path":"`+mount+`"}}`)

	// Control: the parent of a registered mount fires — that is exactly what
	// the registry lookup is for (an editor opened at the repo root above the
	// synced subfolder), and it proves the grep path is live in this fixture.
	parent := filepath.Join(e.root, "work")
	for _, h := range secaud4Hooks() {
		if !e.fires(t, parent, h.cmd) {
			t.Fatalf("control: the %s did not fire in %s, the parent of a registered mount", h.label, parent)
		}
	}

	// A sibling folder whose path is a prefix of the mount's path, and the
	// root of the fixture's filesystem tree. Neither is a mount and neither
	// has one below it.
	for _, dir := range []string{
		secaud4Mkdir(t, filepath.Join(e.root, "work", "proj")),
		secaud4Mkdir(t, filepath.Join(e.root, "work", "projec")),
	} {
		for _, h := range secaud4Hooks() {
			if e.fires(t, dir, h.cmd) {
				t.Errorf("the %s spawned bdrive in %s, which is not a mount and has none below it — "+
					"its path merely prefixes the registered mount %s", h.label, dir, mount)
			}
		}
	}
}

// TestSec_Hooks_InstallNeverStripsAnAgentConfigAboveHome
//
// gitRootOf's walk stops at $HOME: "above it nothing is 'this project's repo'".
// Everything it returns lands in legacyHookDirs, and every directory in that
// list has its agent config REWRITTEN (our hook groups deleted) by
// removeProjectHooks. With the stop removed the walk continues into $HOME's
// ancestors, so a .git anywhere above the home directory — /Users under a
// checked-out dotfiles tree, a container image whose whole filesystem is a
// repo, a home on a versioned network share — hands back that ancestor and
// `bdrive init` silently edits an agent config that has nothing to do with
// this project or this user.
//
// The secure behaviour: installing for a folder only ever touches that
// folder's own tree and the user config — never a config above $HOME.
func TestSec_Hooks_InstallNeverStripsAnAgentConfigAboveHome(t *testing.T) {
	base := t.TempDir()
	home := secaud4Mkdir(t, filepath.Join(base, "home"))
	folder := secaud4Mkdir(t, filepath.Join(home, "proj"))
	secaud4Mkdir(t, filepath.Join(base, ".git")) // a repository ABOVE $HOME
	t.Setenv("HOME", home)

	// An agent config above $HOME, carrying hook groups with our markers.
	outside := filepath.Join(base, ".claude", "settings.json")
	secaud4Mkdir(t, filepath.Dir(outside))
	before := `{
  "hooks": {
    "UserPromptSubmit": [
      {"hooks": [{"type": "command", "command": "sh -c 'bdrive sync . --hook claude-code'"}]}
    ]
  }
}
`
	if err := os.WriteFile(outside, []byte(before), 0o644); err != nil {
		t.Fatal(err)
	}

	res, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	// Control: the install did happen (so a clean assertion below is not just
	// an install that never ran).
	if !Registered("", "claude") {
		t.Fatal("control: Install did not register the hooks in the user config")
	}

	after, err := os.ReadFile(outside)
	if err != nil {
		t.Fatalf("the config above $HOME is gone: %v", err)
	}
	if string(after) != before {
		t.Errorf("Install rewrote %s — an agent config ABOVE $HOME, reached because a .git\n"+
			"sits above the home directory\nbefore:\n%s\nafter:\n%s", outside, before, after)
	}
	for _, r := range res {
		if r.Migrated != "" {
			t.Errorf("Install reported migrating %q, outside this project's tree", r.Migrated)
		}
	}
}

// TestSec_Hooks_ReinstallingNeitherStacksNorFreezesTheHookGroups
//
// mergeJSONHooks finds its own group by marker and converges it in place. Both
// halves of that are load-bearing and neither was asserted:
//
//   - not finding it appends a second copy on every `bdrive init`, so the guard
//     — which runs on every tool call of every session on the machine — is paid
//     twice, then three times; the read hook double-counts every agent read in
//     the hub's heatmap; and the config grows without bound.
//   - not converging it freezes whatever command is already registered. Every
//     hardening of the inline guard (rounds 2, 3 and this one live in that
//     string) would then reach only machines that have never run init before.
//
// The secure behaviour: after any number of installs there is exactly one
// group per marker per event, and its command is the current one.
func TestSec_Hooks_ReinstallingNeitherStacksNorFreezesTheHookGroups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := secaud4Mkdir(t, filepath.Join(t.TempDir(), "proj"))

	for i := 0; i < 3; i++ {
		if _, err := Install(folder, []string{"claude"}); err != nil {
			t.Fatalf("install %d: %v", i, err)
		}
	}
	secaud4AssertOneGroupPerMarker(t, ConfigPath("", "claude"))

	// A registered group carrying an OLD command (what an earlier release
	// wrote): reinstalling must converge it, not leave it and not duplicate it.
	stale := "sh -c 'bdrive sync . --hook claude-code'" // no mount guard at all
	secaud4RewriteFirstCommand(t, ConfigPath("", "claude"), "UserPromptSubmit", stale)
	if _, err := Install(folder, []string{"claude"}); err != nil {
		t.Fatalf("reinstall: %v", err)
	}
	secaud4AssertOneGroupPerMarker(t, ConfigPath("", "claude"))
	if got := secaud4FirstCommand(t, ConfigPath("", "claude"), "UserPromptSubmit"); got == stale {
		t.Errorf("reinstalling left the stale, UNGUARDED hook command in place:\n%s\n"+
			"every fix to the inline guard stops at machines that ran init once", got)
	}
}

func secaud4Groups(t *testing.T, path, event string) []any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return root.Hooks[event]
}

func secaud4AssertOneGroupPerMarker(t *testing.T, path string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root struct {
		Hooks map[string][]any `json:"hooks"`
	}
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	if len(root.Hooks) == 0 {
		t.Fatalf("control: %s carries no hooks at all", path)
	}
	for event, arr := range root.Hooks {
		var sync, read int
		for _, g := range arr {
			if containsMarker(g, readMarker) {
				read++
			} else if containsMarker(g, marker) {
				sync++
			}
		}
		if sync > 1 || read > 1 {
			t.Errorf("%s.%s carries %d bdrive sync group(s) and %d read-log group(s); "+
				"each must appear exactly once no matter how often init runs", path, event, sync, read)
		}
	}
}

func secaud4FirstCommand(t *testing.T, path, event string) string {
	t.Helper()
	for _, g := range secaud4Groups(t, path, event) {
		m, ok := g.(map[string]any)
		if !ok {
			continue
		}
		hooks, _ := m["hooks"].([]any)
		for _, h := range hooks {
			hm, ok := h.(map[string]any)
			if !ok {
				continue
			}
			if cmd, ok := hm["command"].(string); ok {
				return cmd
			}
		}
	}
	t.Fatalf("no command registered under %s in %s", event, path)
	return ""
}

func secaud4RewriteFirstCommand(t *testing.T, path, event, cmd string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	if len(arr) == 0 {
		t.Fatalf("control: nothing registered under %s", event)
	}
	g, _ := arr[0].(map[string]any)
	inner, _ := g["hooks"].([]any)
	if len(inner) == 0 {
		t.Fatalf("control: %s group has no hooks", event)
	}
	h, _ := inner[0].(map[string]any)
	h["command"] = cmd
	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}
