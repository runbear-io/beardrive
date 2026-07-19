// Package plugin embeds the shipped agent assets — currently the `beardrive`
// skill — so the CLI can install them into any agent that reads SKILL.md.
// The file embedded here is the same one the Claude Code plugin ships
// (plugin/skills/beardrive/SKILL.md): one canonical copy, no drift.
package plugin

import _ "embed"

// SkillMD is the beardrive skill: YAML frontmatter (name + description) plus
// the instructions body, the cross-agent SKILL.md format Claude Code, Codex,
// Gemini CLI, and Hermes all read.
//
//go:embed skills/beardrive/SKILL.md
var SkillMD string
