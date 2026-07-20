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
https://drive.example.com/p-1a2b3c4d/wiki/report.html
```

Computed locally with no network call, always shows the latest synced content,
and requires the viewer to be signed in and a member of the project's org.

This is the default link an agent should drop into its reply. `--sync` pushes
first, so a just-created file resolves immediately:

```sh
bdrive url --sync wiki/report.html
```

With no argument, `bdrive url` gives the project home.

:::tip[Agents do this automatically]
The sync hook installed by `bdrive hooks install` injects the project's
gated-link formula into the agent's context, so a connected agent appends
`path` [🔗](link) to every synced path it mentions — without being asked. See
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
for living reports and wiki pages, and live until revoked.

```sh
bdrive share --list                    # every link you've minted
bdrive share --revoke <token-or-url>   # kill one
bdrive share --expires 24h <file>      # self-destructing link
```

The web UI has a Share button on every file.

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
Don't put secrets in a synced folder. Note also that a LAN-bound hub means
LAN-only links.
:::

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
past version, including which device (name, OS, and the IP the server observed)
made each change. Folder rows have a history shortcut for a subtree feed.

This is the part a memory API can't give you: when an agent asserts something,
you can see which agent wrote it, when, from where, and what the file said
before.
