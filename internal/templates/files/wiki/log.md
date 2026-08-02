# Log

Append-only, oldest first. One entry per ingest, per query worth remembering,
and per lint pass. An entry that is already here is never edited — this file is
the timeline, not the current state.

Keep the heading format exactly as below. The consistent prefix is what makes
the log readable with ordinary tools:

```sh
grep "^## \[" log.md | tail -5     # what happened recently
```

<!-- The shape to follow:

## [2026-07-30] ingest | Q2 earnings call
Transcript into sources/. Touched [[acme-corp]], [[margin-compression]], and a
new page [[switching-costs]]. Contradicts the March guidance on unit costs —
flagged on [[margin-compression]].

## [2026-07-31] query | why did unit costs move?
Answered from [[margin-compression]] + [[acme-corp]]. Kept as [[unit-cost-2026]].

## [2026-08-02] lint | 34 pages
Two orphans linked up. One stale claim on [[acme-corp]] superseded by the Q2
call. Gap: nothing on their supply chain — worth finding a source.
-->
