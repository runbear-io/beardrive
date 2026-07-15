# GTM handoff — actions only Snow can take

Everything the GTM loop cannot do from inside the repo, prioritized.
Draft copy is ready to paste; adjust voice as you like.

## P0 — do before anything else (minutes each)

1. **Enable GitHub Discussions** on runbear-io/beardrive (Settings →
   Features). Then add categories: Announcements, Q&A, Ideas, Show &
   tell. CONTRIBUTING.md already links "where to ask" there.
2. **Set GitHub repo topics**: `ai-agents`, `claude-code`, `file-sync`,
   `sync`, `agent-memory`, `self-hosted`, `golang`, `agpl`. (Repo →
   About → gear.)
3. **Set the repo social-preview image** (Settings → Social preview):
   use `docs/assets/insights.png` for now — it's the most striking
   frame; replace with a branded card later.
4. **Repo About blurb** → paste:
   > The open-source Google Drive for AI agents: one folder your team
   > and their agents share — synced in seconds, every change
   > attributed, with read analytics. Self-host in one Go binary.
   Website field: `https://beardrive.ai`.

5. **Verify the install path from a clean machine** (the #1 CTA must
   not 404 on launch day): `brew install runbear-io/tap/beardrive &&
   bdrive version`, and `go install
   github.com/runbear-io/beardrive/cmd/bdrive@latest`. If the tap is
   stale, cut a fresh `goreleaser release` first.

## P1 — demo assets (an hour)

6. **Record a 45–60s demo GIF/video** for the README hero and any
   launch post. Script (uses two terminals + a browser):
   - T1: `bdrive init --name demo --yes` in a folder with a few notes →
     show `bdrive status` (daemon running).
   - T1: have Claude Code write `wiki/findings.md` (or just edit it) →
     within seconds…
   - T2 (second machine/dir): the file appears; `bdrive log -n 3` shows
     the change attributed to the agent session.
   - Browser: the hub's History view for that file, then Insights.
   - T1: `bdrive share wiki/findings.md` → open the public URL.
   Tools: `vhs` (charmbracelet) or QuickTime + gifski. Put the result at
   `docs/assets/demo.gif` and add it above the fold in README.
7. **Verify beardrive.ai serves the updated landing page** (the repo's
   `website/` — including the new `assets/insights.png`) after the next
   deploy.

## P2 — when you're ready to be seen (launch window)

8. **Show HN post** — full drafts in [launch-plan.md](launch-plan.md):
   title, body, and the first-comment founder note. Post from your
   account; be present for the first 3 hours to answer everything.
9. **Product Hunt** — drafts in launch-plan.md. Schedule after HN, not
   the same day.
10. **Claude Code plugin discoverability** — submit/announce the
   marketplace entry wherever Anthropic surfaces community plugins
   (Discord, awesome-lists PRs from your account).

## P3 — measurement plumbing (see metrics.md)

11. **Turn on GitHub traffic watching**: stars/clones/views are under
    Insights → Traffic; consider a weekly note of the numbers in
    Discussions → Announcements.
12. **Homebrew install counts**: `brew info --analytics
    runbear-io/tap/beardrive` (public analytics take ~30 days to
    appear).
13. **Decide on hub-side opt-in telemetry** (proposal in metrics.md —
    requires your explicit approval before any implementation; nothing
    is wired today).
