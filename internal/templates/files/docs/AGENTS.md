# How this folder is organized

This project uses the **docs + decision records** structure. Everything here
syncs to every teammate and every one of their agents, so the filing rules below
are not a preference — they are what keeps the folder readable for people who
did not write the file.

## Where a new file goes

| What you are writing | Where it goes |
| -- | -- |
| Anything explaining how something works, or how to do something | `docs/` |
| A decision, with its context and its consequences | `decisions/` |
| A quick note that has no home yet | `docs/` — a wrong-but-findable file beats a right-but-invisible one |

Read the directory before you add to it. If a file already covers the topic,
append to that file instead of creating a near-duplicate beside it — two files
saying almost the same thing is the failure mode this structure exists to
prevent.

Never create a new top-level directory. If nothing fits, put it in `docs/` and
say so in the file.

## Decisions

A decision record is written **once, when the decision is made**, and is not
edited afterwards — that is the whole point of the format. Number it with the
next free number: `decisions/0002-....md`. See `decisions/0001-record-decisions.md`
for the shape.

## When something is no longer true

Do not delete it and do not silently rewrite it.

- **A doc that is out of date**: fix it in place, and say what changed at the
  bottom under `## Changes`.
- **A decision that has been reversed**: leave the old record alone, write a new
  one, and add one line to the old one — `Superseded by 0007-....md`. The reason
  a decision was reversed is usually more useful than the decision itself.
- **A doc for something that no longer exists**: delete the file. There is no
  archive directory here on purpose — BearDrive keeps every version of every
  file forever, so `bdrive log <file>` is the archive, and a folder of dead docs
  is just noise for the next person searching.

## Filenames

- lowercase, words joined by hyphens, `.md`: `docs/deploy-to-staging.md`
- name the subject, not the format: `docs/auth.md`, never `docs/notes.md`,
  `docs/new.md`, `docs/misc.md`, or a date-stamped file
- decisions are `NNNN-imperative-phrase.md`: `decisions/0002-use-postgres.md`
- one topic per file; if a file grows past a screen or two of scrolling, split it
  and link the parts

## Every file starts with an H1

The first line is `# Title`, and the paragraph under it says who the file is for
and why they would open it. Both the search box and an agent reading in a hurry
use that line.
