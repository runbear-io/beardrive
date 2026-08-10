package main

// Round 13 — the twin of round 7's $BDRIVE_HOME guard, one directory over.
//
// Round 7 closed `bdrive init $BDRIVE_HOME`. The identical shape exists for an
// AGENT's config directory: the reserved-path rule matches
// ".claude/settings.json" on its directory segment, and a mount whose ROOT is
// ~/.claude leaves that file as bare "settings.json" — reserved by nothing.
// Same for .credentials.json and everything under projects/, which is the
// transcript of every session ever run on the machine.
//
// Every test asserts the SECURE behaviour. Helpers are prefixed secfx13; the
// hub fixture and the real-binary runner are reused from sec_init_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secfx13Claude builds a realistic ~/.claude: the hook config the reservation
// rule exists to block, the credential file beside it, and one saved session
// transcript. Each carries its own sentinel so a leak names itself.
func secfx13Claude(t *testing.T, parent string) string {
	t.Helper()
	dir := filepath.Join(parent, ".claude")
	if err := os.MkdirAll(filepath.Join(dir, "projects", "some-project"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, body string) {
		if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(p)), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("settings.json", `{"hooks":{"PreToolUse":[{"command":"SECFX13-HOOK-COMMAND"}]}}`)
	write(".credentials.json", `{"token":"SECFX13-CREDENTIAL"}`)
	write("projects/some-project/session.jsonl", `{"text":"SECFX13-TRANSCRIPT"}`)
	// The one thing in here that SHOULD be shareable, so the test can tell
	// "refused the directory" apart from "refused everything".
	if err := os.MkdirAll(filepath.Join(dir, "skills", "example"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "skills", "example", "SKILL.md"),
		[]byte("# example skill\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// `bdrive init ~/.claude` must refuse before it writes or calls anything.
//
// The assertion that matters is the last one: whatever init printed, no
// sentinel from the agent's config directory may reach the hub.
func TestSec_Init_RefusesAnAgentConfigDirectoryAsAMountRoot(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	claude := secfx13Claude(t, t.TempDir())

	defer e.run(claude, "stop", claude)
	out, err := e.run(claude, "init", "--name", "claude-config", "--server", e.url, "--yes")
	if err == nil {
		t.Errorf("init mounted an agent's configuration directory:\n%s\n"+
			"the reserved-path rule only covers segments BELOW a mount root, so at this root "+
			"settings.json, .credentials.json and every saved session transcript are ordinary "+
			"top-level files", out)
	} else if !strings.Contains(out, ".claude") {
		t.Errorf("the refusal does not name the directory, so a user cannot act on it:\n%s", out)
	}
	if _, serr := os.Stat(filepath.Join(claude, ".bdrive", "config.json")); serr == nil {
		t.Error("init wrote .bdrive/config.json before refusing; the guard must land before any file write")
	}
	if len(e.sentAuth()) != 0 {
		t.Error("init reached the hub before refusing; the guard must land before any network call")
	}

	e.run(claude, "sync")
	for key, body := range e.pushed() {
		for _, sentinel := range []string{"SECFX13-HOOK-COMMAND", "SECFX13-CREDENTIAL", "SECFX13-TRANSCRIPT"} {
			if strings.Contains(string(body), sentinel) {
				t.Fatalf("%s from the agent's config directory was pushed to the hub as object %s:\n%s",
					sentinel, key, body)
			}
		}
	}
}

// Every directory that keys a reserved hook config, under the spellings the
// filesystem folds onto it. A guard that only knows the exact string
// ".claude" is bypassed by the name the same directory also answers to.
func TestSec_Init_RefusesEveryAgentConfigDirectorySpelling(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	for _, name := range []string{".claude", ".codex", ".gemini", ".hermes", ".CLAUDE"} {
		dir := filepath.Join(t.TempDir(), name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "settings.json"),
			[]byte(`{"hooks":"SECFX13-`+name+`"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := e.run(dir, "init", "--name", "cfg"+name, "--server", e.url, "--yes")
		if err == nil {
			e.run(dir, "stop", dir)
			t.Errorf("init mounted %s: its hook config is a top-level file at this root\n%s", name, out)
		}
	}
}

// The positive case, which is the whole point of the feature this guard ships
// with: ~/.claude/skills is what every doc tells people to sync, and
// filepath.Base of it is "skills".
func TestSec_Init_StillMountsTheSkillsDirectory(t *testing.T) {
	e := secinitNewEnv(t, secinitHubOpts{})

	claude := secfx13Claude(t, t.TempDir())
	skills := filepath.Join(claude, "skills")

	defer e.run(skills, "stop", skills)
	out, err := e.run(skills, "init", "--name", "team-skills", "--server", e.url, "--yes")
	if err != nil {
		t.Fatalf("init refused %s, the directory the docs tell users to sync: %v\n%s", skills, err, out)
	}
	if _, serr := os.Stat(filepath.Join(skills, ".bdrive", "config.json")); serr != nil {
		t.Fatalf("init reported success but wrote no project config: %v", serr)
	}

	// And mounting it carries the skill without carrying anything from the
	// agent config directory above it.
	e.run(skills, "sync")
	var sawSkill bool
	for key, body := range e.pushed() {
		if strings.Contains(string(body), "# example skill") {
			sawSkill = true
		}
		for _, sentinel := range []string{"SECFX13-HOOK-COMMAND", "SECFX13-CREDENTIAL", "SECFX13-TRANSCRIPT"} {
			if strings.Contains(string(body), sentinel) {
				t.Fatalf("mounting %s pushed %s from its parent as object %s", skills, sentinel, key)
			}
		}
	}
	if !sawSkill {
		t.Error("the skill file never reached the hub — sharing what an agent reads is the product")
	}
}
