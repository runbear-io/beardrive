package agenthooks

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func readJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("%s is not valid JSON: %v", path, err)
	}
	return m
}

func TestDetect(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	folder := t.TempDir()

	if got := Detect(folder); len(got) != 0 {
		t.Fatalf("nothing configured, detected %v", got)
	}
	// project-level dirs
	os.MkdirAll(filepath.Join(folder, ".codex"), 0o755)
	os.MkdirAll(filepath.Join(folder, ".gemini"), 0o755)
	// home-level dirs
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".hermes"), 0o755)
	got := Detect(folder)
	want := []string{"claude", "codex", "gemini", "hermes"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("detected %v, want %v", got, want)
	}
}

func TestInstallJSONPlatforms(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()

	results, err := Install(folder, []string{"claude", "codex", "gemini"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		if !r.Changed {
			t.Fatalf("%s: fresh install reported unchanged", r.Agent)
		}
	}

	// Claude: both events present, push is async, command carries the label.
	cl := readJSON(t, filepath.Join(folder, ".claude", "settings.json"))
	hooks := cl["hooks"].(map[string]any)
	for _, ev := range []string{"UserPromptSubmit", "PostToolUse"} {
		if _, ok := hooks[ev]; !ok {
			t.Fatalf("claude missing %s", ev)
		}
	}
	raw, _ := json.Marshal(cl)
	if !strings.Contains(string(raw), "claude-code session $s") {
		t.Fatal("claude hook lacks its session-note label")
	}
	if !strings.Contains(string(raw), `"async":true`) {
		t.Fatal("claude push hook should be async")
	}

	// Codex: same schema, its own label and matcher, no async field.
	cx, _ := json.Marshal(readJSON(t, filepath.Join(folder, ".codex", "hooks.json")))
	if !strings.Contains(string(cx), "codex session $s") || !strings.Contains(string(cx), "apply_patch") {
		t.Fatalf("codex hooks wrong: %s", cx)
	}
	if strings.Contains(string(cx), "async") {
		t.Fatal("codex should not get the claude-only async field")
	}

	// Gemini: its own event names and ms timeout.
	gm, _ := json.Marshal(readJSON(t, filepath.Join(folder, ".gemini", "settings.json")))
	for _, want := range []string{"BeforeAgent", "AfterTool", "gemini session $s", "30000"} {
		if !strings.Contains(string(gm), want) {
			t.Fatalf("gemini hooks missing %q: %s", want, gm)
		}
	}
}

func TestInstallIdempotentAndPreserving(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()

	// Pre-existing user hook must survive the merge.
	pre := `{"permissions":{"allow":["Bash(ls:*)"]},"hooks":{"PostToolUse":[{"matcher":"Bash","hooks":[{"type":"command","command":"echo mine"}]}]}}`
	os.MkdirAll(filepath.Join(folder, ".claude"), 0o755)
	os.WriteFile(filepath.Join(folder, ".claude", "settings.json"), []byte(pre), 0o644)

	if _, err := Install(folder, []string{"claude"}); err != nil {
		t.Fatal(err)
	}
	cfg := readJSON(t, filepath.Join(folder, ".claude", "settings.json"))
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), "echo mine") {
		t.Fatal("merge dropped the user's existing hook")
	}
	if _, ok := cfg["permissions"]; !ok {
		t.Fatal("merge dropped unrelated settings keys")
	}
	if got := len(cfg["hooks"].(map[string]any)["PostToolUse"].([]any)); got != 2 {
		t.Fatalf("PostToolUse groups = %d, want user's + ours", got)
	}

	// Second install: no change, byte-identical file.
	before, _ := os.ReadFile(filepath.Join(folder, ".claude", "settings.json"))
	results, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Changed {
		t.Fatal("re-install reported a change")
	}
	after, _ := os.ReadFile(filepath.Join(folder, ".claude", "settings.json"))
	if string(before) != string(after) {
		t.Fatal("re-install rewrote the file")
	}
}

func TestInstallHermesYAML(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	os.MkdirAll(filepath.Join(home, ".hermes"), 0o755)
	// Existing config keys must survive.
	os.WriteFile(filepath.Join(home, ".hermes", "config.yaml"),
		[]byte("model: hermes-4\nhooks_auto_accept: false\n"), 0o644)

	results, err := Install(t.TempDir(), []string{"hermes"})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Changed {
		t.Fatal("fresh hermes install reported unchanged")
	}
	data, _ := os.ReadFile(filepath.Join(home, ".hermes", "config.yaml"))
	var m map[string]any
	if err := yaml.Unmarshal(data, &m); err != nil {
		t.Fatalf("config.yaml no longer parses: %v", err)
	}
	if m["model"] != "hermes-4" {
		t.Fatal("merge dropped existing hermes config")
	}
	hooks := m["hooks"].(map[string]any)
	for _, ev := range []string{"pre_llm_call", "post_tool_call"} {
		if _, ok := hooks[ev]; !ok {
			t.Fatalf("hermes missing %s", ev)
		}
	}
	if !strings.Contains(string(data), "hermes session $s") {
		t.Fatal("hermes hook lacks its session-note label")
	}

	// Idempotent.
	results, _ = Install(t.TempDir(), []string{"hermes"})
	if results[0].Changed {
		t.Fatal("hermes re-install reported a change")
	}
}

func TestInstallAutoUsesDetection(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	os.MkdirAll(filepath.Join(folder, ".gemini"), 0o755)

	results, err := Install(folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Agent != "gemini" {
		t.Fatalf("auto install = %+v, want just gemini", results)
	}
}

func TestInstallUnknownAgent(t *testing.T) {
	if _, err := Install(t.TempDir(), []string{"cursor"}); err == nil {
		t.Fatal("unknown agent should error")
	}
}

// The generated hook command must extract a session id from hook stdin JSON
// and invoke bdrive with the platform label — run it for real against a fake
// bdrive to pin the shell behavior on every platform's payload shape.
func TestHookCommandExtraction(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".bdrive"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	fake := "#!/bin/sh\necho \"$@\" > \"" + dir + "/args.txt\"\n"
	os.WriteFile(filepath.Join(bin, "bdrive"), []byte(fake), 0o755)

	payloads := map[string]string{
		"claude-code": `{"session_id":"abc-123","hook_event_name":"UserPromptSubmit"}`,
		"codex":       `{"session_id":"th_042","turn_id":"t1","cwd":"/x"}`,
		"gemini":      `{"session_id":"g-9f","timestamp":"2026-07-11T00:00:00Z"}`,
		"hermes":      `{"hook_event_name":"pre_llm_call","tool_name":null,"session_id":"sess_abc123"}`,
	}
	for label, payload := range payloads {
		os.Remove(filepath.Join(dir, "args.txt"))
		cmdline := hookCommand(label)
		sh := "cd " + dir + " && PATH=" + bin + ":$PATH " + cmdline
		if err := runShell(t, sh, payload); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		got, err := os.ReadFile(filepath.Join(dir, "args.txt"))
		if err != nil {
			t.Fatalf("%s: hook never called bdrive: %v", label, err)
		}
		want := "sync . --note " + label + " session "
		if !strings.Contains(string(got), want) {
			t.Fatalf("%s: bdrive argv = %q, want it to contain %q", label, got, want)
		}
	}
}

func runShell(t *testing.T, script, stdin string) error {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Stdin = strings.NewReader(stdin)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, out)
	}
	return nil
}
