package agenthooks

// Security tests for the agent hook guard (scoreboard row 13).
//
// These hooks are the highest-blast-radius surface in the repo: `bdrive init`
// writes them into each platform's USER config, so they run on every session
// and every tool call on the machine, inside and outside BearDrive folders.
// CLAUDE.md states two invariants for them:
//
//   - the guard "stays pure shell" and "must never spawn `bdrive` (or anything
//     else) outside a BearDrive project"
//   - Install registers hooks in the user config — it must not write (or
//     un-write) anything else
//
// Every test here asserts the SECURE behavior, so each goes green the moment
// the hole is closed. Helpers are prefixed `sechook` per the harness rules.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ---- harness -------------------------------------------------------------

// sechookEnv is a throwaway machine: an isolated HOME whose mount registry
// names a mount that has nothing to do with any directory under test, and a
// fake `bdrive` on PATH that records the fact it was spawned.
type sechookEnv struct {
	root, home, bin, flag string
}

func sechookSetup(t *testing.T) sechookEnv {
	t.Helper()
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	root := t.TempDir()
	e := sechookEnv{
		root: root,
		home: filepath.Join(root, "home"),
		bin:  filepath.Join(root, "bin"),
		flag: filepath.Join(root, "spawned"),
	}
	if err := os.MkdirAll(filepath.Join(e.home, ".bdrive"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(e.bin, 0o755); err != nil {
		t.Fatal(err)
	}
	// A registry whose only mount lives on a path unrelated to `root`, so
	// nothing under root may legitimately match it.
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

// fires runs one generated hook command with dir as the session folder and
// reports whether the guard let it spawn `bdrive`.
func (e sechookEnv) fires(t *testing.T, dir, cmd string) bool {
	t.Helper()
	os.Remove(e.flag)
	script := "cd '" + dir + "' && HOME='" + e.home + "' " +
		"BDRIVE_HOME='" + filepath.Join(e.home, ".bdrive") + "' " +
		"PATH='" + e.bin + "':$PATH " + cmd
	c := exec.Command("/bin/sh", "-c", script)
	c.Stdin = strings.NewReader(`{"session_id":"s1"}`)
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("guard script failed: %v: %s", err, out)
	}
	_, err := os.Stat(e.flag)
	return err == nil
}

func sechookMkdir(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// ---- Install must not un-write its own user config -----------------------

// Install writes the hooks into the platform's USER config and then migrates
// away hooks that older versions wrote into PROJECTS. The migration walks up
// from the mount to the enclosing git root — but the user config is also a
// "project config" of whatever directory it sits in. When $HOME is itself a
// git repository (a dotfiles repo — extremely common), the enclosing git root
// of a mount under $HOME *is* $HOME, so the migration deletes the very hooks
// the same Install call just wrote, one line earlier.
//
// The user is told the install succeeded (Changed=true, no error), agents keep
// running with no sync hook, and every session from then on works on stale
// files with no signal that anything is wrong.
func TestSec_Hooks_InstallKeepsItsOwnUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	// $HOME is a git repo (dotfiles). Nothing about the mount is unusual.
	sechookMkdir(t, filepath.Join(home, ".git"))
	folder := sechookMkdir(t, filepath.Join(home, "work", "wiki"))
	t.Chdir(folder)
	// The platform is "detected" the way Detect() judges it.
	sechookMkdir(t, filepath.Join(home, ".claude"))

	res, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if len(res) != 1 {
		t.Fatalf("Install returned %d results, want 1", len(res))
	}
	if res[0].Migrated != "" {
		t.Errorf("Install reported migrating away %q — that is the USER config it just wrote,\n"+
			"not a legacy project config", res[0].Migrated)
	}
	if !Registered(folder, "claude") {
		data, _ := os.ReadFile(ConfigPath("", "claude"))
		t.Fatalf("Install reported success (%+v) but the hooks are gone from %s.\nconfig now:\n%s",
			res[0], ConfigPath("", "claude"), data)
	}
}

// The same self-erasure with no git repo involved: `bdrive init <folder>` run
// from the home directory puts $HOME in legacyHookDirs via os.Getwd().
func TestSec_Hooks_InstallFromHomeKeepsItsOwnUserConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := sechookMkdir(t, filepath.Join(home, "wiki"))
	sechookMkdir(t, filepath.Join(home, ".claude"))
	t.Chdir(home) // `cd ~ && bdrive init ./wiki`

	res, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !Registered(folder, "claude") {
		t.Fatalf("Install reported success (%+v) but wiped its own hooks from %s",
			res, ConfigPath("", "claude"))
	}
}

// ---- the guard must never spawn bdrive outside a mount -------------------

// The guard's second half asks the registry whether any mount lives *below*
// the session folder, with `grep -qF "\"$PWD/" mounts.json`. $PWD is
// interpolated into a fixed-string pattern — and grep -F treats every LINE of
// its pattern as a separate alternative. A directory name containing a
// newline therefore splits one pattern into several, and a blank line among
// them is the empty pattern, which matches every line of mounts.json.
//
// Result: in a folder that is not a mount, has no mount above it, and has no
// mount below it, the guard passes and spawns `bdrive` — the one thing
// CLAUDE.md says it must never do. Directory names are attacker-influenced
// (a synced folder, a cloned repo, an unpacked archive).
func TestSec_Hooks_GuardNeverSpawnsBdriveOutsideAMount(t *testing.T) {
	e := sechookSetup(t)

	plain := sechookMkdir(t, filepath.Join(e.root, "plain"))
	// A directory whose name contains a blank line.
	newline := sechookMkdir(t, filepath.Join(e.root, "a\n\nb"))
	// A directory whose name is a bare newline: the pattern splits into
	// `"<root>/` and `/`, and `/` matches every path in the registry.
	slash := sechookMkdir(t, filepath.Join(e.root, "\n"))

	for _, tc := range []struct {
		name string
		dir  string
	}{
		{"a plain unrelated folder", plain},
		{"a folder whose name contains a blank line", newline},
		{"a folder named with a bare newline", slash},
	} {
		for _, h := range []struct{ label, cmd string }{
			{"sync hook", hookCommand("claude-code")},
			{"read hook", readHookCommand()},
		} {
			if e.fires(t, tc.dir, h.cmd) {
				t.Errorf("%s: the %s spawned bdrive in a folder that is not a BearDrive project\n"+
					"  dir      = %q\n  registry = only /nowhere/that/exists/wiki",
					tc.name, h.label, tc.dir)
			}
		}
	}
}

// ---- shell metacharacters in a path must stay data ----------------------

// The concrete question from the brief: a teammate creates a project folder
// named `$(whoami)` (or with backticks, or a `;`). It syncs to my machine, an
// agent session starts in it, and the guard runs with that path as $PWD.
// Nothing in it may execute.
func TestSec_Hooks_MountPathMetacharactersNeverExecute(t *testing.T) {
	e := sechookSetup(t)

	name := "wiki $(touch pwn-a) `touch pwn-b` ;touch pwn-c& |touch pwn-d"
	mount := sechookMkdir(t, filepath.Join(e.root, name))
	makeMount(t, mount)
	inside := sechookMkdir(t, filepath.Join(mount, "notes $(touch pwn-e)"))

	for _, dir := range []string{mount, inside} {
		if !e.fires(t, dir, hookCommand("claude-code")) {
			t.Errorf("guard failed to recognize the mount at %q — the test proves nothing "+
				"if the command never ran", dir)
		}
		for _, sentinel := range []string{"pwn-a", "pwn-b", "pwn-c", "pwn-d", "pwn-e"} {
			for _, where := range []string{e.root, mount, inside, e.home} {
				if _, err := os.Stat(filepath.Join(where, sentinel)); err == nil {
					t.Fatalf("shell injection: %q in a path executed, creating %s",
						name, filepath.Join(where, sentinel))
				}
			}
		}
	}
}

// A registry entry a teammate can influence (mounts.json holds paths, and a
// project name becomes a folder name) must likewise never execute when the
// guard greps it.
func TestSec_Hooks_RegistryContentsNeverExecute(t *testing.T) {
	e := sechookSetup(t)
	if err := os.WriteFile(filepath.Join(e.home, ".bdrive", "mounts.json"),
		[]byte(`{"m-1":{"path":"`+e.root+`/$(touch `+e.root+`/pwn-reg)/wiki"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := sechookMkdir(t, filepath.Join(e.root, "elsewhere"))
	e.fires(t, dir, hookCommand("claude-code"))
	if _, err := os.Stat(filepath.Join(e.root, "pwn-reg")); err == nil {
		t.Fatal("shell injection: mounts.json contents executed")
	}
}

// ---- the guard is the first thing every hook does -----------------------

// Nothing installed may reach `bdrive` (or any other process) before the
// guard has decided the folder is a BearDrive project, and the guard must
// stay inside its stated budget of at most one grep of mounts.json.
func TestSec_Hooks_EveryHookCommandIsGuarded(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := sechookMkdir(t, filepath.Join(home, "wiki"))

	for _, agent := range Agents {
		if _, err := Install(folder, []string{agent}); err != nil {
			t.Fatalf("%s: install: %v", agent, err)
		}
		data, err := os.ReadFile(ConfigPath("", agent))
		if err != nil {
			t.Fatalf("%s: %v", agent, err)
		}
		for _, cmd := range sechookCommands(string(data)) {
			// Substring positions only — the serialized form escapes ">" and
			// "&" differently per format, so match on the escape-free part.
			gate := strings.Index(cmd, "command -v bdrive")
			spawn := strings.Index(cmd, "bdrive sync")
			if spawn < 0 {
				spawn = strings.Index(cmd, "bdrive read-log")
			}
			if spawn >= 0 && (gate < 0 || gate > spawn) {
				t.Errorf("%s: hook invokes bdrive before the guard clears it:\n%s", agent, cmd)
			}
			if n := strings.Count(cmd, "grep "); n > 1 {
				t.Errorf("%s: guard runs %d greps, budget is at most one:\n%s", agent, n, cmd)
			}
		}
	}
}

// sechookCommands pulls every hook command line out of a serialized config
// (JSON or YAML) without needing a per-platform parser.
func sechookCommands(cfg string) []string {
	var out []string
	for _, line := range strings.Split(cfg, "\n") {
		if strings.Contains(line, "sh -c") {
			out = append(out, line)
		}
	}
	return out
}
