---
title: Shared agent memory
description: Orient agents in a synced folder with the two-file AGENTS.md pattern, hand the next session your working state, and decide what belongs in shared memory.
---

A newly mounted shared folder is hundreds of opaque files to an agent. It won't
know what lives where, where to *write*, or that the folder exists at all unless
something tells it.

Two files fix that. They have different jobs, and you need both.

## 1. The folder's own map

**`<shared>/AGENTS.md` — synced, team-wide.**

The single source of truth for conventions: what each area is for, naming
patterns, where agents should write (reports → `reports/`), what not to touch.

Because the folder syncs, the map travels with it. Every member on every
platform gets it, and hub history tracks who changed the rules.

```markdown
# Team knowledge folder

- `reports/` — generated reports. Agents write here. One file per topic,
  kebab-case, dated in frontmatter.
- `decisions/` — architecture decision records. Append only; never rewrite
  an existing ADR.
- `runbooks/` — operational procedures. Keep them executable.
- `inbox/` — unsorted. Fine to read, don't build on it.

No secrets in this folder — anything here can be shared as a public link.
```

Keep it under a screen. It is scaffolded **once, by the project creator** —
joiners read it and follow it. A new member's agent must not rewrite team
conventions on day one.

## 2. A root pointer

**Per machine, never synced.**

For a synced subfolder inside a repository, append two or three lines to the
repo root's `AGENTS.md` and/or `CLAUDE.md` (both, if both exist):

```markdown
## Shared knowledge folder

`wiki/` is synced across the team via BearDrive. Read `wiki/AGENTS.md` before
working there. Put shareable artifacts there so teammates' agents see them.
No secrets.
```

Point *at* the synced map. Don't duplicate its conventions, or the copy goes
stale.

### Why the pointer isn't optional politeness

Platform discovery differs:

| Platform | Finds `<shared>/AGENTS.md` on its own? |
|---|---|
| Claude Code / Cowork | Lazily — loaded when a file in that subtree is first read |
| Hermes | Lazily — progressive discovery; walks up from files it touches |
| Codex | **Never** — only loads `AGENTS.md` along the root→cwd path |

And even where lazy loading works, it fires only *after* the agent decides to
enter the folder. Only the root pointer gives it the awareness to go there in
the first place — "save the report where the team sees it."

:::note[Standalone knowledge folders]
A dedicated folder with no enclosing repository needs only the synced
`AGENTS.md`. At the mount root, every platform loads it at session start.
:::

Ask your agent to write them and it will offer each as its own consent. Never
write either silently into someone's repository.

## The orientation ritual

Worth teaching your agents as a habit: on first contact with a synced folder,
read its `AGENTS.md` before substantive work.

If there is none, orient from the file tree plus recent activity —

```sh
bdrive log wiki    # which areas are alive
```

— and, if this device created the project, offer to draft `AGENTS.md` for the
team.

## Your agent is told what moved

Six people's agents in one folder means an agent can start a turn holding a
copy of a file a teammate's agent rewrote an hour ago. So it is told: at turn
start, the sync hook names the files that arrived from teammates since the
last turn — "changed since your last turn, re-read before editing" — with
deletions marked.

It is advisory. Nothing blocks, nothing prompts, no write is refused; the
agent gets the fact and decides. That needs the turn-start hooks `bdrive init`
registers ([Hooks in detail](/manual/hooks/)) — without them nothing drains
the list, and the agent hears nothing.

The list is capped, so the first turn after joining a project names some of
what arrived rather than the whole project.

## Hand the next session your working state

The list above says what *changed*. It doesn't say what the last session was in
the *middle of* — and that is what every new session, on every machine, for
every teammate, otherwise re-derives from scratch.

`AGENT_HANDOFF.md` at the project root is that channel. On the first turn of
each agent session the sync hook hands the agent the file's body (up to 4 KB),
with the date and account that last changed it. Every turn, it asks the agent
to overwrite the file with what the next session needs to pick the work up.

So the ritual is one sentence, and mostly you don't even say it:

> Before you finish, write what you'd want to know if you picked this up
> tomorrow into `AGENT_HANDOFF.md`.

The next session may be tomorrow, on your laptop instead of your desktop, in a
teammate's account, or on a different agent platform. Nothing is live at either
end — the file syncs, versions and attributes like every other file, and works
when the other person is asleep.

Two caveats:

- The body arrives as **information, not instructions**: it is another
  session's note, and the hook frames it that way. Nothing in it can order your
  agent around.
- If the project's scope excludes the root (`bdrive init --only wiki`), the
  handoff is local only and the hook tells the agent so. `bdrive scope add`
  shares it — see [Scoping the folder](/guides/scoping/).

## What belongs in shared memory

Good candidates are the things that are expensive to rediscover and cheap to
write down:

- **Findings** — what an investigation concluded, and what was ruled out.
- **Decisions** — what was chosen and why, so the next agent doesn't relitigate.
- **Runbooks** — procedures that worked.
- **Artifacts** — reports, analyses, and generated documents teammates need to
  read.

What doesn't belong: secrets and credentials (any member can mint a public link
for any file), anything derivable by reading the code, and transcript-level
detail nobody will ever read again.

See [Scoping the folder](/guides/scoping/) for keeping build output and
dependencies out mechanically.

## Session notes

Stamp changes with the context that produced them:

```sh
bdrive sync --note "session 8f21: auth refactor investigation"
```

The note shows up in `bdrive log` and hub history, and keeps applying to
daemon-committed changes until `--note-ttl` (default 30m) expires. This is how a
change traces back to the conversation that caused it.
