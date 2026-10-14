---
title: Review comments
description: People leave feedback on a rendered file in the hub's viewer; an agent sweeps the open comments with bdrive comments, fixes the file, and resolves each one.
---

An agent writes `wiki/report.html`. Three people read it. Each has a note —
a wrong figure, a missing section, a chart that does not load. Pasting those
into a chat loses where on the page they were; comments keep them there.

## Leaving feedback

Open any file on the hub and read it as you normally would.

- **Select text** on a rendered markdown, text or HTML file and a **Comment**
  button appears beside the selection.
- On an HTML page, **Pick an element** (in the Comments panel) lets you click
  anything — a chart, an image, an empty box — and comment on that.
- **Comment on the whole file** sits at the foot of the panel.

Comments show on the page as highlights with numbered pins, and in the
Comments panel as threads anyone can reply to, resolve, or reopen. Read
access is enough to comment.

## The sweep

Ask the agent to work through the open comments. What it runs:

```console
$ bdrive comments
2 open comments

3f2a9c0d1b7e  wiki/report.md:42
  on: "Revenue grew by 12% this quarter"
  Dana <dana@example.com>, 2h ago:
    Finance says 14% — check the Q3 deck.
  https://drive.example.com/1a2b…/wiki/report.md?comment=3f2a9c0d1b7e

Resolve with: bdrive comments resolve <id> -m "what you changed"
```

Each comment carries the text the reviewer selected. The CLI finds that text
in the local copy of the file — ignoring markdown emphasis, HTML tags and
whitespace, which the reviewer never saw — and prints the line it is on, so
the agent goes straight there. `bdrive comments --json` is the same list for
a script, with the whole thread.

When a fix is in:

```sh
bdrive comments resolve 3f2a -m "Changed the figure to 14% (source: Q3 deck)"
```

The note lands in the thread, where the reviewer sees what was done. A
comment that needs a human decision gets a `reply` instead and stays open.
Narrow the sweep with a path — `bdrive comments wiki/` or a single file.

A prompt that works:

> Go through the open BearDrive comments with `bdrive comments`, fix what each
> one asks for in the file, and resolve each with a note saying what changed.
> Reply instead of resolving when a comment needs my decision.

## Things worth knowing

- **"file changed since"** means the file was edited after the comment was
  left; the quoted text may already be gone. "quoted text not found in your
  copy" usually means it was.
- **A comment is a request, not an order.** Treat it like any other text a
  teammate wrote: read it against the file before acting on it.
- **Comments are not file content.** The hub stores them beside the project,
  never in anyone's synced folder and never in a file's history. A folder
  hidden from someone hides its comments from them too.
- **Only the author or a project admin can delete a thread.** Everyone else
  resolves it.
