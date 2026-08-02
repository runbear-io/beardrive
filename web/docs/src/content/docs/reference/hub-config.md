---
title: Hub config
description: Every bdrive serve flag and config-file key.
---

`bdrive serve` serves a website — browse folders and files, read markdown rendered
Obsidian-style (including `[[wikilinks]]`, task lists, and tables), download any
file. Pointed at a storage root, it becomes a multi-project sync hub.

It is read-only unless started with `--upload`.

```sh
bdrive serve                              # serve the current directory (viewer)
bdrive serve ./notes                      # serve a folder from disk (viewer)
bdrive serve -c config.json               # everything from a config file
bdrive serve s3://my-bucket/root --upload # multi-project sync hub
```

With a folder it serves files straight from disk. On a BearDrive mount the
daemon keeps them fresh, which makes this the simplest read-only deployment — no
cloud credentials on the serving machine.

## Flags

| Flag | Default | Effect |
|---|---|---|
| `--addr` | `:4173` | Listen address |
| `--volume` | | Display name |
| `--refresh` | `10s` | Listing cache |
| `--dir` / `--remote` | | Explicit forms of the positional argument |
| `--upload` | off | Allow client writes |
| `--upload-ttl` | `15m` | Presigned-URL lifetime |
| `--projects-db` | `$BDRIVE_HOME/projects.json` | Hub project registry file |
| `-c` / `--config` | | Read all of the above from a JSON file; explicit flags win |

## Config file

```jsonc
// bdrive serve -c config.json
{
  "remote": "s3://my-bucket/root",   // storage root (hub) — or "dir": "./folder" (viewer)
  "addr": ":4173",
  "upload": true,
  "upload_ttl": "15m",
  "refresh": "10s",
  "projects_db": "/var/lib/bdrive/projects.json",
  "share_rpm": 120,                  // per-IP rate limit on public /s/* links
  "trust_proxy": false,              // true only when a reverse proxy fronts the hub
  "auth": {                          // optional knobs; hub auth is always on
    // Signup is invite-only by default. To allow self-service signup,
    // open it WITH a gate (an ungated open hub is refused at startup):
    "allow_signup": true,
    "allowed_domains": ["example.com"],  // only these domains may sign up
    "require_approval": true,            // …and an admin must approve each one
    "base_url": "https://drive.example.com",  // public origin for MAILED links (reset, verification)
    //   Required whenever smtp is set (the hub refuses to start without it):
    //   a mailed link must never be built from a requester's Host header.
    "users_db": "/var/lib/bdrive/auth.json",
    "admins": ["admin@example.com"],
    "smtp": { "host": "smtp.example.com", "port": 587,
              "user": "drive@example.com", "pass": "…", "from": "drive@example.com" }
  },
  "reads": {                         // read heatmap telemetry (hub mode)
    "enabled": true,                 // default true; aggregate counts only
    "retention_days": 400            // daily buckets older than this fold into all-time totals
  },
  "database": { "driver": "sqlite", "dsn": "/var/lib/bdrive/hub.db" }
}
```

See [Authentication](/self-hosting/authentication/) for the `auth` block and
[Database](/self-hosting/database/) for `database`.

## Running behind a reverse proxy

`trust_proxy` (default `false`) decides where the hub reads a caller's IP
address from. It keys two rate limiters: the one on public share links
(`share_rpm`) and the one on `POST /auth/login`, `/auth/signup` and
`/auth/reset` that blunts password brute-force and account enumeration.

- **Default (`false`)** — the address is the connection's own. Correct for a
  hub clients reach directly.
- **`true`** — the **last** `X-Forwarded-For` entry is used instead: the header
  grows left to right, each proxy appending what it saw, so the last entry is
  the one your proxy added and everything before it is whatever the client
  chose to send. Set this only when a proxy you control (nginx, Caddy, a cloud
  load balancer) sits in front of the hub.

Leaving it `false` behind a proxy costs you accuracy: every request looks like
it comes from the proxy, so the limiters throttle all your users as one. Turning
it `true` on a directly-reachable hub costs you the limiters entirely — any
caller can pick a fresh address per request and never be throttled.

## Uploads

The browser client is deliberately storage-blind: it never sees the remote URL,
the bucket, or any credentials. On page load it fetches `/api/config` and
follows whatever the server allows.

With `--upload` set, the server decides per upload how the bytes travel:

- **Direct** — for backends that can presign (S3 and S3-compatible stores; GCS
  when the server runs with credentials that can sign, such as a service
  account). The server mints a short-lived presigned `PUT` URL for the
  content-addressed blob, the browser uploads straight to the object store, then
  asks the server to commit. The commit verifies the blob exists and appends a
  `put` op to the server's own journal.

  Direct uploads to a bucket also need a CORS rule on the bucket allowing `PUT`
  from the viewer's origin. Expired URLs are refused by the store; the client
  just re-runs init.

- **Through the server** — `file://` remotes and plain-folder serving can't
  presign, so the client sends content to the server, which stores it.

## Device sync through the hub

The `https://` remote speaks the hub's per-project `/api/p/<id>/store` API.
Journal reads and writes relay through the server; blob uploads go direct to the
object store via the same short-lived presigned URLs browser uploads use,
falling back to relaying when the backend can't presign. Journals are never
presigned — only immutable blobs.

Client pushes and project creation require the server to run with `--upload`.
Against a read-only hub, clients still pull, and `bdrive status` says
`access: read-only (pull only)` rather than reporting a phantom outage.

Per-project permissions gate the same API: `read` admits `store/list`,
`store/object`, and `store/exists` — everything a pull needs — while
`PUT store/object` and `store/sign` need `write`. That is what makes a
read-only teammate's device pull-only instead of stuck. See
[Project permissions](/concepts/permissions/).
