---
title: Connect an agent
description: Wire Claude Code, Codex, Gemini CLI, or Hermes into a BearDrive folder so it reads fresh team files every turn and pushes its own artifacts automatically.
---

An agent connected to BearDrive pulls the team's latest files at the start of
every turn and pushes whatever it writes the moment it writes it. Your teammate's
agent sees it seconds later.

## The two pieces

Whatever agent you use, setup is the same two things.

**The skill** teaches the agent the `bdrive` CLI, so "share this file" or "what
changed?" just works without you explaining anything.

```sh
bdrive skill install
```

It writes `SKILL.md` — a cross-agent format — into every detected platform's
user-level skills directory: `~/.claude/skills/beardrive/`, `~/.codex/…`,
`~/.gemini/…`, `~/.hermes/…`.

**The hooks** are what make syncing automatic: a blocking pull when you send a
message, so the agent always reads fresh files, and an async push when the turn
ends.

```sh
bdrive hooks install
```

Both commands are idempotent and both detect the platform (override with
`--agent`). The hook no-ops instantly in folders without a `.bdrive/` project,
so registering it globally is safe.

:::caution[Don't skip the hooks]
This is the step people skip when they copy commands by hand, and it's the one
that makes the whole thing automatic. Without it you're back to running
`bdrive sync` yourself and the agent will read stale files.
:::

## Claude Code

Install the plugin:

```
/plugin marketplace add runbear-io/beardrive
/plugin install beardrive@beardrive
```

Then run **`/beardrive:install`**. It sets the project up conversationally:
installs the CLI, signs in, creates or connects a project (whole folder or a
subfolder like `wiki/`), offers to write the
[agent orientation files](/guides/shared-agent-memory/), and registers
project-level hooks in `.claude/settings.json`.

Those project-level hooks are the part that matters for teams: they're committed
with the repository, so **teammates sync whether or not they installed the
plugin**.

Also available:

- **`/beardrive:init [folder]`** — just start syncing. Takes `--name`,
  `--project`, `--shared`.
- **`/beardrive:status`** — diagnose sync problems.

## Codex, Gemini CLI, Hermes

These agents ship no BearDrive knowledge, so setup is one paste. Start the agent
in the folder you want the files and give it:

```
Set up BearDrive in this folder.
1. If `bdrive` is missing, install it: brew install runbear-io/tap/beardrive
   (no Homebrew? grab the release binary for this OS/arch from
   https://github.com/runbear-io/beardrive/releases)
2. bdrive skill install   # so you know the CLI next time
3. bdrive login --device https://your-hub   # show me the code and the URL
4. bdrive init --project <project-id>
5. bdrive hooks install   # don't skip this - it's what syncs every turn
Then tell me what got set up.
```

You copy one thing; the agent handles every deviation — already installed, no
Homebrew, sign-in, wrong folder.

Step 2 is the durable part. Once the skill is installed, the agent knows the CLI
from then on without being told.

:::tip
A project's home page in the web UI shows this same paste with your hub URL and
project id already filled in, plus a plain-terminal version. Send teammates
there rather than retyping it.
:::

## Check what's wired up

```sh
bdrive skill     # what's installed on this machine
bdrive hooks     # what's registered for this project
bdrive status    # projects, daemon state, pending changes
```

Re-run `bdrive hooks install` once per project and `bdrive skill install` once
per machine after a CLI upgrade.

## Next

[Shared agent memory](/guides/shared-agent-memory/) — a freshly connected folder
is hundreds of opaque files to an agent. This is how you fix that.
