---
title: CLI reference
description: Every bdrive command and what it does.
---

One binary, `bdrive` — the CLI, the sync daemon, and the web server.

## Commands

| Command | Description |
|---|---|
| `bdrive login [server-url]` | Sign this device in. Browser flow; `--device` forces the code flow, and shells without a TTY (agents, CI, SSH) fall back to it automatically. Default server is beardrive.ai — the managed cloud, free personal workspace on signup; pass your hub URL to self-host. Switch hubs with `bdrive login <new-url>`. `--status` shows the current server and account |
| `bdrive logout` | Sign this device out — clear the saved token and account. `--forget` also drops the remembered server |
| `bdrive init [folder]` | Create or connect a project and start syncing. Interactive on a TTY; flags (`--name`, `--project`, `--shared`, `--yes`) for scripts. Re-run to resume |
| `bdrive stop [folder]` | Stop syncing. Files stay on disk; `bdrive init` resumes |
| `bdrive url [path]` | Internal hub link for a file or folder — sign-in and membership required. `--sync` pushes first; no argument gives the project home. Computed locally |
| `bdrive share <file>` | Public URL for a synced file. `--list`, `--revoke`, `--expires` |
| `bdrive sync [folder]` | Run one sync cycle now. `--note <text>` stamps session context onto changes; `--note-ttl` (default 30m) bounds it. `--hook <label>` is agent-hook plumbing |
| `bdrive hooks [install]` | Register turn-boundary sync hooks with detected agent platforms. Idempotent; `--agent` overrides detection |
| `bdrive skill [install]` | Install the `beardrive` skill into detected agent platforms so the agent can do setup itself. Idempotent; `--agent` overrides detection |
| `bdrive read-log [folder]` | Hook plumbing: queue agent file reads for the hub's read heatmap. Registered by `bdrive hooks install` |
| `bdrive status [folder]` | Projects, daemon state, pending changes |
| `bdrive log [folder] [-p path] [-n N]` | Change history: account, device, time, file |
| `bdrive web [folder \| storage-root-url]` | Web server: viewer, uploads, multi-project sync hub |
| `bdrive whoami` | Signed-in account and device identity used in change tracking |
| `bdrive version` | Version (also `bdrive --version`) |

## Notes on a few

### `bdrive init`

The front door. Interactive on a TTY, with survey menus for create-new versus
connect-existing (showing a project list) and whole-folder versus
`--shared <dir>` (which becomes the include list). Full flag bypass with
`--name`, `--project`, `--shared`, `--yes`, and it never prompts without a TTY.

It runs the login flow first when there is no session, writes
`.bdrive/config.json`, seeds `.bdriveignore`, and starts sync. Re-running it
resumes — including after a folder move.

Daemon intervals are tunable here: `--scan-interval` (default 3s) and
`--remote-interval` (default 10s).

### `bdrive sync --note`

Stamps session context — an agent session id, say — onto changes. It shows up in
`bdrive log` and hub history, and keeps applying to daemon-committed changes
until `--note-ttl` expires.

### `bdrive login` and switching hubs

`bdrive login` remembers the server in `settings.json` under the bdrive home. To
move to a different hub, run `bdrive login <new-url>` and then re-run
`bdrive init` in each folder to connect it to a project there.

There is no client command to point a folder at a raw bucket. `init` always
writes a hub remote.

## Environment

| Variable | Effect |
|---|---|
| `BDRIVE_HOME` | Relocate all BearDrive state — device identity, settings, mount registry, volume stores — away from `~/.bdrive`. Useful for testing |
| `BDRIVE_TOKEN` | Device token, taking precedence over `settings.json` |
