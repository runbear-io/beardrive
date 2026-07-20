---
title: Skills and hooks in detail
description: What bdrive skill install and bdrive hooks install actually write — per-platform paths, hook events, idempotency, and when to re-run them.
---

Setting up through your agent runs these two commands for you. This is what
they do, for when you want to run them yourself, review what changed, or debug a
folder that isn't syncing.

## The skill

```sh
bdrive skill install            # every detected platform
bdrive skill install --agent codex,hermes
bdrive skill                    # status table
```

It writes one `SKILL.md` — the cross-agent format — into each platform's
user-level skills directory:

| Platform | Path |
|---|---|
| Claude Code | `~/.claude/skills/beardrive/SKILL.md` |
| Codex | `~/.codex/skills/beardrive/SKILL.md` |
| Gemini CLI | `~/.gemini/skills/beardrive/SKILL.md` |
| Hermes | `~/.hermes/skills/beardrive/SKILL.md` |

Installs are user-level on purpose: the skill is about the CLI, not about one
folder, and a synced project folder should never carry it. The file is the
binary's own copy, so re-running after a CLI upgrade refreshes a stale one.

The Claude Code plugin ships the same skill, so `/plugin install
beardrive@beardrive` covers Claude without this command.

## The hooks

```sh
bdrive hooks install            # every detected platform, this project
bdrive hooks install --agent claude,codex,gemini,hermes
bdrive hooks                    # status table
```

Each platform gets the same three hooks written into its own config format:

| Platform | Config it writes | Pull / push / read events |
|---|---|---|
| Claude Code | `<project>/.claude/settings.json` | `UserPromptSubmit` / `PostToolUse` (Write\|Edit) / `PostToolUse` (Read\|Grep\|Bash) |
| Codex | `<project>/.codex/hooks.json` | `UserPromptSubmit` / `PostToolUse` (apply_patch) / `PostToolUse` (read_file\|shell) |
| Gemini CLI | `<project>/.gemini/settings.json` | `BeforeAgent` / `AfterTool` (write_file\|replace) / `AfterTool` (read tools) |
| Hermes | `~/.hermes/config.yaml` (per user) | `pre_llm_call` / `post_tool_call` (write_file\|patch) / `post_tool_call` (read_file\|grep\|bash) |

Three hooks, three jobs:

- **Pull**, before the agent answers, so it never reads a stale file. This one
  blocks — it is the only place BearDrive makes you wait, and it is why the
  whole thing works.
- **Push**, after an edit, so teammates see the change within seconds rather
  than whenever a daemon tick lands.
- **Read tracking**, on the agent's read-shaped tools, queued locally and sent
  on the next sync. This is what fills [Insights](/guides/what-agents-read/).
  Listing tools are deliberately excluded: seeing a filename is not reading it.

Every platform pipes hook JSON with a session id, so one hook command serves all
four, and changes are stamped with `<agent> session <id>` — visible in
`bdrive log` and the hub's history.

Codex asks once to trust the project's `.codex` layer. Answer yes, or run
`/hooks` inside Codex.

## Both are safe to re-run

Merging is idempotent and preserves hooks you already have. Each hook carries
its own marker, so a config written before a hook existed gains just the missing
one, and a registered hook's matcher is upgraded in place when coverage grows.

Re-run after a CLI upgrade: `bdrive hooks install` once per project, `bdrive
skill install` once per machine.

## Where they live matters

Claude Code, Codex, and Gemini CLI hooks are **project-level** — they ride the
repository, so a teammate who clones it syncs whether or not they installed
anything. Hermes hooks are **per-user** (`~/.hermes/config.yaml`), outside the
repo, so each person registers their own.

The hook is a fast no-op in any folder without a `.bdrive/` directory, which is
what makes registering it globally safe.
