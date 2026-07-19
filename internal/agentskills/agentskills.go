// Package agentskills installs the `beardrive` skill into the agent
// platforms a user works with, so their agent knows how to drive the CLI —
// including running `bdrive init` and `bdrive hooks install` itself, which is
// how hooks end up registered correctly without the user hand-copying
// commands.
//
// SKILL.md is a cross-agent standard: every supported platform discovers
// skills the same way — a directory per skill under the platform's config
// dir, holding a SKILL.md with `name` + `description` frontmatter:
//
//	claude  ~/.claude/skills/beardrive/SKILL.md
//	codex   ~/.codex/skills/beardrive/SKILL.md
//	gemini  ~/.gemini/skills/beardrive/SKILL.md
//	hermes  ~/.hermes/skills/beardrive/SKILL.md
//
// Installs are user-level on purpose: the skill is about the CLI, not about
// one folder, and a synced project folder should never carry it. The skill
// is the binary's own copy (embedded at build time), so upgrading bdrive and
// re-running install refreshes it — writes are idempotent and report whether
// anything changed.
package agentskills

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/runbear-io/beardrive/internal/store"
	"github.com/runbear-io/beardrive/plugin"
)

// Agents is the supported platforms, in the order they are reported.
var Agents = []string{"claude", "codex", "gemini", "hermes"}

// Result reports what Install did for one agent platform.
type Result struct {
	Agent   string
	Path    string // SKILL.md written (or already current)
	Changed bool   // false = the installed copy already matched
}

// Path returns where an agent reads (or would read) the beardrive skill.
// Empty if the agent is unknown or the home directory is undiscoverable.
func Path(agent string) string {
	if !supported(agent) {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, "."+agent, "skills", "beardrive", "SKILL.md")
}

// Detect reports which agent platforms are in use, judged — like
// agenthooks.Detect — by their config dirs existing in the project or the
// home directory.
func Detect(folder string) []string {
	home, _ := os.UserHomeDir()
	var found []string
	for _, a := range Agents {
		dir := "." + a
		if dirExists(filepath.Join(folder, dir)) || (home != "" && dirExists(filepath.Join(home, dir))) {
			found = append(found, a)
		}
	}
	return found
}

// Installed reports whether an agent already has the current skill.
func Installed(agent string) bool {
	path := Path(agent)
	if path == "" {
		return false
	}
	data, err := os.ReadFile(path)
	return err == nil && string(data) == plugin.SkillMD
}

// Install writes the skill for the given agents ("auto"/empty = every
// detected platform). An outdated copy is overwritten — the file is ours.
func Install(folder string, agents []string) ([]Result, error) {
	if len(agents) == 0 || (len(agents) == 1 && agents[0] == "auto") {
		agents = Detect(folder)
	}
	var out []Result
	for _, a := range agents {
		if !supported(a) {
			return out, fmt.Errorf("unknown agent %q (supported: %s)", a, strings.Join(Agents, ", "))
		}
		path := Path(a)
		if path == "" {
			return out, fmt.Errorf("%s: cannot locate home directory", a)
		}
		if Installed(a) {
			out = append(out, Result{Agent: a, Path: path})
			continue
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return out, fmt.Errorf("%s: %w", a, err)
		}
		if err := store.WriteFileAtomic(path, []byte(plugin.SkillMD), 0o644); err != nil {
			return out, fmt.Errorf("%s: %w", a, err)
		}
		out = append(out, Result{Agent: a, Path: path, Changed: true})
	}
	return out, nil
}

func supported(agent string) bool {
	for _, a := range Agents {
		if a == agent {
			return true
		}
	}
	return false
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
