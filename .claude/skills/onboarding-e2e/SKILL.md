---
name: onboarding-e2e
description: "Live end-to-end test of BearDrive's agent-first onboarding: spawn a fresh agent that role-plays a real user conversation ('keep our wiki synced…') against a running hub, following the plugin's SKILL/install instructions verbatim, and report the full transcript plus doc-vs-reality findings. Use when the plugin onboarding copy, the CLI init/login/hooks flow, or hub auth changed and you want proof the conversation still works. Args: [hub-url] [authenticated BDRIVE_HOME] [bdrive-binary]"
---

# Agent-onboarding live E2E

Tests the thing unit tests can't: that a *conversation* driven by the plugin
instructions actually onboards a user end to end against a real hub. The
deliverable is a transcript + findings, not a pass/fail bit — instruction
drift (docs promising what the binary doesn't do) is exactly what this
catches.

## Inputs (ask if not provided)

- `HUB` — a running hub URL (dev default: the local cloud hub).
- `HOME_AUTH` — a `BDRIVE_HOME` directory already signed in to that hub
  (`bdrive login --status` must show an account). Browser signup can't run
  headlessly; if none exists, create a synthetic user first (hub-dependent —
  on a PropelAuth hub: Backend API `POST /api/backend/v1/user/` then a magic
  link, or the device flow approved from an authenticated browser session).
- `BDRIVE` — path to the `bdrive` binary to test (build it from the tree
  under test: `go build -o /tmp/bdrive-e2e ./cmd/bdrive`). Don't trust PATH.

## Procedure

1. **Stage a realistic repo** in a scratch dir (never inside the real repo):
   ```sh
   A=<scratch>/agent-e2e && mkdir -p $A/repo/wiki $A/repo/src $A/plugin
   # 3 markdown pages with [[wikilinks]] in wiki/, a token src/ file,
   # git init + commit (the wiki MUST be git-tracked — the handoff step
   # is part of the test).
   ```
2. **Materialize the instructions under test** — from the branch being
   tested, not the working tree:
   ```sh
   git show <branch>:plugin/commands/install.md  > $A/plugin/install.md
   git show <branch>:plugin/skills/beardrive/SKILL.md > $A/plugin/SKILL.md
   ```
3. **Spawn a fresh agent** (Task/Agent tool, general-purpose) with a prompt
   that makes it role-play a real Claude Code session. The prompt must:
   - confine all writes to the scratch dir;
   - name the two instruction files as its ONLY operating manual and demand
     it follow them faithfully, noting friction instead of papering over it;
   - require `BDRIVE_HOME=$HOME_AUTH` on every bdrive call and forbid bare
     `bdrive login` (no browser available);
   - fix the project name (avoid collisions with earlier runs);
   - open with the exact user message
     `"keep our wiki synced with the team and give me a link to it"`;
   - script the simulated user: consent to the sync + synced AGENTS.md, but
     DECLINE one optional step (e.g. the root pointer) so the transcript
     proves the consent gates are real;
   - require every command's real output in the transcript — no fabrication;
   - end with cleanup: `bdrive stop <repo>` so no daemon lingers;
   - demand a two-part report: **TRANSCRIPT** (User/Claude turns with real
     command output) and **TEST FINDINGS** (numbered: worked-as-written /
     doc-vs-reality gaps with quoted instruction text / first-timer
     confusion / whether the `bdrive url` payoff link served real content —
     verify with an authenticated `curl` against the project API).
4. **Independently verify** the agent's headline claims before relaying:
   the project exists on the hub (`/api/projects` with the token), the file
   content round-trips, the daemon is stopped.
5. **Relay** the transcript verbatim and triage findings into: fix-now doc
   patches, behavior bugs (file/branch them), and cosmetics.

## Pass bar

The conversation must reach the payoff (a working hub link) with no step
where the agent had to contradict the instructions silently. Any place the
agent adapted beyond the written instructions is a finding, even if the run
"worked".

## Known environment quirks

- `command -v bdrive` may find a Homebrew binary that's older than the tree
  under test — always pass `BDRIVE` explicitly and watch for version skew.
- On hubs with PropelAuth + "must be in at least one org": brand-new users
  are gated at PropelAuth's create-org screen unless the `user.created`
  webhook can reach the hub — synthetic-user setup must account for it
  (deliver the webhook by hand or pre-create the org).
