package agenthooks

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// Round 10 — Uninstall, never swept in nine rounds. It is a write path over
// the same machine-wide user configs Install guards carefully, and its whole
// contract is a negative one: "Removes only beardrive's own hook entries from
// each platform's user config, leaving any other hooks in those files
// untouched" (cmd/bdrive/hooks.go, and agenthooks.go:259).
//
// Plus the assignment's second half: installHermes carries a byte-for-byte
// copy of mergeJSONHooks' converge/idempotency block, and round 9 asserted it
// only on the JSON side.

// sec10Home isolates $HOME so ConfigPath resolves under a temp dir.
func sec10Home(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	return home
}

// sec10WriteJSON writes a config file, creating its directory.
func sec10WriteJSON(t *testing.T, path string, v any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// sec10ReadJSON loads a config file back.
func sec10ReadJSON(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return out
}

// sec10Commands lists every "command" string under one event of a hooks map.
func sec10Commands(root map[string]any, event string) []string {
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	var out []string
	for _, g := range arr {
		grp, _ := g.(map[string]any)
		inner, _ := grp["hooks"].([]any)
		for _, h := range inner {
			if m, ok := h.(map[string]any); ok {
				if c, ok := m["command"].(string); ok {
					out = append(out, c)
				}
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// 1. Uninstall deletes hook groups it never wrote.
// ---------------------------------------------------------------------------

// TestSec_HooksUninstall_KeepsHooksBearDriveNeverWrote
//
// removeHooks decides what is "ours" with containsMarker — a substring hunt
// for "bdrive sync" / "bdrive read-log" over the JSON of a whole hook GROUP.
// Anything a user wrote that merely mentions the command is deleted from their
// machine-wide agent config, in an event beardrive never registers anything
// under, by a command whose help text promises the opposite.
//
// The delta that makes this the code's decision and not a fixture artifact:
// the same file also holds a group with no such mention, and that one survives.
// Only the string in the command decides.
func TestSec_HooksUninstall_KeepsHooksBearDriveNeverWrote(t *testing.T) {
	sec10Home(t)
	path := ConfigPath("", "claude")

	// A user's own config. Neither group was written by beardrive: SessionStart
	// is an event agenthooks never touches, and both groups are hand-authored.
	sec10WriteJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"SessionStart": []any{
				map[string]any{"hooks": []any{
					// The user's own pull, from before `bdrive hooks` existed.
					map[string]any{"type": "command", "command": "cd ~/wiki && bdrive sync . || true"},
				}},
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": "echo hello"},
				}},
			},
		},
	})

	if _, err := Uninstall([]string{"claude"}); err != nil {
		t.Fatal(err)
	}

	got := sec10Commands(sec10ReadJSON(t, path), "SessionStart")
	// The control: the unrelated hook is still there, so nothing about the
	// fixture or the call is broken.
	if len(got) == 0 || got[len(got)-1] != "echo hello" {
		t.Fatalf("fixture broken: the unrelated hook did not survive either, got %q", got)
	}
	if len(got) != 2 {
		t.Errorf("Uninstall deleted a hook the user wrote: SessionStart commands = %q, "+
			"want both kept — beardrive registered nothing under SessionStart, and "+
			"`bdrive hooks uninstall` documents that it leaves other hooks untouched", got)
	}
}

// TestSec_HooksUninstall_DoesNotSwallowASiblingHookInTheSameGroup
//
// containsMarker is applied to the GROUP, not the hook. A group holding
// beardrive's command next to somebody else's — which is what a user gets the
// moment they tidy their settings.json by hand, or any tool that merges by
// matcher does — loses both. The user's hook is collateral for a removal it
// was never part of.
func TestSec_HooksUninstall_DoesNotSwallowASiblingHookInTheSameGroup(t *testing.T) {
	sec10Home(t)
	path := ConfigPath("", "claude")

	sec10WriteJSON(t, path, map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{"matcher": "Write|Edit", "hooks": []any{
					map[string]any{"type": "command", "command": hookCommand("claude-code")},
					map[string]any{"type": "command", "command": "run-my-formatter"},
				}},
			},
		},
	})

	if _, err := Uninstall([]string{"claude"}); err != nil {
		t.Fatal(err)
	}

	got := sec10Commands(sec10ReadJSON(t, path), "PostToolUse")
	for _, c := range got {
		if c == hookCommand("claude-code") {
			t.Fatalf("fixture broken: beardrive's own hook survived uninstall")
		}
	}
	found := false
	for _, c := range got {
		if c == "run-my-formatter" {
			found = true
		}
	}
	if !found {
		t.Errorf("Uninstall removed the user's `run-my-formatter` hook along with beardrive's, "+
			"because containsMarker judges the whole group: PostToolUse commands = %q", got)
	}
}

// ---------------------------------------------------------------------------
// 2. Uninstall through a symlinked config.
// ---------------------------------------------------------------------------

// TestSec_HooksUninstall_RemovesHooksFromASymlinkedConfig
//
// ~/.claude/settings.json as a symlink into a dotfiles repo is the normal
// shape for anyone who versions their machine config. removeHooks READS
// through the link (os.ReadFile follows) and WRITES with store.WriteFileAtomic,
// whose final os.Rename replaces the link with a regular file.
//
// Two consequences, both asserted here: the user's dotfiles link is destroyed
// without a word, and — the part that matters — the hooks are still in the file
// they actually live in, so every other machine sharing that repo, and this one
// the next time the link is restored, keeps running a command the user just
// told beardrive to remove. Uninstall reports Changed: true either way.
func TestSec_HooksUninstall_RemovesHooksFromASymlinkedConfig(t *testing.T) {
	home := sec10Home(t)
	real := filepath.Join(t.TempDir(), "dotfiles-settings.json")
	sec10WriteJSON(t, real, map[string]any{
		"hooks": map[string]any{
			"PostToolUse": []any{
				map[string]any{"matcher": "Write|Edit", "hooks": []any{
					map[string]any{"type": "command", "command": hookCommand("claude-code")},
				}},
			},
		},
	})
	link := ConfigPath("", "claude")
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	res, err := Uninstall([]string{"claude"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].Changed {
		t.Fatalf("fixture: Uninstall reported %+v, expected a change", res)
	}

	// Where the hooks actually live.
	if cmds := sec10Commands(sec10ReadJSON(t, real), "PostToolUse"); len(cmds) != 0 {
		t.Errorf("Uninstall reported the hooks removed but %s still carries %q — "+
			"it wrote a fresh regular file over the symlink instead of through it, so the "+
			"config that is actually deployed still runs beardrive on every tool call",
			real, cmds)
	}
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("Uninstall replaced the user's %s symlink with a regular file", link)
	}
}

// ---------------------------------------------------------------------------
// 3. Uninstall leaves the legacy project-level registration running.
// ---------------------------------------------------------------------------

// TestSec_HooksUninstall_AlsoRemovesLegacyProjectHooks
//
// Install goes out of its way to strip hooks earlier versions wrote into a
// project (removeProjectHooks — "leaving those behind would run every hook
// twice"). Uninstall only ever touches ConfigPath, the user-level file.
//
// A user who upgrades and then asks for the hooks to be REMOVED — without
// re-running install first, which is the natural order — is told "removed" and
// still has a `bdrive sync` firing on every turn from the project config. The
// command's documented job is "Remove beardrive's sync hooks from every agent
// platform"; a residual it knows how to find and does not is a live execution
// surface the user believes is gone.
//
// The legacy config is planted in the WORKING DIRECTORY, which is one of the
// three places legacyHookDirs already searches — so this is reachable by the
// discovery Install itself uses, not a location no fix could find.
func TestSec_HooksUninstall_AlsoRemovesLegacyProjectHooks(t *testing.T) {
	sec10Home(t)
	project := t.TempDir()
	t.Chdir(project)

	// The user-level registration a current version wrote.
	sec10WriteJSON(t, ConfigPath("", "claude"), map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": hookPullCommand("claude-code")},
				}},
			},
		},
	})
	// The project-level one an older version wrote, which Install migrates away
	// and Uninstall never looks at.
	legacy := projectConfigPath(project, "claude")
	sec10WriteJSON(t, legacy, map[string]any{
		"hooks": map[string]any{
			"UserPromptSubmit": []any{
				map[string]any{"hooks": []any{
					map[string]any{"type": "command", "command": hookCommand("claude-code")},
				}},
			},
		},
	})

	if _, err := Uninstall([]string{"claude"}); err != nil {
		t.Fatal(err)
	}

	// Control: the user-level one really is gone, so the call worked.
	if cmds := sec10Commands(sec10ReadJSON(t, ConfigPath("", "claude")), "UserPromptSubmit"); len(cmds) != 0 {
		t.Fatalf("fixture: the user-level hook survived, got %q", cmds)
	}
	if cmds := sec10Commands(sec10ReadJSON(t, legacy), "UserPromptSubmit"); len(cmds) != 0 {
		t.Errorf("`hooks uninstall` left the legacy project registration at %s running %q — "+
			"Install knows how to find and strip exactly this file (removeProjectHooks); "+
			"Uninstall never looks, so the user is told the hooks are gone while one still fires",
			legacy, cmds)
	}
}

// ---------------------------------------------------------------------------
// 4. Uninstall's refusals — asserted, not assumed.
// ---------------------------------------------------------------------------

// TestSec_HooksUninstall_RefusesAnUnknownAgentAndWritesNothing
//
// The agent list comes straight off `--agent` on the command line. An unknown
// name must be refused before anything is written, and must never resolve to a
// path (ConfigPath returns "" for an unknown agent, which would be a write to
// the process's working directory).
func TestSec_HooksUninstall_RefusesAnUnknownAgentAndWritesNothing(t *testing.T) {
	home := sec10Home(t)
	before, _ := os.ReadDir(home)

	for _, name := range []string{"../../etc", "", "CLAUDE", "claude/../codex", "hermes\x00"} {
		if _, err := Uninstall([]string{name}); err == nil {
			t.Errorf("Uninstall(%q) was accepted; only the four known platforms may name a config path", name)
		}
	}
	if after, _ := os.ReadDir(home); len(after) != len(before) {
		t.Errorf("a refused Uninstall created %d entries under $HOME", len(after)-len(before))
	}
}

// TestSec_HooksUninstall_LeavesAnUntouchedFileByteIdentical
//
// removeHooks rewrites with json.MarshalIndent, which reorders and reformats.
// A config with none of our markers must not be rewritten at all — otherwise
// `hooks uninstall` reformats every agent config on the machine, including the
// three platforms the user never used.
func TestSec_HooksUninstall_LeavesAnUntouchedFileByteIdentical(t *testing.T) {
	sec10Home(t)
	path := ConfigPath("", "gemini")
	raw := []byte("{\n\t\"hooks\": {\n\t\t\"AfterTool\": [{\"hooks\":[{\"type\":\"command\",\"command\":\"my-tool\"}]}]\n\t},\n\t\"theme\": \"dark\"\n}\n")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Uninstall([]string{"gemini"}); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(raw) {
		t.Errorf("Uninstall rewrote a config it removed nothing from:\n--- before ---\n%s\n--- after ---\n%s", raw, after)
	}
}

// ---------------------------------------------------------------------------
// 5. installHermes' copy of the converge block — the YAML sibling round 9
//    never asserted.
// ---------------------------------------------------------------------------

func sec10ReadYAML(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := yaml.Unmarshal(data, &out); err != nil {
		t.Fatalf("parse %s: %v\n%s", path, err, data)
	}
	return out
}

func sec10HermesGroups(t *testing.T, path, event string) []any {
	t.Helper()
	hooks, _ := sec10ReadYAML(t, path)["hooks"].(map[string]any)
	arr, _ := hooks[event].([]any)
	return arr
}

// TestSec_HooksHermes_ConvergesInPlaceWithoutDuplicating
//
// installHermes repeats mergeJSONHooks' converge block over YAML: a managed
// group found by marker is REPLACED in place when its shape has drifted,
// never appended. That property is what stops a reinstall from stacking a
// second `bdrive sync` onto every pre_llm_call — the hook runs on every turn
// of every session, so a duplicate is a doubled sync and a doubled read count.
//
// Round 9 asserted this only on the JSON path. Asserted here on the YAML one:
// stale shape converges, a second install changes nothing, and a group the
// user wrote in the same event is neither replaced nor counted as ours.
func TestSec_HooksHermes_ConvergesInPlaceWithoutDuplicating(t *testing.T) {
	sec10Home(t)
	path := ConfigPath("", "hermes")

	// A config carrying a STALE managed group (an older command shape) plus one
	// group the user wrote.
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	stale := map[string]any{
		"hooks": map[string]any{
			"pre_llm_call": []any{
				map[string]any{"command": "sh -c 'bdrive sync . >/dev/null'", "timeout": 5},
				map[string]any{"command": "my-own-preflight", "timeout": 9},
			},
		},
	}
	data, err := yaml.Marshal(stale)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, changed, err := installHermes(""); err != nil || !changed {
		t.Fatalf("installHermes over a stale config: changed=%v err=%v, want a converge", changed, err)
	}

	pre := sec10HermesGroups(t, path, "pre_llm_call")
	if len(pre) != 2 {
		t.Fatalf("pre_llm_call has %d groups, want 2 (converged managed group + the user's): %#v", len(pre), pre)
	}
	managed, mine := 0, 0
	for _, g := range pre {
		grp, _ := g.(map[string]any)
		if cmd, _ := grp["command"].(string); cmd == hookCommand("hermes") {
			managed++
		} else if cmd == "my-own-preflight" {
			mine++
			if to, _ := grp["timeout"].(int); to != 9 {
				t.Errorf("the user's group was rewritten: timeout = %v, want 9", grp["timeout"])
			}
		}
	}
	if managed != 1 {
		t.Errorf("pre_llm_call carries %d beardrive groups after converge, want exactly 1 — "+
			"a duplicate is a doubled sync on every turn", managed)
	}
	if mine != 1 {
		t.Errorf("the user's own pre_llm_call group did not survive installHermes")
	}

	// Idempotent: a second install must not write at all.
	beforeRun, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, changed, err := installHermes(""); err != nil || changed {
		t.Errorf("second installHermes: changed=%v err=%v, want no change", changed, err)
	}
	afterRun, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeRun) != string(afterRun) {
		t.Errorf("a no-op installHermes rewrote the file:\n%s\n---\n%s", beforeRun, afterRun)
	}
}

// TestSec_HooksHermes_UninstallRemovesBothManagedGroups
//
// The hermes side of Uninstall runs removeHooks with isYAML, over a file
// carrying three managed groups across two events. All three must go, the
// user's must stay, and the file must stay parseable YAML.
func TestSec_HooksHermes_UninstallRemovesBothManagedGroups(t *testing.T) {
	sec10Home(t)
	path := ConfigPath("", "hermes")
	if _, _, err := installHermes(""); err != nil {
		t.Fatal(err)
	}
	// Add one of the user's own after the fact.
	root := sec10ReadYAML(t, path)
	hooks, _ := root["hooks"].(map[string]any)
	arr, _ := hooks["post_tool_call"].([]any)
	hooks["post_tool_call"] = append(arr, map[string]any{"command": "my-own-post", "timeout": 3})
	data, err := yaml.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Uninstall([]string{"hermes"}); err != nil {
		t.Fatal(err)
	}
	if got := sec10HermesGroups(t, path, "pre_llm_call"); len(got) != 0 {
		t.Errorf("pre_llm_call still carries %#v after uninstall", got)
	}
	post := sec10HermesGroups(t, path, "post_tool_call")
	if len(post) != 1 {
		t.Fatalf("post_tool_call has %d groups after uninstall, want just the user's: %#v", len(post), post)
	}
	if grp, _ := post[0].(map[string]any); grp["command"] != "my-own-post" {
		t.Errorf("the surviving post_tool_call group is %#v, not the user's", post[0])
	}
}
