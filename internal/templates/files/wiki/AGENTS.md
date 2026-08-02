# How this folder is organized

This project is an **LLM wiki**: you curate sources and ask questions, and the
agent writes and maintains the pages. You rarely write the wiki yourself.

It instantiates the LLM Wiki pattern described by Andrej Karpathy
(<https://gist.github.com/karpathy/442a6bf555914893e9891c11519de94f>) — *one*
instantiation of it, not the canonical one. That document is deliberately
abstract and says the specifics are yours to work out. So change anything here
that does not fit your material; this file is meant to be edited as you learn
what your domain needs.

## The three layers

| Layer | Who writes it |
| -- | -- |
| `sources/` | **you** — clippings, papers, transcripts, notes, exports |
| `wiki/` | **the agent** — every page here is generated and maintained by it |
| `AGENTS.md` (this file) | both of you, over time |

**Nothing in `sources/` is ever edited.** It is the record of what was actually
said, and every claim in the wiki is only worth as much as that record. Add to
it freely; correct it never. Nothing enforces this — BearDrive syncs `sources/`
like any other folder — so it holds only because everyone honors it.

## If there is nothing here yet

Do not build scaffolding before there is material. With no sources there is no
wiki to maintain, and empty category pages are ceremony that will go stale
before anyone reads them.

The first move is a source: put something in `sources/`, then ingest it. The
structure grows out of the material, never ahead of it.

## Where a new page goes

Everything the agent writes goes in `wiki/`. There are no fixed subdirectories —
add one when a category has earned it (a dozen people, a dozen papers), not
before. Until then flat is fine, because `index.md` is the navigation.

Before creating a page, read `index.md` for one that already covers the subject.
Extending a page beats adding a near-duplicate beside it: two pages on one
subject is how a wiki starts contradicting itself.

## The three operations

### Ingest — a new source arrived

1. Read it.
2. Say what you took from it, and stop. The human steers what matters here.
3. Write or update the pages it touches in `wiki/`. One source usually touches
   several: a summary, the entities it names, the concepts it bears on.
4. **Update `index.md` in the same turn.**
5. Append one entry to `log.md`.

**A page write that has not updated the index is an incomplete write.** The
index is what every future session reads *first* to find anything. A page
missing from it is invisible; a wrong line in it is worse than a missing one,
because it will be believed and acted on. If you can only do one thing
properly, do the index.

### Query — a question was asked

Read `index.md`, pick the pages that look relevant, read those, and answer with
links to the ones you used.

Then ask whether the answer is worth keeping. A comparison, a synthesis, a
connection nobody had written down — file it as a page and index it. Answers
left in the conversation are answers you will pay to derive again.

### Lint — periodically, or on request

Walk the wiki and look for:

- pages that contradict each other;
- claims a newer source has superseded;
- orphans — pages nothing links to;
- concepts referred to everywhere with no page of their own;
- gaps worth going and finding a source for.

Report what you find. Fix the mechanical parts yourself — missing links, index
drift, broken names. Ask before rewriting anything that is a judgment call.

## When something stops being true

Revise the page. Do not append a correction underneath the old text and do not
leave both versions standing: a page should read as current truth from top to
bottom, because that is how it will be quoted.

Nothing is lost by rewriting. BearDrive keeps every version of every file, so
`bdrive log <file>` still shows what the page said before and who changed it.
When a source is what changed your mind, cite it in the revision and note it in
`log.md`.

## Filenames

- lowercase, words joined by hyphens, `.md`: `wiki/margin-compression.md`
- name the subject: `wiki/anthropic.md`, never `wiki/notes-3.md` or a bare date
- one subject per page
- link pages to each other with `[[wikilinks]]` — the hub renders them, and they
  are what lets a lint pass find orphans at all

## Every page starts with an H1 and one line

The first line is `# Title`. The line under it is a one-sentence summary of the
whole page — and it is the same sentence that goes in `index.md`. Writing it
once and using it in both places is what keeps the two agreeing.
