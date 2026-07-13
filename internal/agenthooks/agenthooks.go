// Package agenthooks detects which AI agent platforms a user works with and
// registers BearDrive's sync hooks in each platform's own hook config, so
// files sync at turn boundaries no matter which agent edits them.
//
// Every supported platform runs command hooks the same way — spawn a shell
// command, pipe event JSON (with a session_id) on stdin — so one hook command
// works everywhere; only the config file format and event names differ:
//
//	claude  .claude/settings.json    UserPromptSubmit / PostToolUse   (project)
//	codex   .codex/hooks.json        UserPromptSubmit / PostToolUse   (project)
//	gemini  .gemini/settings.json    BeforeAgent / AfterTool          (project)
//	hermes  ~/.hermes/config.yaml    pre_llm_call / post_tool_call    (user)
//
// The hook syncs the project and stamps changes with "<agent> session <id>"
// (see `bdrive sync --note`), so hub history links every change to the agent
// session that made it. A third hook runs `bdrive read-log` on each
// platform's read-shaped tools — native file reads, grep-style searches
// (the files the matches came from), and shell commands (the existing files
// they name) — queueing agent file reads for the hub's read heatmap
// (drained on the next sync — the hook itself never touches the network).
// Listing tools (glob, ls) are deliberately unmatched: seeing a file's name
// is not reading it. Hooks are fast no-ops outside bdrive projects, and
// reinstalling upgrades a registered hook's matcher in place when coverage
// grows.
package agenthooks

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/runbear-io/beardrive/internal/store"
)

// Markers identify our hooks inside a config, for idempotency and status.
// The sync and read hooks are separate groups (different matchers), so each
// carries its own marker — re-running install on a config that predates the
// read hook adds just the missing group.
const (
	marker     = "bdrive sync"
	readMarker = "bdrive read-log"
)

// Agent names, in the order they are reported.
var Agents = []string{"claude", "codex", "gemini", "hermes"}

// Result reports what Install did for one agent platform.
type Result struct {
	Agent   string
	Path    string // config file the hooks live in
	Changed bool   // false = already registered
	Note    string // extra step the user must take, if any
}

// hookCommand is the one shell command every platform runs: sync the project
// if it is a bdrive mount, stamping changes with the agent session id parsed
// from the hook's stdin JSON. POSIX sh only — no jq, no bashisms.
func hookCommand(label string) string {
	return `sh -c '` +
		`cd "${CLAUDE_PROJECT_DIR:-.}" && [ -d .bdrive ] && command -v bdrive >/dev/null || exit 0; ` +
		`s=; [ -t 0 ] || s=$(head -c 8192 2>/dev/null | tr -d \" | sed -n "s/.*session_id[[:space:]]*:[[:space:]]*\([a-zA-Z0-9_-]*\).*/\1/p" | head -n 1); ` +
		`if [ -n "$s" ]; then bdrive sync . --note "` + label + ` session $s" >/dev/null 2>&1 || true; ` +
		`else bdrive sync . >/dev/null 2>&1 || true; fi'`
}

// readHookCommand queues agent file reads for the hub's read heatmap:
// `bdrive read-log` parses the hook's stdin JSON itself and only appends to
// a local spool, so this stays cheap enough to run on every read-tool call.
func readHookCommand() string {
	return `sh -c '` +
		`cd "${CLAUDE_PROJECT_DIR:-.}" && [ -d .bdrive ] && command -v bdrive >/dev/null || exit 0; ` +
		`bdrive read-log . >/dev/null 2>&1 || true'`
}

type platform struct {
	label      string // session-note label
	projectDir string // presence of this dir (project or home) = detected
	userLevel  bool   // config lives in the home dir, not the project
	install    func(folder string) (path string, changed bool, err error)
	note       string
}

var platforms = map[string]platform{
	"claude": {
		label:      "claude-code",
		projectDir: ".claude",
		install: func(folder string) (string, bool, error) {
			// Reads happen through more than the Read tool: Grep consumes
			// the files its matches come from, and Bash reads whatever
			// files the command names (`read-log` mines both payloads).
			// Glob stays unmatched on purpose — listing names isn't reading.
			return mergeJSONHooks(filepath.Join(folder, ".claude", "settings.json"),
				"UserPromptSubmit", "PostToolUse", "Write|Edit|MultiEdit", "Read|Grep|Bash", "claude-code", 30, true)
		},
	},
	"codex": {
		label:      "codex",
		projectDir: ".codex",
		install: func(folder string) (string, bool, error) {
			// Codex reads mostly happen through shell commands; read-log
			// mines the command line for the files it names.
			return mergeJSONHooks(filepath.Join(folder, ".codex", "hooks.json"),
				"UserPromptSubmit", "PostToolUse", "apply_patch", "read_file|shell", "codex", 30, false)
		},
		note: "run /hooks inside Codex once to trust the project's .codex layer",
	},
	"gemini": {
		label:      "gemini",
		projectDir: ".gemini",
		install: func(folder string) (string, bool, error) {
			// Gemini uses its own event names and millisecond timeouts.
			return mergeJSONHooks(filepath.Join(folder, ".gemini", "settings.json"),
				"BeforeAgent", "AfterTool", "write_file|replace|edit",
				"read_file|read_many_files|search_file_content|run_shell_command", "gemini", 30000, false)
		},
	},
	"hermes": {
		label:     "hermes",
		userLevel: true,
		install:   installHermes,
	},
}

// Detect reports which agent platforms are in use, judged by their config
// dirs existing in the project or the home directory.
func Detect(folder string) []string {
	home, _ := os.UserHomeDir()
	var found []string
	for _, name := range Agents {
		p := platforms[name]
		switch {
		case p.userLevel:
			if dirExists(filepath.Join(home, "."+name)) {
				found = append(found, name)
			}
		case dirExists(filepath.Join(folder, p.projectDir)) ||
			(home != "" && dirExists(filepath.Join(home, p.projectDir))):
			found = append(found, name)
		}
	}
	return found
}

// Registered reports whether an agent's config already carries our hooks.
func Registered(folder, agent string) bool {
	data, err := os.ReadFile(ConfigPath(folder, agent))
	return err == nil && strings.Contains(string(data), marker)
}

// ConfigPath returns where an agent's hooks are (or would be) registered.
func ConfigPath(folder, agent string) string {
	switch agent {
	case "claude":
		return filepath.Join(folder, ".claude", "settings.json")
	case "codex":
		return filepath.Join(folder, ".codex", "hooks.json")
	case "gemini":
		return filepath.Join(folder, ".gemini", "settings.json")
	case "hermes":
		home, _ := os.UserHomeDir()
		return filepath.Join(home, ".hermes", "config.yaml")
	}
	return ""
}

// Install registers the sync hooks for the given agents ("auto"/empty =
// every detected platform). Merging is idempotent and preserves whatever
// hooks the config already has.
func Install(folder string, agents []string) ([]Result, error) {
	if len(agents) == 0 || (len(agents) == 1 && agents[0] == "auto") {
		agents = Detect(folder)
	}
	var out []Result
	for _, name := range agents {
		p, ok := platforms[name]
		if !ok {
			return out, fmt.Errorf("unknown agent %q (supported: %s)", name, strings.Join(Agents, ", "))
		}
		path, changed, err := p.install(folder)
		if err != nil {
			return out, fmt.Errorf("%s: %w", name, err)
		}
		out = append(out, Result{Agent: name, Path: path, Changed: changed, Note: p.note})
	}
	return out, nil
}

// mergeJSONHooks adds the pull + push + read hook trio to a Claude-style
// hooks JSON file (Claude, Codex, and Gemini all use this shape:
// hooks.<Event> is an array of {matcher?, hooks: [{type: "command", ...}]}
// groups). Push and read share the tool-use event under different matchers,
// each idempotent on its own marker.
func mergeJSONHooks(path, pullEvent, pushEvent, pushMatcher, readMatcher, label string, timeout int, async bool) (string, bool, error) {
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(data, &root); err != nil {
			return path, false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	cmd := hookCommand(label)
	pull := map[string]any{"hooks": []any{map[string]any{
		"type": "command", "command": cmd, "timeout": timeout,
		"statusMessage": "beardrive: pulling latest files",
	}}}
	pushHook := map[string]any{"type": "command", "command": cmd, "timeout": timeout}
	readHook := map[string]any{"type": "command", "command": readHookCommand(), "timeout": timeout}
	if async {
		pushHook["async"] = true
		readHook["async"] = true
	}
	push := map[string]any{"matcher": pushMatcher, "hooks": []any{pushHook}}
	read := map[string]any{"matcher": readMatcher, "hooks": []any{readHook}}

	changed := false
	for _, g := range []struct {
		event   string
		group   map[string]any
		marker  string
		matcher string // non-empty: keep an already-registered group's matcher current
	}{
		{pullEvent, pull, marker, ""},
		{pushEvent, push, marker, pushMatcher},
		{pushEvent, read, readMarker, readMatcher},
	} {
		arr, _ := hooks[g.event].([]any)
		if grp := findMarkerGroup(arr, g.marker); grp != nil {
			// Already registered — but upgrade a stale matcher in place so
			// coverage improvements reach existing projects on reinstall
			// (e.g. the read hook growing from "Read" to "Read|Grep|Bash").
			if g.matcher != "" && grp["matcher"] != g.matcher {
				grp["matcher"] = g.matcher
				changed = true
			}
			continue
		}
		hooks[g.event] = append(arr, g.group)
		changed = true
	}
	if !changed {
		return path, false, nil
	}
	return path, true, writeConfig(path, func() ([]byte, error) {
		return json.MarshalIndent(root, "", "  ")
	})
}

// installHermes merges the hook pair into ~/.hermes/config.yaml
// (hooks.<event> is an array of {matcher?, command, timeout}).
func installHermes(string) (string, bool, error) {
	path := ConfigPath("", "hermes")
	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		if err := yaml.Unmarshal(data, &root); err != nil {
			return path, false, fmt.Errorf("parse %s: %w", path, err)
		}
	} else if !os.IsNotExist(err) {
		return path, false, err
	}
	hooks, ok := root["hooks"].(map[string]any)
	if !ok {
		hooks = map[string]any{}
		root["hooks"] = hooks
	}
	cmd := hookCommand("hermes")
	groups := []struct {
		event   string
		group   map[string]any
		marker  string
		matcher string
	}{
		{"pre_llm_call", map[string]any{"command": cmd, "timeout": 30}, marker, ""},
		{"post_tool_call", map[string]any{"matcher": "write_file|patch", "command": cmd, "timeout": 30}, marker, "write_file|patch"},
		{"post_tool_call", map[string]any{"matcher": "read_file|grep|bash", "command": readHookCommand(), "timeout": 30}, readMarker, "read_file|grep|bash"},
	}
	changed := false
	for _, g := range groups {
		arr, _ := hooks[g.event].([]any)
		if grp := findMarkerGroup(arr, g.marker); grp != nil {
			if g.matcher != "" && grp["matcher"] != g.matcher {
				grp["matcher"] = g.matcher
				changed = true
			}
			continue
		}
		hooks[g.event] = append(arr, g.group)
		changed = true
	}
	if !changed {
		return path, false, nil
	}
	return path, true, writeConfig(path, func() ([]byte, error) {
		return yaml.Marshal(root)
	})
}

// containsMarker reports whether a hook array already holds the hook the
// marker identifies. Serializing sidesteps walking every platform's nesting
// by hand.
func containsMarker(v any, m string) bool {
	data, err := json.Marshal(v)
	return err == nil && strings.Contains(string(data), m)
}

// findMarkerGroup returns the hook group in the array that carries the
// marker (so its matcher can be upgraded in place), or nil.
func findMarkerGroup(arr []any, m string) map[string]any {
	for _, it := range arr {
		grp, ok := it.(map[string]any)
		if ok && containsMarker(grp, m) {
			return grp
		}
	}
	return nil
}

func writeConfig(path string, marshal func() ([]byte, error)) error {
	data, err := marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return store.WriteFileAtomic(path, append(data, '\n'), 0o644)
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
