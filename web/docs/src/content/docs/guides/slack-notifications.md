---
title: Tell Slack what your agents changed
description: Post every teammate and agent change to a Slack or Teams channel — either locally with post_sync, or hub-side with a project webhook.
---

A teammate's agent writes *"we're dropping Redis for Postgres"* into
`runbook.md`. It lands in your folder in about fifteen seconds. Nothing tells
you. BearDrive answers "who changed this" perfectly once you go looking — the
point of this page is to stop you having to look.

There are two ways to do it, and they cover different people.

| | Runs where | Covers | Needs |
| -- | -- | -- | -- |
| **`post_sync`** | your device | everything that lands in *your* folder | a daemon running |
| **Project webhook** | the hub | every write to the project, including browser uploads | a project admin |

Set up whichever matches who needs telling. They are independent; a project
can have both.

## The copy that matters

The message should name the actor, not the file count:

```
Dana Kim's Claude updated shared/findings/eu-checkout.md
Dana Kim updated shared/findings/eu-checkout.md
Sam Ito deleted notes/retired.md
```

The first line is an agent write — the agent sync hook stamps the platform
into the op's note. The second is a plain daemon push with no agent involved.
"1 file changed" tells nobody anything; every recipe below names a person.

One caveat, stated plainly: the note is **user-settable**
(`bdrive sync --note "…"`) and **display-only**. It names an agent; it never
proves one. Treat it as a label, not as attribution you would act on.

## Option 1 — `post_sync` on your device

Create an [incoming webhook](https://api.slack.com/messaging/webhooks) in
Slack (or a Workflows webhook in Teams) and copy the URL. Then point your
folder's `post_sync` at a script:

```jsonc
// .bdrive/config.json
{ "id": "m-5a10b713", "volume": "notes",
  "remote": "https://drive.example.com/p/7f3a2c91-…",
  "post_sync": "./.bdrive-notify.sh" }
```

The applied batch arrives as JSON on stdin, one object per cycle:

```json
{ "project": "m-5a10b713", "folder": "/Users/you/notes",
  "changed": [ { "path": "shared/findings/eu-checkout.md", "op": "write",
                 "user": "Dana Kim", "note": "claude session 41f2" },
               { "path": "notes/retired.md", "op": "delete",
                 "user": "Sam Ito" } ] }
```

`user` is the signed-in account's name, falling back to the account email and
then to the device's git/OS identity — the same order `bdrive log` prints.
`note` carries the agent session when one wrote the change. Both are omitted
when unknown, so a script written before they existed keeps working.

A minimal recipe, using `jq`:

```sh
#!/bin/sh
# .bdrive-notify.sh — chmod +x this, and keep it out of sync with .bdriveignore
HOOK="https://hooks.slack.com/services/T0/B0/xxxx"
text=$(jq -r '
  .changed
  | .[0:20]
  | map(
      (.user // "Someone")
      + (if (.note // "") | test("^claude ") then "'"'"'s Claude" else "" end)
      + (if .op == "delete" then " deleted " else " updated " end)
      + .path
    )
  | join("\n")')
[ -n "$text" ] && curl -sf -X POST -H 'Content-type: application/json' \
  --data "$(jq -n --arg t "$text" '{text:$t}')" "$HOOK" >/dev/null
```

Two things worth knowing before you turn it on:

- **The first cycle on a fresh folder materializes everything.** That is one
  invocation carrying every path in the project — hence the `.[0:20]` cap
  above. Without it, day one is a wall of several hundred lines.
- **It fires once per cycle**, inbound only. A cycle that just pushes your own
  edits sends nothing, and a hook that hangs or exits non-zero is logged and
  forgotten — it can never break sync.

`post_sync` lives in `.bdrive/config.json`, which never syncs. Nobody else can
put a command on your machine. See
[Project files](/reference/project-files/) for the full field reference.

## Option 2 — a project webhook on the hub

The device-side recipe only covers people running a daemon. If a teammate
works entirely in the browser — uploading files, restoring versions — their
writes never touch anyone's `post_sync` until a device syncs them. A project
webhook is the hub telling the channel directly.

A **project admin** opens **Settings → General → Change notifications**,
pastes the incoming-webhook URL, and saves. That is the whole surface: there
is no prompt, no nav entry and no empty-state card. A project with no webhook
set produces no notifications and makes no outbound requests at all.

What you get:

- One message per journal write, batching that write's changes into a single
  post, capped at 20 lines plus an "…and N more".
- Every hub-side write covered — device syncs, browser uploads, removes,
  restores and run undo.
- The message names the actor, in the same wording as above.

Some deliberate limits:

- **Only Slack and Teams hosts are accepted**: `hooks.slack.com`,
  `*.webhook.office.com` and `*.logic.azure.com`, `https://` only. A URL the
  hub fetches on an admin's say-so is a way to make the hub reach things it
  should not, and the host check closes that. Mattermost, Discord and n8n are
  not accepted today.
- **The URL never leaves the server.** Once saved, no API response ever
  returns it — the settings page shows *On* and a **Turn off** button, and
  pasting a new URL replaces the old one. It is a credential: whoever holds it
  can post into your channel.
- **No retries.** A delivery that fails is logged once and dropped. This is a
  notifier, not a queue.
- **No filtering or quiet window** in this version. A busy agent session on
  the default ten-second sync interval is a handful of messages a minute per
  device — worth knowing before you point it at a channel people read.
