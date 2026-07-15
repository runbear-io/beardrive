# Spec: GTM readiness for BearDrive (reviewer-satisfied goal loop)

Authoritative spec for the GTM improvement loop. The bar is met by
independent review: a `beardrive-gtm` subagent (COSS operator persona)
audits the repo's go-to-market surfaces and scores them; the implementing
agent never scores its own work.

## Scope — what the loop may change

Everything GTM-relevant that lives in this repo:

- `README.md` (positioning, quickstart, time-to-wow, demo assets plan)
- `website/` (headline, category claim, CTAs, pricing posture copy)
- `docs/` (self-host guide, quickstart, a launch plan, a metrics plan)
- Community surface: `CONTRIBUTING.md`, `.github/` issue/PR templates,
  `CHANGELOG.md` or release-notes convention, a public `ROADMAP.md`
- `plugin/` copy where it is a funnel surface (install flow wording)
- The open↔paid line as *explanation* (docs/website copy about what OSS
  ships vs what beardrive.ai adds)

## Hard guardrails (violating any = the round is void)

1. **Truth only.** Never fabricate social proof: no invented testimonials,
   logos, user counts, star counts, or benchmarks. Every product claim
   must be verifiable against the code/docs. Aspirations are labeled as
   roadmap, not shipped features.
2. **No license change, no pricing decisions.** Pricing/packaging content
   is written as clearly-labeled *proposal* for Snow to approve, not
   published as fact.
3. **No external actions.** No posting, publishing, account creation,
   emailing, or launching anywhere. Anything requiring action outside the
   repo goes to `docs/gtm-handoff.md` — a prioritized checklist for Snow
   (e.g. "post X to HN with this title", "enable GitHub Discussions",
   "record 45s demo of Y") with ready-to-use draft copy where applicable.
4. Keep README ↔ SKILL.md ↔ CLAUDE.md consistent when CLI behavior is
   described; never commit `internal/webapp/manual_serve_test.go`;
   `go build ./... && go test ./...` stays green if anything code-adjacent
   is touched; frontend changes rebuild committed static.
5. Branch `feat/gtm-readiness`, commit per round, PR at the end; never
   merge or deploy.

## Review protocol (per round)

Launch a `beardrive-gtm` subagent. It must score the six dimensions from
its charter (positioning, funnel, open-vs-paid line, community readiness,
launch readiness, metrics) 0–10 **on repo-controllable readiness**: "has
everything within this repo's power been done for this dimension?" —
external-world gaps (no stars yet, no community yet) do NOT cap the score
if the corresponding handoff items exist and are well-prepared. It returns
findings tagged blocker / major / minor, each marked REPO-FIXABLE or
HANDOFF, with concrete fixes (replacement copy written out).

The implementing agent fixes every repo-fixable blocker/major (and minors
where cheap), routes handoff items into `docs/gtm-handoff.md`, and records
the round in the Scorecard below.

## Exit bar

Two CONSECUTIVE review rounds where every dimension scores ≥ 8 and there
are zero repo-fixable blocker or major findings. Disagreements: the
reviewer's judgment wins unless it violates a guardrail (e.g. demands
fabricated proof) — record such rejections under Won't-fix with reasons.

## Scorecard (append one row per round)

| Round | positioning | funnel | open-paid | community | launch | metrics | repo-fixable blockers/majors |
|---|---|---|---|---|---|---|---|
| 1 (baseline) | 6 | 4 | 8 | 3 | 3 | 2 | 3 blockers, 3 majors (all fixed this round; screenshots captured from the live demo hub — real UI, not fabricated) |
| 2 | 8 | 7 | 9 | 8 | 8 | 8 | 1 major (residual dead-deployment-mode copy ×3 on the site — fixed) + 4 minors fixed; share.png re-captured with real rendered content |
| 3 | 9 | 7 | 9 | 8 | 7 | 9 | 1 major (quickstart defaulted into the not-yet-live beardrive.ai — quickstart/closer rewritten to self-host-first, first-run gate added to handoff) + README trimmed 583→524, starter-issues & waitlist handoffs added |
| 4 | 9 | 6 | 9 | 9 | 7 | 9 | 2 majors: bare-login default survived in the README HERO and both plugin commands (+ SKILL walkthrough, website terminal animation) — all four surfaces now self-host-first, fixed this round |
| 5 | 9 | 8 | 9 | 9 | 8 | 9 | 0 — PASS #1 (two optional one-line minors applied after the round) |
| 6 (confirmation) | 9 | 8 | 9 | 9 | 8 | 9 | 0 — PASS #2, EXIT BAR MET (truth-verified against code; two optional minors — og:image, SKILL parity — applied post-exit) |

## Won't-fix / disputed

(none yet)

## Status

GOAL COMPLETE (2026-07-15): rounds 5 and 6 both scored every dimension
≥8 with zero repo-fixable blocker/major findings. Six reviewer rounds
total. The loop's own copy was audited for truth-overreach each round —
rounds 3 and 4 caught the loop routing strangers into the not-yet-live
cloud, which is exactly what independent review is for. Remaining work
is Snow's external checklist: docs/gtm-handoff.md (P0 first).
