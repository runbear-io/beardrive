package main

// Round 8 — the target is round 7's fixes (382882b) in cmd/bdrive: the
// `store.UnderRoot(config.Home(), folder)` guard init grew at its door, and
// the origin binding on this device's token (tokenGoesTo + dropTokenOffOrigin).
//
// Every test asserts the SECURE behaviour. Helpers are prefixed secfx7; no
// existing file is touched. The hub fixture and the real-binary runner are
// reused from sec_init_test.go rather than rebuilt.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secfx7Home re-points a fixture env at a beardrive home of the test's
// choosing. $BDRIVE_HOME is documented, is what the repo's own harnesses set,
// and is what any CI job or multi-profile setup uses — and the guard's
// behaviour depends entirely on where it points.
func secfx7Home(e *secinitEnv, bhome string) {
	e.bhome = bhome
	e.env = append(secinitEnvWithout("HOME", "BDRIVE_HOME", "BDRIVE_TOKEN", "XDG_CONFIG_HOME"),
		"HOME="+e.home, "BDRIVE_HOME="+bhome, "XDG_CONFIG_HOME="+filepath.Join(e.home, ".config"))
}

// ---------------------------------------------------------------------------
// init's beardrive-home guard, in the direction it does not look.
// ---------------------------------------------------------------------------

// Round 7 closed a critical: `bdrive init $BDRIVE_HOME` mounted the directory
// holding this device's bearer token, and the first cycle pushed settings.json
// to the hub as project content — for every member and every teammate's disk.
// The fix asks one question:
//
//	if home, err := config.Home(); err == nil && store.UnderRoot(home, folder)
//
// which is "is the FOLDER inside the HOME". The damage does not need that. It
// needs the HOME inside the FOLDER — mount any ancestor of $BDRIVE_HOME and
// settings.json is an ordinary file some levels down, which is exactly the
// case the reserved-directory rule was already unable to see. UnderRoot(home,
// folder) answers ".." for a parent and the guard reads that as "fine".
//
// With the default home (~/.bdrive) the segment happens to be the reserved
// name, so `bdrive init ~` is contained by accident. With $BDRIVE_HOME set —
// documented in this repo's own CLAUDE.md, used by every harness here, and the
// normal shape of a CI or multi-profile install — the segment is whatever the
// operator called it, and nothing is reserved about it.
func TestSec_Init_RefusesToMountAnAncestorOfTheBdriveHome(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	// $BDRIVE_HOME lives inside the folder about to be mounted, under a name
	// with nothing special about it.
	work := t.TempDir()
	bhome := filepath.Join(work, "state")
	secfx7Home(e, bhome)

	// A real session, so the home holds a settings.json with a token in it.
	seed := filepath.Join(t.TempDir(), "seed")
	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}
	defer e.run(seed, "stop", seed)
	if out, err := e.run(seed, "init", "--name", "seed", "--server", e.url, "--yes"); err != nil {
		t.Fatalf("seed init: %v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(bhome, "settings.json"),
		[]byte(`{"server":"`+e.url+`","token":"SECRET-DEVICE-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, "readme.md"), []byte("ordinary project content\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	defer e.run(work, "stop", work)
	out, err := e.run(work, "init", "--name", "work", "--server", e.url, "--yes")
	if err != nil {
		t.Logf("refused: %s", out)
	}
	e.run(work, "sync")

	for key, body := range e.pushed() {
		if strings.Contains(string(body), "SECRET-DEVICE-TOKEN") {
			t.Fatalf("this device's hub bearer token was pushed to the hub as project object %s:\n%s\n"+
				"`bdrive init %s` was accepted with $BDRIVE_HOME at %s. The round-7 guard asks "+
				"store.UnderRoot(home, folder) — is the folder inside the home — which answers "+
				"\"..\" and therefore false for a folder that CONTAINS the home. The reserved-"+
				"directory rule cannot see it either: it only knows the name .bdrive, and "+
				"$BDRIVE_HOME is whatever the operator set. Every member of the project, and "+
				"every teammate's disk, now has this device's credential.",
				key, body, work, bhome)
		}
	}
}

// The same guard, disabled by the shape of the value rather than by direction.
//
// config.Home() returns $BDRIVE_HOME verbatim — no filepath.Abs — and
// store.UnderRoot resolves it with filepath.EvalSymlinks. Handed a relative
// value it resolves against the process's working directory, which for `bdrive
// init <folder>` is wherever the user's shell happens to be, and filepath.Rel
// of an absolute path against a relative root fails outright. UnderRoot's
// fail-closed posture ("anything that does not resolve is outside") is the
// wrong direction for this caller, whose question is "is this dangerous": a
// home it cannot resolve reads as "not the home", and the mount is allowed.
func TestSec_Init_ARelativeBdriveHomeStillRefusesToBeMounted(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	work := t.TempDir()
	abs := filepath.Join(work, "bstate")
	if err := os.MkdirAll(abs, 0o755); err != nil {
		t.Fatal(err)
	}
	secfx7Home(e, "bstate") // relative: resolved against the command's cwd

	// A populated beardrive home, exactly as `bdrive login` would leave it.
	if err := os.WriteFile(filepath.Join(abs, "settings.json"),
		[]byte(`{"server":"`+e.url+`","token":"RELATIVE-HOME-TOKEN"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(abs, "device.json"),
		[]byte(`{"id":"m-abc12345","name":"box","author":"box"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// Point init AT the home itself — the exact case round 7 closed — from the
	// directory the relative value is relative to.
	defer e.run(abs, "stop", abs)
	out, err := e.run(work, "init", "bstate", "--name", "home", "--server", e.url, "--yes")
	if err != nil {
		t.Logf("refused: %s", out)
	}
	e.run(abs, "sync")

	if _, serr := os.Stat(filepath.Join(abs, ".bdrive", "config.json")); serr == nil && err == nil {
		t.Errorf("init mounted the beardrive home itself because $BDRIVE_HOME is a relative "+
			"path:\n%s\n"+
			"config.Home() returns the environment value verbatim and store.UnderRoot "+
			"EvalSymlinks-es it; a relative root makes filepath.Rel(root, abs) fail, UnderRoot "+
			"returns false, and the guard reads that as \"not the beardrive home\". The guard is "+
			"disabled by the shape of the value, not by anything about the folder.", out)
	}
	for key, body := range e.pushed() {
		if strings.Contains(string(body), "RELATIVE-HOME-TOKEN") {
			t.Errorf("the device token was pushed to the hub as object %s:\n%s", key, body)
		}
	}
}
