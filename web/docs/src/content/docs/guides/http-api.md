---
title: Write files over HTTP
description: Persist output from an ephemeral agent sandbox or CI job with two curl calls — no CLI, no daemon, no device registration. Mint a token once, then PUT files into a project and GET them back.
---

The CLI is the right tool on a machine you keep. A CI job, a throwaway cloud
sandbox or a five-line script is not that machine: it lives for ninety
seconds and then it is gone, and installing a binary, registering a device
and running a sync daemon for that is absurd.

Those runs can write straight to the hub over HTTP instead.

```sh
curl -H "Authorization: Bearer $TOKEN" \
     -T notes.md \
     "$HUB/api/p/$PROJECT/upload/content?path=notes.md"
```

That's the whole write. The file lands in the project, appears in the
viewer, and shows up in History like any other change — every teammate's
next sync pulls it down.

## Before you start

You need three things:

| | |
|---|---|
| `$HUB` | Your hub's URL, e.g. `https://hub.example.com` (BearDrive Cloud: `https://beardrive.ai`) |
| `$PROJECT` | The project id — it's in the URL when you open the project in the hub, and on its **Installation** page |
| `$TOKEN` | A device token, minted once by a human (below) |

The hub must be running with uploads enabled (`bdrive serve … --upload`).
Without it every write answers `403 uploads are disabled on this server`.

## Mint a token

A token is minted once, by a person, in a browser — three calls, all
curl-able. There is no separate API-key type: this is the same device token
the CLI uses, and it is bound to the account that approves it.

**1. Start the flow.** No authentication needed; name the thing that will
hold the token, so the approval page and the hub's device list say something
recognizable later.

```sh
curl -s -X POST "$HUB/api/auth/device/start" \
     -H "Content-Type: application/json" \
     -d '{"device":"ci-runner","os":"linux"}'
```

```json
{ "code": "…", "verify_url": "https://hub.example.com/auth/device/…", "interval": 2 }
```

**2. Approve it.** Open `verify_url` in a browser signed in to the hub and
confirm. The link is the secret and it expires in 10 minutes — treat it like
a password, and only ever approve one you started yourself.

**3. Collect the token.** Poll with the `code` from step 1.

```sh
curl -s -X POST "$HUB/api/auth/device/poll" \
     -H "Content-Type: application/json" \
     -d '{"code":"…"}'
```

Before approval this answers `{"pending":true}`; after it, once:

```json
{ "token": "bdt_…", "user": { "id": "u-…", "email": "you@example.com", "name": "You" } }
```

Store the token wherever your runner keeps secrets. It does not expire on a
clock; it stops working when the account signs that device out.

## Write a file

`PUT` the bytes, name the destination in `path`:

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
     -T report.md \
     "$HUB/api/p/$PROJECT/upload/content?path=reports/2026-08-22.md"
```

```json
{ "ok": true, "path": "reports/2026-08-22.md" }
```

Nested paths create the folders. Writing a path that already exists is an
edit, not an error — the previous version stays in History.

Path rules, all of which answer `400` rather than doing something surprising:

- Relative only — no leading `/`, no `..`, no `.` segments.
- No control characters.
- `.bdrive/` and `.git/` are refused at any depth (they are the project's own
  identity and executable hooks — a write there would reconfigure or run code
  on every teammate's machine).
- URL-encode anything spicy in the query string: `path=meeting%20notes.md`.

To send bytes you generated rather than a file on disk, pipe them:

```sh
echo "# Run summary" | curl -fsS -H "Authorization: Bearer $TOKEN" \
     -T - "$HUB/api/p/$PROJECT/upload/content?path=summary.md"
```

## Read a file back

```sh
curl -fsS -H "Authorization: Bearer $TOKEN" \
     "$HUB/api/p/$PROJECT/download?path=reports/2026-08-22.md"
```

The response is the file's current bytes. This is how a sandbox picks up the
team's shared context on the way in — read the docs it needs at the start of
the run, write its output at the end.

## Worked example: a CI job that persists its output

```yaml
# .github/workflows/nightly.yml
- name: Publish the run summary to BearDrive
  env:
    HUB: https://hub.example.com
    PROJECT: ${{ vars.BEARDRIVE_PROJECT }}
    TOKEN: ${{ secrets.BEARDRIVE_TOKEN }}
  run: |
    curl -fsS -H "Authorization: Bearer $TOKEN" \
         -T build/summary.md \
         "$HUB/api/p/$PROJECT/upload/content?path=ci/nightly-$(date +%F).md"
```

The same three lines work from an ephemeral agent sandbox — Claude Code on
the web, E2B, or any container that dies at the end of the run. Nothing is
installed and no device is registered, so there is nothing left behind to
clean up afterwards.

## What the hub records

**The write is not anonymous.** The change is attributed to the account that
owns the token, by name, exactly as if that person had uploaded the file in
the browser. History shows the hub itself as the device — HTTP writes are
journalled by the hub, not by a machine that registered itself — so a row
reads "written by *you*, through the hub", never "written by nobody".

**A token adds no seat.** Seats are counted when an invite adds a member to
an org. A device token belongs to an account that already holds a seat, so
the CI runner, the sandbox and the script are free — however many of them
you mint.

## When it 403s

| Response | Cause |
|---|---|
| `uploads are disabled on this server` | The hub is running without `--upload`. Ask whoever runs it. |
| `you have read-only access to this project` | The token's account can read this project but not write it. Someone with admin on it can raise that in **Project settings → People**. |
| `this source is read-only` | The hub is serving a plain folder rather than a storage root — there is nowhere to journal a write. |

A `401` means the token is wrong, missing, or has been signed out.

## When to use the CLI instead

This door is for machines that write and vanish. For a folder you actually
work in — your laptop, a workstation, a long-lived dev box — install the CLI
and let it sync: you get the files on disk, offline edits, conflict
preservation, and the agent hooks that keep every session's context fresh.
See [Set up with your agent](/start/setup/).
