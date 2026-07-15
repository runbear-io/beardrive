# Metrics plan

What we measure to know whether BearDrive is working as a project and a
product. Stage-aware: today (a16z stage 1, project-community fit) the
numbers that matter are attention and activation, not revenue.

## North star

> **Active shared brains**: projects with ≥2 members where an agent both
> wrote and read a file in the last 7 days.

This is the promise — "your agent knows what their agent knows" —
actually happening: multiple people, agents on both sides of the sync,
within a week. Everything else is upstream of it.

## Funnel metrics (in order)

| # | Metric | Source | Stage |
|---|---|---|---|
| 1 | README/landing views → GitHub stars | GitHub Insights → Traffic (handoff #13) | attention |
| 2 | Installs: brew + `go install` | brew analytics (handoff #14); go proxy stats are noisy — treat as directional | acquisition |
| 3 | Activation: `bdrive init` → first successful sync | today: anecdotal/self-reported; future: opt-in telemetry (below) | activation |
| 4 | Team formation: project gains a 2nd member (invite redeemed) | hub data (self-hosted: invisible to us; beardrive.ai once live) | expansion |
| 5 | Wedge proof: first agent read-telemetry event in a project | hub read ledger (same visibility caveat) | wedge |

## What we can already see without any new code

- GitHub: stars, forks, issues, Discussions activity, traffic.
- Homebrew analytics (public, aggregated).
- beardrive.ai hub metrics once Cloud is live: metrics 3–5 fall out of
  data the hub already stores (device registry, org invites, read
  ledger) — measurement is a query, not instrumentation.

## Proposal (not implemented — needs Snow's approval)

Opt-in, anonymous CLI telemetry (a single `bdrive init` success ping with
version + OS, no paths, no identifiers beyond a random install id,
disabled by default with `BDRIVE_TELEMETRY=1` to enable). This is the
only way metric #3 becomes visible for self-hosted users. **Deliberately
a proposal**: shipping any telemetry, even opt-in, changes the trust
posture of an OSS tool and is Snow's call, not the GTM loop's.
