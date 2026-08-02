# How this folder is organized

This project uses **PARA**: four top-level directories, sorted by how
*actionable* something is rather than by what topic it belongs to. Everything
here syncs to every teammate and every one of their agents, so the filing rules
below are what keep the folder usable by someone who did not write the file.

## Where a new file goes

Ask one question — *what is this for?* — and take the first row that fits:

| The file is about… | It goes in |
| -- | -- |
| Something with a finish line and a deadline | `projects/` |
| A responsibility with no finish line — a system, a team, a customer you keep tending | `areas/` |
| A topic you are collecting material on, useful but not owned | `resources/` |
| Something already finished, dropped, or superseded | `archives/` |

The ordering matters: a thing with a deadline is a project even if it is also a
topic. When two rows both fit, take the higher one.

One directory per project or area, named after the thing, with a `README.md`
inside stating its goal and its current state. Notes live inside that directory,
not loose at the top level.

Never create a fifth top-level directory. The four are the whole structure; if
something does not fit, it belongs in `resources/`.

## When something is archived

Archiving is an explicit move, and it is the habit that keeps PARA from becoming
a pile. Move the whole directory into `archives/`, unchanged, when:

- a project is shipped, cancelled, or has not been touched in about a month and
  nobody can say what the next step is;
- an area is no longer anyone's responsibility;
- a resource has been superseded by a better one — move it, do not delete it.

Add one line at the top of its `README.md` saying what happened and when
(`Archived 2026-07-30 — shipped in v2.1.`). Nothing is deleted: `archives/` is
searchable, and it is what makes "we tried this before" answerable. Past work
that is still being cited is not archived — pull it back out into `resources/`
instead of copying it.

Un-archiving is the same move backwards, and is completely fine.

## Filenames

- lowercase, words joined by hyphens, `.md`: `projects/q3-launch/press-plan.md`
- name the subject, not the format: never `notes.md`, `new.md`, `misc.md`, or a
  bare date
- every project and area directory has a `README.md`; that is the file someone
  opens first
- one topic per file — if a file grows past a screen or two of scrolling, split
  it and link the parts

## Every file starts with an H1

The first line is `# Title`, and the paragraph under it says who the file is for
and why they would open it. Both the search box and an agent reading in a hurry
use that line.
