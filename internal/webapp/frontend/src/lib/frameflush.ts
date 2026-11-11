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
   every patch before it has been spliced into the document. */

// `lock`: the frame also stops taking edits until `bd-edit:unlock`, so nothing
// typed while the save that follows is on the wire can be left out of it.
export type FlushRequest = { type: "bd-edit:flush"; id: number; lock?: true };

export type FrameChannel = {
  /** Handle one message, after every message before it has been handled. */
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

export function frameChannel(): FrameChannel {
  let queue: Promise<unknown> = Promise.resolve();
  const waiting = new Map<number, () => void>();
  let seq = 0;
  let closed = false;
  const settled = () => queue.then(() => {});
  return {
    enqueue(handle) {
      // A handler that throws must not stall every message after it.
      queue = queue.then(handle).catch(() => {});
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
