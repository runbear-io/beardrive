# Launch plan (drafts — nothing here is posted; posting is Snow's call)

Two launches with different goals, and the difference matters:

- **Show HN (stage 1, done first)** — the OSS project. The goal is strangers
  who care: stars, issues, first outside users. Not signups, not revenue.
- **Product Hunt (stage 2)** — **BearDrive Cloud**, the managed service, run
  as a standalone self-serve business. Cloud is live, signup is open with no
  credit card, and pricing is public. Revenue signal is the point here, which
  is exactly why the OSS framing above must not leak into it.

## Show HN (primary)

**Title (pick one):**
1. `Show HN: BearDrive – open-source Google Drive for AI agents`
2. `Show HN: A shared folder where your team's AI agents read and write — with attribution`
3. `Show HN: BearDrive – give every agent on your team the same folder as memory`

**Body draft:**

> BearDrive mounts any folder as a synced volume for a team *and their
> AI agents*: files sync through a self-hostable hub in seconds, every
> change is attributed to the human/agent/device that made it, and the
> hub shows what your agents actually read.
>
> Why we built it: our agents kept re-deriving context that a teammate's
> agent had already figured out. Memory APIs felt wrong — we wanted
> real files on disk (agents are great at files), with provenance. So:
> append-only per-device journals, last-writer-wins replay,
> content-addressed blobs (all history retained), offline-first, one Go
> binary for CLI + daemon + hub. AGPL; a managed cloud will fund it.
>
> The part we like most: read telemetry. Agent hooks report which files
> agents consume, so the Insights view shows "hot but stale" knowledge —
> docs everyone's agents rely on that no one maintains.
>
> Happy to answer anything about the sync design (no locks — no object
> ever has two writers), the AGPL choice, or the agent hooks.

**First-comment (founder voice) draft:** technical deep-dive offer —
"the whole concurrency story is that no object has two writers: each
device appends to its own journal; replay is deterministic," + link to
CLAUDE.md's invariants. Answer every comment for the first 3 hours.

**Prep checklist:** install path verified from a clean machine
(`brew install runbear-io/tap/beardrive && bdrive version` — a 404 tap on
launch day is fatal), full first-run verified against a working hub (handoff #6), demo GIF live in README (handoff #8), Discussions
enabled, `docs/self-hosting.md` linked from README, hub demo instance
warm (expect self-host attempts within minutes).

## Product Hunt (after HN, separate day)

**This launch is BearDrive Cloud, not the OSS project**, run as a standalone
self-serve business rather than top-of-funnel for anything else. Cloud is
live, signup is open with no credit card, and pricing is public — see
`/pricing`. There is no waitlist and there will not be one.

- **Tagline:** "The open-source Google Drive for AI agents"
- **Description:** One folder your whole team and their agents share —
  synced in seconds, every change attributed, read analytics included.
  Start free, or self-host in one Go binary.
- **First comment:** the HN body, warmer tone, plus the 60s demo video.
- **Lead with the Insights screenshot** (the reads × staleness quadrant), not
  the sync. Sync is not defensible on a launch page; nobody else ships
  read-heat attribution per agent, and that is the image that wins the thread.

**Prepared answer for "$19 for file sync?"** — that question is a positioning
failure, not a pricing one, so it gets answered from a script, not composed
live: Dropbox syncs files for humans and has no idea which agent read what.
BearDrive is the shared folder your team's agents work in, with per-agent
attribution, full version history, and read-heat analytics showing which docs
your agents actually depend on and which of those are quietly rotting.
Different job, different buyer.

**Day-one paid conversion will look bad, and that is not a signal about
price.** Product Hunt sends individuals, not teams; they sign up solo, stay on
Free, and never reach the 3-seat wall. The number that matters is second-seat
rate — of accounts created in launch week, the fraction that invite at least
one person within 14 days. Above ~15% means the team thesis holds. Near zero
means people are treating BearDrive as a personal sync tool and the wedge
needs rethinking before any tier optimization. Instrumented server-side in the
cloud repo (`internal/analytics`): `invite_sent` and `invite_accepted` are
tracked as distinct events.

## Launch-Week cadence (one artifact/day)

1. **Sync core** — the journal/replay design post ("no locks, no two
   writers").
2. **Attribution & history** — every change knows who/what/where; time
   travel is already free (blobs retained).
3. **Share links** — files as public pages; sandboxed HTML.
4. **Insights** — the read×staleness quadrant; what agents actually
   read.
5. **Agent onboarding** — one paste that points any agent at
   `INSTALL_FOR_AGENTS.md`; `bdrive url` links in agent replies. (There is
   no Claude Code plugin and no bundled skill — the integration is
   `internal/agenthooks` plus that runbook.)

Each day: a blog-able writeup + a tweet-length version + a repo artifact
(doc or demo). Drafts to be written per-day when scheduled.

## Not part of launch

Enterprise self-serve (Enterprise is contact-sales), paid promotion, and any
claim of traction we don't have.

## No "beta" label

No "beta", "early access", or "preview" badge on the product, the site, or
the launch copy. A beta label is a discount that cannot be taken back
cleanly, it repels the team-lead buyer who will not put company context into
a beta tool, and it suppresses the willingness-to-pay signal this launch
exists to produce. "Early but serious" is signalled by the version number,
the public CHANGELOG, and the public ROADMAP, which already exist.
