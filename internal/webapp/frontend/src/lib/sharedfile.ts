import { CollabDoc, type CollabStatus } from "./collab";
import { putText } from "../api/http";

/* One open file, shared with everyone else who has it open, and written back
   to the hub when the typing stops.

   This is the half of editing that has nothing to do with a keyboard: join the
   room, hold the CRDT, save on idle, save once more on the way out. Both
   editing surfaces sit on it —

   - the source editor (Editor.tsx), which binds CodeMirror to `text`
   - the visual editor (VisualEdit.tsx), which splices patches into `text`
     when someone clicks a paragraph on the rendered page

   — and they share a Y.Text of the FILE SOURCE, which is what lets them
   co-edit each other. Somebody in CodeMirror and somebody clicking a headline
   are in the same room, and neither surface has to know the other exists.

   Nothing here knows what the text means. The source editor treats it as
   markup to type; the visual editor treats it as a string to splice by byte
   range; the file is the same either way. */

export type SaveState = "clean" | "dirty" | "saving" | "error";

// Idle time before the document is written to the file.
export const SAVE_IDLE_MS = 700;

export type SharedFile = {
  readonly collab: CollabDoc;
  /** Text as it stands, whether the relay ever answered or not. */
  current(): string;
  /** Force a save now, skipping the idle wait. */
  saveNow(): Promise<void>;
  destroy(): void;
};

export function openSharedFile(opts: {
  apiBase: string;
  path: string;
  /** The file's current bytes, used only if this client seeds the room. */
  seed: string;
  me?: { name: string; colour: string };
  /** The relay answered: the shared document is live and holds the truth. */
  onReady: (collab: CollabDoc) => void;
  /** No relay at all — an older hub, or a desktop build that does not proxy
      the route. The caller falls back to single-writer editing. */
  onSolo: () => void;
  onState?: (s: SaveState) => void;
  onCollab?: (s: CollabStatus) => void;
  onPeers?: (n: number) => void;
  onSaved?: (text: string) => void;
  /** Called immediately BEFORE a write goes out, not after it lands.
      The change stream announces a write as soon as the hub journals it,
      which can be before the PUT's own response gets back here — so a caller
      that only learns about its own write afterwards will see its own save
      arrive as if a stranger had made it. */
  onWriting?: () => void;
  /** Text to save when the relay never answered, so there is no CRDT to read
      it from. The solo surface owns its own buffer. */
  soloText?: () => string;
}): SharedFile {
  let saved = opts.seed;
  let timer: ReturnType<typeof setTimeout> | null = null;
  const setState = (s: SaveState) => opts.onState?.(s);

  const save = async (text: string) => {
    if (text === saved) return;
    setState("saving");
    opts.onWriting?.();
    try {
      await putText(
        opts.apiBase + "upload/content?path=" + encodeURIComponent(opts.path),
        text,
      );
      saved = text;
      setState("clean");
      opts.onSaved?.(text);
    } catch {
      // Keep the document: the CRDT is the truth until a save lands, and the
      // next edit schedules another attempt.
      setState("error");
    }
  };

  const collab = new CollabDoc(
    opts.apiBase + "collab?path=" + encodeURIComponent(opts.path),
    opts.seed,
    (s) => opts.onCollab?.(s),
    () => opts.onReady(collab),
    () => opts.onSolo(),
    opts.me,
  );

  // Any change to the shared document — mine or a peer's — restarts the idle
  // timer. Whoever stops typing last writes the file, and because the content
  // is identical for everyone, a second writer is a no-op put of a blob the
  // store already has.
  const onDocChange = () => {
    setState("dirty");
    if (timer) clearTimeout(timer);
    timer = setTimeout(() => save(collab.text.toString()), SAVE_IDLE_MS);
  };
  collab.text.observe(onDocChange);
  const onPeerChange = () => opts.onPeers?.(collab.peerCount());
  collab.awareness.on("change", onPeerChange);
  collab.connect();

  // Whichever surface is live holds the truth: the CRDT when the relay
  // answered, the surface's own buffer when it never did.
  const current = () =>
    collab.text.length ? collab.text.toString() : (opts.soloText?.() ?? "");

  return {
    collab,
    current,
    saveNow: () => save(current()),
    destroy() {
      if (timer) clearTimeout(timer);
      // Closing the tab mid-word must not lose the word.
      const text = current();
      if (text && text !== saved) void save(text);
      collab.awareness.off("change", onPeerChange);
      collab.text.unobserve(onDocChange);
      collab.destroy();
    },
  };
}
