# Architecture diagrams

Mermaid diagrams of the current implementation, kept next to the code so PRs
can update them alongside the change.

**Convention:** when a PR changes the structure drawn here (new/removed types,
new seams, changed relationships), update the affected diagram in the same PR
and add an "Architecture changes" section to the PR description that, per
changed diagram:

1. names exactly which types/relationships changed and how (one sentence);
2. shows a **Before** and an **After** mermaid block — each an *excerpt* of
   only the affected classes and their immediate relationships, never the
   full diagram (Before comes from the diagram at the merge base).

The committed diagram file stays the full current state; the before/after
excerpts exist only in the PR description so reviewers see the structural
delta at a glance. A pre-PR hook (`.claude/hooks/check-arch-diagrams.sh`)
reminds Claude Code sessions when server code changed but no diagram did.

Together these cover every application package in the repo — every code
change lands inside exactly one detail diagram's scope (plus the overview
when the package map or cross-piece wiring changes):

- [overview.md](overview.md) — system diagram: every package and surface on one page, and how they connect
- [cli-sync.md](cli-sync.md) — class diagram of the CLI and sync engine (`cmd/bdrive` + `internal/{syncer,store,journal,config,daemon,agenthooks,agentskills}`)
- [webapp-server.md](webapp-server.md) — class diagram of the `bdrive web` server (`internal/webapp` + its `internal/remote` seam)
- [webapp-frontend.md](webapp-frontend.md) — module diagram of the hub's React SPA (`internal/webapp/frontend/src`)

Not covered on purpose: `web/docs` (content site, no application code) and
`cloud/` (private nested repo — its architecture lives there).
