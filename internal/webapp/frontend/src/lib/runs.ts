import type { HistoryEntry } from "../api/types";

/* ---- agent runs ----
   Pure grouping for the history feed, no React: the run card's shape and the
   one number in its header, unit-tested on node (`npm test`). */

// One run: the entries that share a (session, device) — or a (note, device)
// for ops written before session ids existed — with the index each came from
// so diff lookups still address the flat feed.
export type Run = { note: string; session?: string; entries: HistoryEntry[]; idx: number[] };
export type Item = { run?: Run; i: number };

// How much of the project a run touched: distinct paths, not ops. A path
// rewritten five times is one file, and this is the number a reader uses to
// size a run before expanding it — the op count stays visible as the rows
// inside the card.
export function runFileCount(run: Run): number {
  return new Set(run.entries.map((e) => e.path)).size;
}

/* Group key = session id + device id when the entry carries a session,
   falling back to note + device id for ops written before Op.Session existed.
   The session is preferred because it is the one of the two the writer cannot
   choose: `bdrive sync --note` sets any note it likes, so grouping on the
   note alone let a member forge a string that collides with a teammate's run
   and merge into their card. The key still guarantees a group never spans two
   journals — one writer, one op range — which is what a later run-wide
   restore needs. Two devices that happen to share a note (or a session) are
   two runs. Grouping spans the whole window rather than only consecutive
   rows, so a run whose ops interleave with another device's still reads as
   one thing; each group sits where its newest member did, keeping the feed
   newest-first. */
export function groupRuns(entries: HistoryEntry[]): Item[] {
  // NUL separator: it cannot occur in a note, a session id or a device id, so
  // no pair of them can collide into one key. Written as "\0", never pasted
  // as a literal NUL (BEA-70: a Bin diff on a .tsx is the tell).
  // The "s"/"n" tag keeps a session-keyed group from ever colliding with a
  // note-keyed one on a device that writes both.
  const key = (e: HistoryEntry) =>
    (e.session ? "s\0" + e.session : "n\0" + e.note) + "\0" + (e.device?.id ?? "");
  const runs = new Map<string, Run>();
  entries.forEach((e, i) => {
    if (!e.note && !e.session) return;
    const run = runs.get(key(e));
    if (run) {
      run.entries.push(e);
      run.idx.push(i);
      return;
    }
    runs.set(key(e), { note: e.note ?? "", session: e.session, entries: [e], idx: [i] });
  });
  // A run that touched one file is not worth a card: the row already shows
  // its note, and wrapping it would say the same thing twice. Grouping earns
  // its chrome from the second file on — counted by file, not by op, so five
  // edits to one path stay five bare rows in place of a card claiming
  // "5 files". Note-less changes were always bare rows.
  const out: Item[] = [];
  const carded = new Set<Run>();
  entries.forEach((e, i) => {
    const run = e.note || e.session ? runs.get(key(e)) : undefined;
    if (!run || runFileCount(run) < 2) {
      out.push({ i });
      return;
    }
    if (carded.has(run)) return; // its rows live inside the card
    carded.add(run);
    out.push({ run, i });
  });
  return out;
}
