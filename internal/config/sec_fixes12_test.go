package config_test

import (
	"testing"

	"github.com/runbear-io/beardrive/internal/config"
)

// Round 13 — attacking the boundary round 12 drew around agent config.
//
// agentHookConfigs states the rule it is enforcing in its own words: the shape
// that is reserved is "not 'content an agent reads' — the product's whole
// premise — but 'code the agent runs, chosen by whoever wrote the file'".
// Skills, commands, agents and CLAUDE.md are deliberately left out because they
// are the first kind.
//
// The table is a list of file names, not of that rule, and it was written from
// internal/agenthooks' own platform table. Claude Code's project-scoped MCP
// server file is not in that table and is squarely the SECOND kind: `.mcp.json`
// at the project root declares stdio servers as {"command": ..., "args": [...]}
// and the agent SPAWNS them. It is the one remaining per-project file whose
// contents are a command line rather than prose.
func TestSec_Reserved_AProjectScopedMCPServerFileIsAgentHookConfig(t *testing.T) {
	// The rule as round 12 implemented it, on the files it knew:
	for _, p := range []string{".claude/settings.json", ".claude/settings.local.json", ".codex/config.toml"} {
		if !config.ReservedPath(p) {
			t.Fatalf("fixture: %q should already be reserved by round 12", p)
		}
	}
	// And the file that carries the same capability and is not:
	for _, p := range []string{".mcp.json", "notes/.mcp.json"} {
		if !config.ReservedPath(p) {
			t.Errorf("%q syncs: a peer's project-scoped MCP server definition materializes into "+
				"every member's folder. Its {\"command\",\"args\"} pairs are processes the agent "+
				"launches — the same 'code the agent runs, chosen by whoever wrote the file' that "+
				"agentHookConfigs reserves .claude/settings.json for, and the same reason "+
				"internal/agenthooks refuses to write agent config into a mount at all.", p)
		}
	}
	// Case- and trailing-dot-folded, like every other reserved name (NTFS/APFS
	// resolve these onto the same file, which is the stated ground for
	// ReservedDir's folding).
	for _, p := range []string{".MCP.JSON", ".mcp.json."} {
		if !config.ReservedPath(p) {
			t.Errorf("%q syncs and the filesystem resolves it onto .mcp.json", p)
		}
	}
	// The line round 12 drew stays where it drew it: reading is the product.
	for _, p := range []string{"CLAUDE.md", ".claude/skills/x/SKILL.md", ".claude/commands/c.md", ".claude/agents/a.md"} {
		if config.ReservedPath(p) {
			t.Errorf("%q must keep syncing — sharing what an agent READS is the product, and "+
				"round 12 said so deliberately", p)
		}
	}
}
