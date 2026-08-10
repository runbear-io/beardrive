# How this folder is organized

This project is a **shared skill library**. The skills your agent loads live in
`.claude/skills/`, they sync like any other file, and every teammate's agent
picks them up on its next session — no export, no registry, no MCP server per
client. Write a skill once, and the rest of the team's agents know it.

**Sharing what an agent reads is the product; sharing what it runs is not.**
Skills, commands, subagents and `AGENTS.md` sync. An agent's hook
configuration files never do — a hook is a shell command, and syncing one
would install it on your teammate's machine. That line is not a setting; it is
the rule BearDrive enforces.

## The shape

| Path | What it holds |
| -- | -- |
| `.claude/skills/<name>/SKILL.md` | one skill: frontmatter + instructions |
| `.claude/skills/<name>/` | anything that skill needs beside it — scripts, references, examples |
| `AGENTS.md` (this file) | how the library is kept |

Start this project in the folder your agent already starts sessions in. If you
want a library of skills that is not tied to one project, sync
`~/.claude/skills` — **the `skills` directory, never `~/.claude` itself**,
which also holds this machine's private agent state. `bdrive init` refuses the
latter for exactly that reason.

## Where a new skill goes

One directory per skill under `.claude/skills/`, named for the job it does.
Before adding one, read the existing `SKILL.md` files for a skill that already
covers the job: extending one beats adding a near-duplicate beside it, because
two skills that both claim a job is how an agent starts picking the wrong one.

A skill earns its own directory when it has a trigger someone can state in a
sentence. Until then it is a paragraph in this file.

## What a skill file looks like

Every `SKILL.md` opens with YAML frontmatter carrying `name` and
`description`. The description is the only thing an agent reads when deciding
whether to load the skill, so it names both the job and the words a person
would use to ask for it. Everything below the frontmatter is the instruction
the agent follows once it is loaded.

Keep it short enough to be read in full, and concrete: the steps, the commands,
the gotcha that cost someone an afternoon. A skill that restates general
knowledge is one nobody's agent needed.

## When something stops being true

Edit the skill. Do not append a correction under the old text and do not leave
two versions standing — a `SKILL.md` is followed literally, so it has to read
as current instruction from top to bottom.

Nothing is lost by rewriting: BearDrive keeps every version of every file, so
`bdrive log .claude/skills/<name>/SKILL.md` still shows what it said before and
who changed it. Retire a skill by deleting its directory; the History view is
the archive.

## Filenames

- one directory per skill, lowercase, words joined by hyphens:
  `.claude/skills/release-checklist/`
- the instruction file is always `SKILL.md`, capitalized, at the directory root
- name the job, not the tool: `deploy-staging`, never `scripts-v2`
- supporting files sit beside it with plain names: `checklist.md`, `queries.sql`
