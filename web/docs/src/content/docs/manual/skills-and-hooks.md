---
title: Skills and hooks in detail
description: What bdrive skill install and bdrive hooks install actually write — the user-level config paths, hook events, idempotency, and when to re-run them.
---

`bdrive init` runs both of these for you — that is why setup is one command. This
is what they do, for when you want to run them yourself, review what changed, or
debug a folder that isn't syncing.

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
bdrive hooks install            # every detected platform, this machine
bdrive hooks install --agent claude,codex,gemini,hermes
bdrive hooks                    # status table
bdrive hooks uninstall          # remove them again
```

Each platform gets the same three hooks written into its own user-level config,
in its own format:

| Platform | Config it writes | Pull / push / read events |
|---|---|---|
| Claude Code | `~/.claude/settings.json` | `UserPromptSubmit` / `PostToolUse` (Write\|Edit\|MultiEdit) / `PostToolUse` (Read\|Grep\|Bash) |
| Codex | `~/.codex/hooks.json` | `UserPromptSubmit` / `PostToolUse` (apply_patch) / `PostToolUse` (read_file\|shell) |
| Gemini CLI | `~/.gemini/settings.json` | `BeforeAgent` / `AfterTool` (write_file\|replace\|edit) / `AfterTool` (read tools) |
| Hermes | `~/.hermes/config.yaml` | `pre_llm_call` / `post_tool_call` (write_file\|patch) / `post_tool_call` (read_file\|grep\|bash) |

Three hooks, three jobs:

- **Pull**, before the agent answers, so it never reads a stale file. This one
  blocks — it is the only place BearDrive makes you wait, and it is why the
  whole thing works.
- **Push**, after an edit, so teammates see the change within seconds rather
  than whenever a daemon tick lands.
- **Read tracking**, on the agent's read-shaped tools, queued locally and sent
  on the next sync. This is what fills the [Dashboard](/guides/what-agents-read/).
  Listing tools are deliberately excluded: seeing a filename is not reading it.

Every platform pipes hook JSON with a session id, so one hook command serves all
four, and changes are stamped with `<agent> session <id>` — visible in
`bdrive log` and the hub's history.

Codex hooks are experimental and off by default. Turn them on in
`~/.codex/config.toml`:

```toml
[features]
codex_hooks = true
```

Codex then asks once to trust the hook definition. Answer yes.

## Both are safe to re-run

Merging is idempotent and preserves hooks you already have. Each hook carries
its own marker, so a config written before a hook existed gains just the missing
one, and a registered hook's matcher is upgraded in place when coverage grows.

Re-run after a CLI upgrade: `bdrive hooks install` and `bdrive skill install`,
once per machine each.

## Where they live matters

Hooks are registered **once per machine**, in each platform's own user config —
never inside a project. Agent platforms read hook config only from the directory
a session starts in: never a parent, never a subfolder. A file in the project
would fire only for the sessions that happened to start exactly there, and — if
the project is synced — would travel to the whole team. A user-level
registration covers every session in every folder instead.

So BearDrive writes no agent-config directory into your project, and teammates
don't inherit your hooks: each device registers its own the first time it runs
`bdrive init`. Earlier versions did write project-level hooks; installing strips
those out as it goes, so nothing ends up running twice.

The hook opens with a shell guard that makes it a fast no-op in any folder
without a `.bdrive/` directory, which is what makes registering it globally safe.

`bdrive hooks uninstall` takes them back out — it removes only BearDrive's own
entries and leaves every other hook in those files untouched. Syncing itself is
unaffected; only turn-boundary sync stops.
