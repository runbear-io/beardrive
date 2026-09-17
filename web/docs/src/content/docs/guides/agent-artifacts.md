---
title: Artifacts and links
description: When an agent creates something in the shared folder, it should reply with a URL — internal for teammates, public for everyone else.
---

An agent that writes `wiki/report.html` and says "I've written the report" has
done half the job. The other half is the link.

BearDrive gives every synced file two of them: an **internal link** gated by
sign-in and org membership, and a **public link** that needs no account.

## Internal links, for teammates

```console
$ bdrive url wiki/report.html
https://drive.example.com/1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d/wiki/report.html
```

Computed locally with no network call, always shows the latest synced content,
and requires the viewer to be signed in and a member of the project's org.

This is the default link an agent should drop into its reply. `--sync` pushes
first, so a just-created file resolves immediately:

```sh
bdrive url --sync wiki/report.html
```

With no argument, `bdrive url` gives the project home.

If someone later renames the file or drags it into a folder, the old URL keeps
working: the hub pairs the rename's two halves in the journal and redirects to
the file's new home, saying "Moved from …" above the content. A **live** path
always wins, though — if a brand-new file has since taken the old address, that
new file is what the URL serves, with no redirect.

:::tip[Agents do this automatically]
The sync hook `bdrive init` registers injects the project's
gated-link formula into the agent's context, so a connected agent appends
`path` [🔗](link) to every synced path it mentions — without being asked. If a
session's root holds several connected folders, each one's URL goes in, so a
path is always linked to the project it actually lives in. See
[Set up with your agent](/start/setup/).
:::

## Public links, for everyone else

For people outside the hub — a client, a candidate, someone in another company —
share the file publicly. Hand them the URL and they see it, no account needed.

```console
$ bdrive share wiki/report.html
https://drive.example.com/s/eacc1df3ee6a6ebbdacc535c2796dc30
```

Links serve the file's **latest** synced content, which is the right behavior
for living reports and wiki pages, and live until they expire or you revoke them.

A share link points at one **file**, not at an address — the opposite of an
internal link. Move or rename the file and the link follows it, even if a new
file later takes the old path. Delete the file and the link 404s, and stays
404 forever: nothing that later appears at that path is ever served through a
link minted for something else.

```sh
bdrive share --list                    # every link you've minted
bdrive share --revoke <token-or-url>   # kill one
bdrive share --expires 24h <file>      # self-destructing link
```

The web UI has a Share button on every file. Its dialog also carries an
**Expires** selector — Never, 24 hours, 7 days, 30 days — which re-dates the
link it just minted. The token and URL don't change, so an expiry you add after
copying the link doesn't invalidate what you pasted.

This is what makes "write a report and share it" a single request: the agent
generates `wiki/report.html`, the hook pushes it, and the reply comes back with
a public URL already in it.

## How shared files render

- **HTML** renders as a real page — which is why generated reports are worth
  emitting as HTML.
- **Markdown** renders like the viewer, with a small "Shared with BearDrive"
  footer. Raw HTML inside it is served byte-for-byte and never injected into.
- **PDFs** open inline.

Rendering is sandboxed: `/s/*` responses carry a strict CSP, never see auth
cookies, and sit behind a per-IP rate limit, so a malicious shared file's
scripts can't touch hub sessions and a scraper can't turn your hub into a CDN.

:::caution
Any org member can mint links, and a link is public to whoever holds the URL.
Before minting, the hub scans the first 1 MiB of the file for
credential-shaped strings and refuses (`--force`, or **Share anyway** in the
UI, overrides it) — but that check runs *at the moment you share*, and a link
serves the file's latest content forever, so don't put secrets in a synced
folder. Note also that a LAN-bound hub means LAN-only links.
:::

## Fixing the wording without regenerating the report

An agent writes a good report with one wrong number, or a sentence that reads
badly. Re-prompting the agent to regenerate the whole file to fix six words is
the slow answer, and it churns every other line in the process.

Open the file in the web UI and press **Edit**. The page stays the page — its
own CSS, its own layout — and the text becomes clickable. Click a paragraph,
type. `Cmd/Ctrl+B` and `Cmd/Ctrl+I` do bold and italic; `Cmd/Ctrl+Z` undoes.
It saves as you go — pressing **Done** straight after typing keeps what you
typed — and teammates editing the same file at the same time see each other's
changes. A rendered page also refreshes itself when the file changes, so a
teammate's edit appears while you are looking at it.

**The address bar follows you into the editor**, from
`.../<project>/report.html` to `.../<project>/edit/report.html`. Copy it and
send it to someone: they land in the same live document rather than on the
read-only page, so "come and fix this paragraph with me" is one link. It is
still a gated hub URL — sign-in and write access on that folder — so the
invitation only works for people who could have edited the file anyway.
Reloading, and Back, do what you expect for the same reason.

The point is what happens to the file. Only the paragraph you edited is
rewritten: your indentation, comments, `<script>` and `<style>` blocks come
back byte-for-byte, so the next agent to read the file finds what it wrote and
the History diff is one line instead of a whole-file reformat.

Some things it deliberately won't do:

- **No structural edits.** Enter doesn't split a paragraph and Backspace won't
  merge two. Adding a section, deleting one, or repairing a tag is what
  **Edit source** is for — the same file, its markup, in a normal text editor.
- **Occasionally a paragraph won't open.** Inline markup the editor doesn't
  model — a styled `<span>`, a link, `<sup>` — is carried through untouched,
  so those paragraphs edit normally and the markup comes back byte-for-byte.
  A few things still can't be represented (a `<br>` inside a paragraph, for
  one), and those stay read-only rather than being quietly flattened. Hovering
  shows a not-allowed cursor; use **Edit source** for them.
- **Text a page's own JavaScript generates isn't editable**, because it isn't
  in the file. A chart's labels are drawn at runtime; there is nothing to
  click and nothing to write back.

Editing needs write permission on the file, and respects
[folder permissions](/concepts/permissions/) — a read-only folder shows no Edit
button rather than one that fails on save. Shared `/s/` links are never
editable.

## Who wrote what

Every change is attributed to the account, agent, and device behind it, and
content is content-addressed and retained forever — so every version stays
viewable.

```sh
bdrive log                    # recent changes across the project
bdrive log -p wiki/report.md  # one file's history
bdrive log -n 50              # more of them
```

The web UI's **History** view shows the same thing with view and download of any
past version, including which device (name and OS) made each change. Folder rows have a history shortcut for a subtree feed.

This is the part a memory API can't give you: when an agent asserts something,
you can see which agent wrote it, when, from where, and what the file said
before.
