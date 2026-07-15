# Launch plan (drafts — nothing here is posted; posting is Snow's call)

Stage-1 launch: the goal is strangers who care — stars, issues, first
outside users — not signups or revenue. Sequence: quiet HN → learnings →
Product Hunt → Launch-Week-style feature cadence as Cloud approaches.

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

**Prep checklist:** demo GIF live in README (handoff #5), Discussions
enabled, `docs/self-hosting.md` linked from README, hub demo instance
warm (expect self-host attempts within minutes).

## Product Hunt (after HN, separate day)

- **Tagline:** "The open-source Google Drive for AI agents"
- **Description:** One folder your whole team and their agents share —
  synced in seconds, every change attributed, read analytics included.
  Self-host in one Go binary, or join the cloud waitlist.
- **First comment:** the HN body, warmer tone, plus the 60s demo video.

## Launch-Week cadence (when Cloud nears — one artifact/day)

1. **Sync core** — the journal/replay design post ("no locks, no two
   writers").
2. **Attribution & history** — every change knows who/what/where; time
   travel is already free (blobs retained).
3. **Share links** — files as public pages; sandboxed HTML.
4. **Insights** — the read×staleness quadrant; what agents actually
   read.
5. **The Claude Code plugin** — `/beardrive:install`, two-file AGENTS.md
   orientation, `bdrive url` links in agent replies.

Each day: a blog-able writeup + a tweet-length version + a repo artifact
(doc or demo). Drafts to be written per-day when scheduled.

## Not part of launch

Pricing (Cloud is waitlist-only), enterprise pages, paid promotion, and
any claim of traction we don't have.
