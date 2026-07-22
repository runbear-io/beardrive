# Architecture diagrams

Mermaid diagrams of the current implementation, kept next to the code so PRs
can update them alongside the change.

**Convention:** when a PR changes the structure drawn here (new/removed types,
new seams, changed relationships), update the affected diagram in the same PR
and embed the changed diagrams' mermaid blocks in the PR description under an
"Architecture changes" section, so reviewers see the structural delta. A
pre-PR hook (`.claude/hooks/check-arch-diagrams.sh`) reminds Claude Code
sessions when server code changed but no diagram did.

- [webapp-server.md](webapp-server.md) — class diagram of the `bdrive web` server (`internal/webapp` + its `internal/remote` seam)
