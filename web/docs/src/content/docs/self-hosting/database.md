---
title: Database
description: Where a hub keeps its metadata — file, SQLite, or Postgres — and how to choose.
---

A hub keeps a little **metadata** separate from your files: accounts, projects,
orgs, invites, shares, devices, and read buckets.

File content and the sync journals always live in the object store. The database
never holds them.

## The three drivers

```jsonc
"database": { "driver": "file" }                       // default — JSON under BDRIVE_HOME
"database": { "driver": "sqlite",   "dsn": "/var/lib/bdrive/hub.db" }
"database": { "driver": "postgres", "dsn": "postgres://…@…pooler.supabase.com:6543/postgres" }
```

- **file** (default) — zero dependencies, human-readable JSON. Right for a
  laptop or a small self-hosted hub.
- **sqlite** — one embedded database file. A real database locally, with no
  server to run.
- **postgres** — a managed Postgres such as **Supabase** for production. Point
  `dsn` at its connection string; use the transaction pooler when you expect
  many connections. Since Supabase *is* Postgres, this stays fully open source
  with no managed-only lock-in.

## Choosing

`file` and `sqlite` are single-writer — run one hub instance. Postgres is
transactional and can back more than one.

Both SQL drivers are pure Go, so the binary stays a CGO-free static build.

:::caution
Switching backends does not migrate existing data. Pick one when you set the hub
up.
:::
