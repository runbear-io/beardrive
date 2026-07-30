# 0001 — Record decisions

**Status:** accepted

## Context

Decisions get made in a call, a thread, or an agent session, and the reasoning
evaporates within a week. Six months later someone re-opens the same question,
or — worse — quietly reverses it without knowing what it cost the first time.

## Decision

Anything with a consequence someone could reasonably want to undo gets a file in
this directory. Copy the shape of this one:

```
# NNNN — Imperative phrase

**Status:** accepted | superseded by NNNN

## Context      what was true, and what forced a choice
## Decision     what we are doing, in the present tense
## Consequences what this costs, including what it makes harder
```

Number sequentially, never reuse a number, and write the record when the
decision is made rather than when someone remembers.

(The format has a name — an **architecture decision record**, or ADR — if you
want to read more about it. Nothing here depends on knowing that.)

A record is written once and not edited afterwards. Reversing a decision means a
new record, plus one line here — `Superseded by NNNN-....md` — so the trail stays
readable in both directions.

## Consequences

A small tax on every real decision, and a directory that answers "why is it like
this?" without anyone having to remember. The failure mode to watch for is
recording everything: a decision with no alternative and no cost is just a doc,
and belongs in `docs/`.
