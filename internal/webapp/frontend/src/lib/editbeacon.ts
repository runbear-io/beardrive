/* The editing iframe's last word, when the app may never hear it.

   An edit reaches the app as a postMessage, and the app acknowledges each
   patch once it is in the document. A tab closed between the keystroke and
   that acknowledgement leaves an edit only the iframe holds — and the iframe
   is sandboxed without allow-same-origin, so it has no session to save it
   with. What it has instead is a single-file capability the app fetched for
   it (POST ycollab/frame): on the way out it posts, cookie-free, the rendered
   document's hash and every span it edited, and the hub splices them onto
   that version and merges the result like any staging checkpoint (idempotent
   if the app's own save also landed).

   Pure, so it is tested without a DOM; inline-edit.ts owns the state. */

export type EditedSpan = { start: number; end: number; html: string; seq: number };

/* The beacon's body, or null when nothing is owed: no capability yet, every
   patch acknowledged, no rendered version to splice onto, or nothing edited.
   Every span edited on this render goes, not just the unacknowledged ones:
   the hub rebuilds the document from `src_hash`, so the patches must spell
   all of it. */
export function beaconBody(o: {
  url: string;
  patchSeq: number;
  ackedSeq: number;
  hash: string;
  len: number;
  spans: Iterable<EditedSpan>;
}): string | null {
  if (!o.url || o.patchSeq <= o.ackedSeq || !o.hash || o.len < 0) return null;
  const patches = [...o.spans]
    .sort((a, b) => a.start - b.start)
    .map(({ start, end, html }) => ({ start, end, html }));
  if (!patches.length) return null;
  return JSON.stringify({ src_hash: o.hash, src_len: o.len, patches });
}
