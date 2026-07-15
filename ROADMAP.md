# BearDrive roadmap

Last updated: 2026-07-15. This is the direction, not a contract — items
move as we learn. Comments and PRs welcome; items marked **help wanted**
are deliberately scoped for outside contributors.

## Now (next release or two)

- **Time travel / restore** — `bdrive restore <path>@<time>`: every blob
  is already retained forever, so restore is re-putting an old blob as a
  new op. The history UI and blob API already exist; this is the CLI and
  revert UX.
- **Demo assets & docs** — 60-second demo, richer self-hosting guide.

## Next

- **Journal compaction & blob GC policies** — bounded storage for
  long-lived, high-churn volumes (opt-in; today everything is retained).
- **Per-path access scopes** — multi-agent setups where an agent can be
  limited to a subtree of a project. **help wanted** (design discussion
  first — this touches the org/permission model).
- **More agent platforms in `bdrive hooks install`** — the hook engine is
  platform-generic; adding a platform is a small adapter + matcher set.
  **help wanted**.

## Later / exploring

- **FUSE / NFS mount mode** — lazy-loading huge volumes instead of full
  materialization.
- **Search across the hub** — full-text + wikilink graph over a project.
- **Webhooks / notifications** — "a file changed in `reports/`" pushed to
  Slack or similar (hub-side).

## Recently shipped

See [CHANGELOG.md](CHANGELOG.md): React web UI (v0.6.0), read-heat
Insights and agent read attribution (v0.4.0), `bdrive url` internal links
(v0.7.0), SQL metadata backends incl. Postgres/Supabase (v0.3.0).
