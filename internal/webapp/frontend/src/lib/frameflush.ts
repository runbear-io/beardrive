/* The visual editor's side of the iframe's message stream, in order.

   Edits reach the app from the sandboxed iframe as postMessage, which is
   asynchronous: a word typed just before Done may still be in the queue when
   Done asks the file to save, and a save that sees a clean buffer answers at
   once — the editor closed, and the word never met the save (or the shrink
   check, or the failure) that would have kept it open.

   So every message is handled strictly in arrival order, each after the one
   before it has finished (anchoring is async), and draining asks the frame to
   post what it holds and answer `bd-edit:flushed`. The frame sends that answer
   after its last patch on the same channel, so by the time it is handled here
   every patch before it has been spliced into the document.

   Order must not cost time, though. With nothing waiting, a message is
   handled synchronously inside its own event: a page that is going
   (pagehide straight after the message, before any microtask) must already
   have the patch in the document it sends. Only a handler that returns a
   promise holds the messages after it. */

// `lock`: the frame also stops taking edits until `bd-edit:unlock`, so nothing
// typed while the save that follows is on the wire can be left out of it.
export type FlushRequest = { type: "bd-edit:flush"; id: number; lock?: true };

export type FrameChannel = {
  /** Handle one message, after every message before it has been handled —
      at once, synchronously, when nothing is still waiting. */
  enqueue(handle: () => unknown): void;
  /** The frame answered flush request `id` (called from a queued handler). */
  flushed(id: number): void;
  /** Ask the frame for everything it holds and wait until it has been
      handled. Waits as long as the frame is there to answer — never on a
      timer — and gives up only once `gone()` says it cannot. With `lock`, the
      frame takes no more edits until it is sent `bd-edit:unlock`. */
  drain(post: (msg: FlushRequest) => void, gone: () => boolean, lock?: boolean): Promise<void>;
  /** The editor is going: nothing waits on the frame any more. */
  close(): void;
};

// How often a drain checks that the frame it asked is still there to answer.
const GONE_POLL_MS = 50;

const isPromiseLike = (v: unknown): v is PromiseLike<unknown> =>
  v !== null && (typeof v === "object" || typeof v === "function") && "then" in v && typeof v.then === "function";

export function frameChannel(): FrameChannel {
  // The async handler still running, and everything queued behind it.
  let pending: Promise<unknown> | null = null;
  const waiting = new Map<number, () => void>();
  let seq = 0;
  let closed = false;
  const settled = (): Promise<void> => (pending ? pending.then(() => {}) : Promise.resolve());
  // A handler that throws must not stall every message after it.
  const hold = (p: Promise<unknown>) => {
    const held: Promise<unknown> = p.catch(() => {}).finally(() => {
      if (pending === held) pending = null;
    });
    pending = held;
  };
  return {
    enqueue(handle) {
      if (pending) {
        hold(pending.then(handle));
        return;
      }
      try {
        const out = handle();
        if (isPromiseLike(out)) hold(Promise.resolve(out));
      } catch {
        // as above: the next message is handled regardless
      }
    },
    flushed(id) {
      waiting.get(id)?.();
      waiting.delete(id);
    },
    async drain(post, gone, lock = false) {
      await settled();
      if (closed || gone()) return;
      const id = ++seq;
      let poll: ReturnType<typeof setInterval> | undefined;
      const answered = new Promise<void>((resolve) => {
        waiting.set(id, resolve);
        poll = setInterval(() => {
          if (!closed && !gone()) return;
          waiting.delete(id);
          resolve();
        }, GONE_POLL_MS);
      });
      try {
        post(lock ? { type: "bd-edit:flush", id, lock: true } : { type: "bd-edit:flush", id });
        await answered;
      } finally {
        clearInterval(poll);
      }
      await settled();
    },
    close() {
      closed = true;
      for (const resolve of waiting.values()) resolve();
      waiting.clear();
    },
  };
}

/* Done's save through the frame: drain it and lock it (`drain`), save, and
   unlock it unless the save landed. A save that lands leaves the frame
   locked, since the editor is about to close; one that fails, throws or
   opens a question unlocks it, since the editor stays and the user may go on
   typing. */
export async function saveLocked(
  drain: () => Promise<void>,
  save: () => Promise<boolean>,
  unlock: () => void,
): Promise<boolean> {
  let ok = false;
  try {
    await drain();
    ok = await save();
    return ok;
  } finally {
    if (!ok) unlock();
  }
}
