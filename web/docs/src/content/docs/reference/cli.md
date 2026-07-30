---
title: CLI reference
description: Every bdrive command and what it does.
---

One binary, `bdrive` — the CLI, the sync daemon, and the web server.

## Commands

| Command | Description |
|---|---|
| `bdrive login [server-url]` | Sign this device in. Browser flow; `--device` forces the approval-link flow, and shells without a TTY (agents, CI, SSH) fall back to it automatically. Default server is beardrive.ai — the managed cloud, free personal workspace on signup; pass your hub URL to self-host. Switch hubs with `bdrive login <new-url>`. `--status` shows the current server and account |
| `bdrive logout` | Sign this device out — clear the saved token and account. `--forget` also drops the remembered server |
| `bdrive init [folder]` | Create or connect a project and start syncing — the mount is always exactly the folder named. Interactive on a TTY; flags (`--name`, `--project`, `--server`, `--only`, `--yes`) for scripts. Also installs the agent skill, registers agent sync hooks for detected platforms (`--no-hooks` skips the hooks only), and prints the project's hub link. Re-run to resume |
| `bdrive stop [folder]` | Stop syncing — daemon and agent sync hooks both pause. Files stay on disk; `bdrive init` resumes |
| `bdrive scope [add\|rm <dirs...>]` | Show or change which subfolders sync — edits the managed block of `.bdriveignore` rules that `init --only` writes. Run from the mount root; the daemon picks changes up in seconds. `rm` stops syncing a folder but deletes nothing, locally or on the hub |
| `bdrive scope --explain` | List every path in the folder, split into what syncs and what does not, with counts — the verifiable answer to "what leaves this machine". Pure read: no daemon, no lock, no network |
| `bdrive forget <path>...` | Stop syncing a path and remove it from the hub. Adds the rule to `.bdriveignore` (which syncs) and prunes in one step. Local files are never touched, here or on teammates' devices |
| `bdrive url [path]` | Internal hub link for a file or folder — sign-in and membership required. `--sync` pushes first; no argument gives the project home. Computed locally |
| `bdrive share <file>` | Public URL for a synced file. `--list`, `--revoke`, `--expires` (the hub's Share dialog can also set an expiry on an existing link) |
| `bdrive sync [folder]` | Run one sync cycle now. Refuses folders this device never `init`ed and folders paused by `bdrive stop`. `--note <text>` stamps session context onto changes; `--note-ttl` (default 30m) bounds it. `--prune` also removes from the hub what `.bdriveignore` now excludes (files stay on disk everywhere). `--hook <label>` is agent-hook plumbing |
| `bdrive hooks [install\|uninstall]` | Register turn-boundary sync hooks in each detected agent platform's user config — once per machine, covering every folder. Run automatically by `bdrive init`; idempotent; `--agent` overrides detection. `uninstall` removes only BearDrive's own hook entries |
| `bdrive skill [install]` | Install the `beardrive` skill into detected agent platforms so the agent can do setup itself. Run automatically by `bdrive init`; idempotent; `--agent` overrides detection |
| `bdrive hook-approve` | Hook plumbing: answers the beardrive plugin's `PreToolUse` hook, auto-approving bare `bdrive init\|login\|hooks\|status\|sync\|url` so setup costs no permission prompts. Anything with a shell operator is left to the normal prompt |
| `bdrive read-log [folder]` | Hook plumbing: queue agent file reads for the hub's read heatmap. Registered by `bdrive hooks install` |
| `bdrive status [folder]` | Projects, daemon state, pending changes |
| `bdrive log [folder] [-p path] [-n N]` | Change history: account, device, time, file |
| `bdrive restore <file> [version]` | Put an earlier version of a file back, as a new change. No version restores the previous one; `--list` shows the versions with their short hashes |
| `bdrive export [folder]` | Export the whole project — all devices' history and content — to a portable `.tar.gz` (`-o` names the file) |
| `bdrive import <archive>` | Import an export archive as a new project on the hub you're logged into (`--name` overrides the archive's name) |
| `bdrive web [folder \| storage-root-url]` | Web server: viewer, uploads, multi-project sync hub |
| `bdrive whoami` | Signed-in account and device identity used in change tracking |
| `bdrive version` | Version (also `bdrive --version`) |

## Notes on a few

### `bdrive init`

The front door. **The mount is always exactly the folder you name** —
`bdrive init wiki` makes `./wiki` the project, and its contents are the
project's contents. Nothing re-roots a mount somewhere else.

Interactive on a TTY, with survey menus for create-new versus
connect-existing (showing a project list) and whole-folder versus only some
subfolders. To sync part of a folder without moving the mount, use
`--only <dirs>` (comma-separated — `bdrive init . --only wiki,docs`), which
writes a managed block of `.bdriveignore` rules rather than a separate scope
setting. Full flag bypass with `--name`, `--project`, `--only`, `--yes`, and
it never prompts without a TTY.

It runs the login flow first when there is no session, writes
`.bdrive/config.json`, seeds `.bdriveignore`, installs the `beardrive` skill and
registers agent sync hooks for every detected platform (Claude Code, Codex,
Gemini CLI, Hermes — `--no-hooks` skips the hooks; the skill is installed either way), starts sync, and prints the
project's hub link. That is deliberate: one command means one permission prompt
for an agent, instead of four. Re-running it resumes — including after a folder
move.

The hooks land in each platform's **user** config (`~/.claude/settings.json` and
friends), once per machine, so they cover every session in every folder; nothing
is written inside the project. See
[Skills and hooks in detail](/manual/skills-and-hooks/).

The daemon scans every 3s and talks to the hub every 10s; those intervals are
tunable on `bdrive daemon run`, not on init.

### `bdrive sync --note`

Stamps session context — an agent session id, say — onto changes. It shows up in
`bdrive log` and hub history, and keeps applying to daemon-committed changes
until `--note-ttl` expires.

### `bdrive restore` — undoing a change

An agent rewrote a file you liked. Put the old bytes back:

```
$ bdrive restore docs/spec.md
restored docs/spec.md to the version from 2026-07-28 14:01 (a3f9c1e2, 12.0 KB)

$ bdrive restore docs/spec.md --list      # short hash, time, size, who
$ bdrive restore docs/spec.md a3f9c1e2    # a specific version (any unique prefix)
```

Restoring writes those bytes back as a **new change**. Nothing is erased: the
versions in between stay in the history, the restore itself shows up in
`bdrive log` and the hub's History view, and it syncs to every device and
teammate like any other edit — so you can restore away from a restore. The hub
has the same button on every history row.

**Known gap:** restore puts content back; it cannot yet remove a file, so a
file that a run *created* cannot be un-created. Delete it yourself and let the
next sync carry that.

### `bdrive forget` and `bdrive sync --prune` — cleaning up the hub

Adding a rule to `.bdriveignore` only stops *future* uploads. Anything that
synced before the rule existed stays on the hub. These two commands are how it
comes off:

```
$ bdrive forget .omc
added `.omc/` to .bdriveignore
synced /Users/you/notes (project "notes")
  ...
  pruned:         72 path(s) removed from the hub (kept on disk)

$ bdrive sync --prune       # same cleanup for rules you added by hand
```

`forget` writes the rule (a trailing `/` for a directory) and prunes in the
same run; it is idempotent, so re-running it just prunes. A path outside the
project is an error and writes nothing.

**No device loses a file.** The removal is journaled as an ordinary delete, and
because `.bdriveignore` syncs, every device receives the rule alongside the
delete and simply stops tracking the path — the file itself stays on disk here
and on every teammate's machine. Nothing is destroyed either: blobs are
retained forever, so the removal shows in `bdrive log` and every past version
stays in the hub's history.

Prune reconciles against `.bdriveignore`, which is shared — so it refuses
outright when those rules contain `!` scope rules (the "only these folders"
block that `init --only` and `bdrive scope` write). With such a scope, pruning
would mean deleting everything outside it from the hub for every teammate. To
remove a specific path, `bdrive forget` it — that writes the exclusion into the
shared rules first, which is what makes it safe.

Mounts created before the scope moved into `.bdriveignore` may still carry a
per-device `include` list in `.bdrive/config.json`; prune never reconciles
against that, so a narrow legacy scope still means "not on my disk", not "not
on the hub".

If a teammate edits the file between your prune and their next sync, their
version wins and the path comes back. Run `--prune` again once they have synced.

### `bdrive status` — and the two degraded access states

Alongside `pending`, `status` prints an `access:` line whenever the hub is
refusing this device. Neither is the same as being offline, and neither ever
touches your files:

```
  pending:  3 local change(s) not yet pushed
  access:   read-only (pull only) — 3 local change(s) stay on this device
```

- **`read-only (pull only)`** — you have `read` on the project. The daemon keeps
  pulling teammates' changes; your own edits stay journaled locally, never
  pushed and never dropped. They go out if you are granted `write` again.
- **`no access to this project — sync paused`** — your access was revoked.
  Nothing is pulled, pushed, or written; the working folder is left exactly as
  it is. Re-granting resumes on the next tick with no manual step.

`bdrive sync` shows the same two as `remote: read-only (pull only)` /
`remote: no access — sync paused`, and the daemon logs each once on
transition rather than on every tick. Both are permission answers: they are
fixed in the hub's Project settings → People, not on the device. See
[Project permissions](/concepts/permissions/).

### `bdrive login` and switching hubs

`bdrive login` remembers the server in `settings.json` under the bdrive home. To
move to a different hub, run `bdrive login <new-url>` and then re-run
`bdrive init` in each folder to connect it to a project there.

There is no client command to point a folder at a raw bucket. `init` always
writes a hub remote.

### `bdrive export` / `bdrive import` — moving a project between hubs

Re-running `init` against a new hub carries only your files' current state.
`export` + `import` carry the whole project: every device's journal and every
retained blob, so per-file history and authorship arrive intact — and devices
that later connect to the imported project resume exactly where they left off.

```sh
# on any machine that syncs the project (run bdrive sync first)
bdrive export ~/team-wiki -o team-wiki.tar.gz

# point the device at the destination hub and import
bdrive login https://your-hub.example
bdrive import team-wiki.tar.gz
bdrive init --project p-xxxxxxxx   # connect folders to the new project
```

This works in both directions — cloud to self-hosted or self-hosted to
cloud. The archive is the project's store layout in a tar.gz; the target
project must be empty, and the destination hub needs uploads enabled. Shares,
invite links, and read-heat stay behind (they belong to the hub, not the
project store). Step-by-step walkthrough:
[Migrate between hubs](/reference/migration/).

## Environment

| Variable | Effect |
|---|---|
| `BDRIVE_HOME` | Relocate all BearDrive state — device identity, settings, mount registry, volume stores — away from `~/.bdrive`. Useful for testing |
| `BDRIVE_TOKEN` | Device token, taking precedence over `settings.json` |
