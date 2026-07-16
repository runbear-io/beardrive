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
	if !strings.Contains(string(raw), "bdrive sync . --hook claude-code") {
		t.Fatal("claude pull hook should use --hook mode (link-formula injection)")
	}
	if !strings.Contains(string(raw), `"async":true`) {
		t.Fatal("claude push hook should be async")
	}
	// …and the read hook, on its own matcher.
	if !strings.Contains(string(raw), "bdrive read-log") || !strings.Contains(string(raw), `"matcher":"Read|Grep|Bash"`) {
		t.Fatalf("claude read hook missing: %s", raw)
	}

	// Codex: same schema, its own label and matcher, no async field.
	cx, _ := json.Marshal(readJSON(t, filepath.Join(folder, ".codex", "hooks.json")))
	if !strings.Contains(string(cx), "codex session $s") || !strings.Contains(string(cx), "apply_patch") {
		t.Fatalf("codex hooks wrong: %s", cx)
	}
	if strings.Contains(string(cx), "async") {
		t.Fatal("codex should not get the claude-only async field")
	}
	if !strings.Contains(string(cx), "bdrive read-log") || !strings.Contains(string(cx), `"matcher":"read_file|shell"`) {
		t.Fatalf("codex read hook missing: %s", cx)
	}

	// Gemini: its own event names and ms timeout.
	gm, _ := json.Marshal(readJSON(t, filepath.Join(folder, ".gemini", "settings.json")))
	for _, want := range []string{"BeforeAgent", "AfterTool", "gemini session $s", "30000", "bdrive read-log", "read_file|read_many_files|search_file_content|run_shell_command"} {
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
	if got := len(cfg["hooks"].(map[string]any)["PostToolUse"].([]any)); got != 3 {
		t.Fatalf("PostToolUse groups = %d, want user's + our push + our read", got)
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

// A config from before the read hook existed (sync hooks only) gains just
// the read group on re-install — the sync hooks are not duplicated.
func TestInstallUpgradesSyncOnlyConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	old := `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]}],
		"PostToolUse":[{"matcher":"Write|Edit|MultiEdit","hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]}]}}`
	os.MkdirAll(filepath.Join(folder, ".claude"), 0o755)
	os.WriteFile(filepath.Join(folder, ".claude", "settings.json"), []byte(old), 0o644)

	results, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Changed {
		t.Fatal("upgrade install reported unchanged")
	}
	cfg := readJSON(t, filepath.Join(folder, ".claude", "settings.json"))
	hooks := cfg["hooks"].(map[string]any)
	if got := len(hooks["UserPromptSubmit"].([]any)); got != 1 {
		t.Fatalf("UserPromptSubmit groups = %d, want the old one only", got)
	}
	if got := len(hooks["PostToolUse"].([]any)); got != 2 {
		t.Fatalf("PostToolUse groups = %d, want old push + new read", got)
	}
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), "bdrive read-log") {
		t.Fatal("upgrade did not add the read hook")
	}
}

// A config registered when the read hook only matched "Read" gets its
// matcher upgraded in place on re-install — coverage improvements must
// reach existing projects without duplicating groups.
func TestInstallUpgradesReadMatcher(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	old := `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]}],
		"PostToolUse":[
			{"matcher":"Write|Edit|MultiEdit","hooks":[{"type":"command","command":"sh -c 'bdrive sync .'"}]},
			{"matcher":"Read","hooks":[{"type":"command","command":"sh -c 'bdrive read-log .'"}]}]}}`
	os.MkdirAll(filepath.Join(folder, ".claude"), 0o755)
	os.WriteFile(filepath.Join(folder, ".claude", "settings.json"), []byte(old), 0o644)

	results, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Changed {
		t.Fatal("matcher upgrade reported unchanged")
	}
	cfg := readJSON(t, filepath.Join(folder, ".claude", "settings.json"))
	hooks := cfg["hooks"].(map[string]any)
	if got := len(hooks["PostToolUse"].([]any)); got != 2 {
		t.Fatalf("PostToolUse groups = %d, want push + read (no duplicates)", got)
	}
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), `"matcher":"Read|Grep|Bash"`) {
		t.Fatalf("read matcher not upgraded: %s", raw)
	}
	if strings.Contains(string(raw), `"matcher":"Read"`+",") {
		t.Fatalf("old matcher left behind: %s", raw)
	}

	// And it settles: the next install is a no-op.
	results, _ = Install(folder, []string{"claude"})
	if results[0].Changed {
		t.Fatal("re-install after upgrade reported a change")
	}
}

// A config registered before the link-formula hook existed gets its pull
// command converged in place on re-install — no duplicate groups, and the
// next install settles to a no-op.
func TestInstallUpgradesPullCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	folder := t.TempDir()
	old := `{"hooks":{
		"UserPromptSubmit":[{"hooks":[{"type":"command","command":"sh -c 'bdrive sync .'","timeout":30}]}],
		"PostToolUse":[
			{"matcher":"Write|Edit|MultiEdit","hooks":[{"type":"command","command":"sh -c 'bdrive sync .'","timeout":30,"async":true}]},
			{"matcher":"Read|Grep|Bash","hooks":[{"type":"command","command":"sh -c 'bdrive read-log .'","timeout":30,"async":true}]}]}}`
	os.MkdirAll(filepath.Join(folder, ".claude"), 0o755)
	os.WriteFile(filepath.Join(folder, ".claude", "settings.json"), []byte(old), 0o644)

	results, err := Install(folder, []string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].Changed {
		t.Fatal("pull-command upgrade reported unchanged")
	}
	cfg := readJSON(t, filepath.Join(folder, ".claude", "settings.json"))
	hooks := cfg["hooks"].(map[string]any)
	if got := len(hooks["UserPromptSubmit"].([]any)); got != 1 {
		t.Fatalf("UserPromptSubmit groups = %d, want converged single group", got)
	}
	if got := len(hooks["PostToolUse"].([]any)); got != 2 {
		t.Fatalf("PostToolUse groups = %d, want push + read (no duplicates)", got)
	}
	raw, _ := json.Marshal(cfg)
	if !strings.Contains(string(raw), "bdrive sync . --hook claude-code") {
		t.Fatalf("pull command not upgraded: %s", raw)
	}

	// Settles: the next install is a no-op.
	results, _ = Install(folder, []string{"claude"})
	if results[0].Changed {
		t.Fatal("re-install after upgrade reported a change")
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
	if got := len(hooks["post_tool_call"].([]any)); got != 2 {
		t.Fatalf("hermes post_tool_call groups = %d, want push + read", got)
	}
	if !strings.Contains(string(data), "hermes session $s") {
		t.Fatal("hermes hook lacks its session-note label")
	}
	if !strings.Contains(string(data), "bdrive read-log") {
		t.Fatal("hermes read hook missing")
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

// The read hook must hand the event JSON through to `bdrive read-log`
// untouched — the binary does the parsing, the shell only guards.
func TestReadHookCommand(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".bdrive"), 0o755)
	bin := filepath.Join(dir, "bin")
	os.MkdirAll(bin, 0o755)
	fake := "#!/bin/sh\necho \"$@\" > \"" + dir + "/args.txt\"\ncat > \"" + dir + "/stdin.txt\"\n"
	os.WriteFile(filepath.Join(bin, "bdrive"), []byte(fake), 0o755)

	payload := `{"session_id":"abc","tool_name":"Read","tool_input":{"file_path":"/x/wiki/a.md"}}`
	sh := "cd " + dir + " && PATH=" + bin + ":$PATH " + readHookCommand()
	if err := runShell(t, sh, payload); err != nil {
		t.Fatal(err)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatalf("read hook never called bdrive: %v", err)
	}
	if !strings.Contains(string(args), "read-log .") {
		t.Fatalf("bdrive argv = %q, want read-log .", args)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt"))
	if string(stdin) != payload {
		t.Fatalf("event JSON not passed through: %q", stdin)
	}

	// Outside a bdrive project the hook exits before invoking anything.
	os.Remove(filepath.Join(dir, "args.txt"))
	os.RemoveAll(filepath.Join(dir, ".bdrive"))
	if err := runShell(t, sh, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "args.txt")); !os.IsNotExist(err) {
		t.Fatal("hook invoked bdrive outside a project")
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
