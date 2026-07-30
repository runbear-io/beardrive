---
title: Set up by hand
description: The same setup an agent performs, run yourself — sign a device in, start syncing a folder, and connect a second machine.
---

This is [what your agent does for you](/start/setup/), one command at a time.
Useful on a machine with no agent, when scripting a fleet, or when you simply
want to see the moving parts.

Three steps: sign the device in, start syncing a folder, work normally. `init`
installs the agent skill and [registers the hooks](/manual/skills-and-hooks/)
along the way — that is what keeps an agent's files fresh, and it used to be the
step hand-setups forgot.

## 1. Sign this device in

Once per device.

```sh
bdrive login
```

Bare `bdrive login` targets [beardrive.ai](https://beardrive.ai) — the managed
cloud, where signup auto-creates a free personal workspace. Self-hosting? Pass
your hub's URL:

```sh
bdrive login https://your-hub
```

This opens your browser, and the terminal finishes on its own. On a headless or
SSH machine login falls back to the device-code flow automatically (no TTY, or
no browser can open): it prints one link you open in any signed-in browser and
approve — nothing to retype. `bdrive login --device` forces that flow.

`bdrive login --status` shows the current server and account.

## 2. Start syncing a project

Once per project.

```console
$ cd ~/workspace && bdrive init
initialized /Users/snow/workspace
  server:  https://your-hub
  project: workspace (p-7f3a2c91)
  skill:   installed for claude, codex
  claude   hooks registered  →  /Users/snow/.claude/settings.json
  daemon:  running (pid 55434, scan 3s, remote sync 10s)
```

On a terminal, `init` walks you through two questions: **create a new project or
connect an existing one** (picked from the server's list), and **sync the whole
folder or only a subfolder** such as `./shared`.

Every question has a flag — `--name`, `--project`, `--only`, `--yes` — and
without a TTY init never prompts. It creates-or-joins a project named after the
folder and syncs everything.

Init writes `.bdrive/config.json`, seeds a starter `.bdriveignore`
(node_modules, build dirs, caches, `.env*`), starts the daemon, and prints the
project's hub link. It also installs the `beardrive` skill and registers the sync
hooks for any agent platform it detects — in that platform's **user** config, once
per machine, so nothing lands inside the project. `--no-hooks` skips the hooks
(the skill is installed either way). Not
signed in yet? It runs the login flow first.

:::tip[Working inside a repository]
Sync a subfolder rather than the repo root: `bdrive init docs` makes `./docs`
the project and scans nothing else. For several subfolders in one project, mount
the repo and narrow it: `bdrive init . --only wiki,docs`, which writes the
narrowing as `.bdriveignore` rules. Git directories are never synced (per-file
last-writer-wins would corrupt a repository), but a narrower scope keeps the
sync surface honest. Adjust it later with `bdrive scope add`/`rm` — see
[Scoping the folder](/guides/scoping/).
:::

## 3. Work normally

Create, edit, and delete files with any tool. Local changes are detected within
seconds.

```sh
echo "remember this" > memory.md

bdrive log       # what changed, who changed it, from which device
bdrive status    # projects, daemon state, pending changes
bdrive stop      # stop syncing; files stay on disk, init resumes any time
```

## 4. Add a second machine

```sh
bdrive login https://your-hub    # once per device
cd ~/workspace && bdrive init    # connect the same project
```

The files appear and stay in sync.

## Moving a folder is safe

State is keyed by a stable project id, never the path. The daemon notices a move
or rename, steps aside, and the next `bdrive init` — or any bdrive command — at
the new location resumes exactly where it left off. Zero re-scan, zero spurious
changes.

## Next

- [Skills and hooks in detail](/manual/skills-and-hooks/) — what `init` wrote to
  make an agent read fresh files every turn, and how to inspect or remove it.
- [Shared agent memory](/guides/shared-agent-memory/) — orient agents in the
  folder so they know where to read and write.
- [Artifacts and links](/guides/agent-artifacts/) — internal links for
  teammates, public share links for everyone else.
