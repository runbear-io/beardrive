---
title: Quickstart
description: Sign a device in, start syncing a folder, and connect a second machine — the whole loop in a few minutes.
---

Three steps: sign the device in, start syncing a folder, work normally.

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
SSH machine, `bdrive login --device` prints a short code you approve from any
signed-in browser instead.

`bdrive login --status` shows the current server and account.

## 2. Start syncing a project

Once per project.

```console
$ cd ~/workspace && bdrive init
initialized /Users/snow/workspace
  project: workspace (p-7f3a2c91)
  daemon:  running (pid 55434, scan 3s, remote sync 10s)
```

On a terminal, `init` walks you through two questions: **create a new project or
connect an existing one** (picked from the server's list), and **sync the whole
folder or only a subfolder** such as `./shared`.

Every question has a flag — `--name`, `--project`, `--shared`, `--yes` — and
without a TTY init never prompts. It creates-or-joins a project named after the
folder and syncs everything.

Init writes `.bdrive/config.json`, seeds a starter `.bdriveignore`
(node_modules, build dirs, caches, `.env*`), and starts the daemon. Not signed
in yet? It runs the login flow first.

:::tip[Working inside a repository]
Sync a subfolder rather than the repo root: `bdrive init --shared docs`. Git
directories are never synced (per-file last-writer-wins would corrupt a
repository), but a narrower scope keeps the sync surface honest.
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

- [Connect an agent](/guides/connect-an-agent/) — wire Claude Code, Codex,
  Gemini CLI, or Hermes into this folder. This is what BearDrive is for.
- [Shared agent memory](/guides/shared-agent-memory/) — orient agents in the
  folder so they know where to read and write.
- [Artifacts and links](/guides/agent-artifacts/) — internal links for
  teammates, public share links for everyone else.
