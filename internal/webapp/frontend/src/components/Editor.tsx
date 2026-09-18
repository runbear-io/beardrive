import { useEffect, useRef } from "react";
import { EditorState } from "@codemirror/state";
import {
  EditorView,
  keymap,
  lineNumbers,
  highlightActiveLine,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import { markdown } from "@codemirror/lang-markdown";
import {
  syntaxHighlighting,
  defaultHighlightStyle,
} from "@codemirror/language";
import { yCollab } from "y-codemirror.next";
import { type CollabStatus } from "../lib/collab";
import {
  openSharedFile,
  SAVE_IDLE_MS,
  type MergeResult,
  type SaveState,
  type SharedFile,
} from "../lib/sharedfile";

/* Editing a text file in the browser, with everyone else who has it open.

   This reverses a stated product decision — the web app was a read/share/
   history surface and content entered only through local sync (Browser.tsx).
   The server side needed nothing new for the write itself: upload/content has
   existed, PermWrite and quota-checked, with no caller.

   Two layers, and the split is the whole design:

   - The DOCUMENT is a CRDT (Yjs) shared through the hub's relay. That is what
     makes two people typing in one paragraph merge instead of clobber. It
     lives only between browsers and only while they are editing.
   - The FILE is what everyone else sees, and it is unchanged: an ordinary
     blob written by an ordinary upload/content call. The journal never learns
     a CRDT exists, so desktop devices, agents and older clients converge
     exactly as before.

   Snapshotting is therefore a client's job, not the hub's. Whoever stops
   typing last writes the file; the relay's log is deliberately not durable —
   the file is. */

export type { SaveState };

export function Editor({
  apiBase,
  path,
  initial,
  onSaved,
  onWriting,
  onStateChange,
  onCollab,
  onPeers,
  onExternal,
  onConflictCopy,
  baseSha,
  me,
}: {
  apiBase: string;
  path: string;
  initial: string;
  onSaved?: (text: string) => void;
  onWriting?: () => void;
  onStateChange?: (s: SaveState) => void;
  onCollab?: (s: CollabStatus) => void;
  /* Someone wrote the file while it was open here, and whether that write
     could be folded into the buffer. False is the case the banner is for. */
  onExternal?: (r: MergeResult) => void;
  // A concurrent edit this client could not merge: its version was preserved
  // beside the file rather than dropped.
  onConflictCopy?: (path: string) => void;
  // The version the buffer was read at, so a save can refuse to land on top
  // of somebody else's write.
  baseSha?: string;
  // Who this editor is, for the label and colour on a remote caret.
  me?: { name: string; colour: string };
  // Reports whether other editors are in the document, so the caller can
  // tell a co-editor's snapshot from an outside write.
  onPeers?: (n: number) => void;
}) {
  const host = useRef<HTMLDivElement>(null);
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null);

  // Everything the effect needs from props goes through a ref, and the effect
  // depends on the DOCUMENT ALONE (apiBase, path). Two reasons, both of which
  // cost real bugs before they were understood:
  //
  //  - the callbacks are inline arrows at the call site, so they change
  //    identity every render, and reporting a save state re-renders. With them
  //    in the deps CodeMirror was torn down and rebuilt on every keystroke,
  //    which reset the cursor to 0 and wrote the typed text back to front.
  //  - `initial` changes whenever the seed query refetches, and a peer's write
  //    invalidates exactly that query — so it would reset the buffer under the
  //    typist's cursor, which is the one thing this component must never do.
  const cb = useRef({ onSaved, onWriting, onStateChange, onCollab, onPeers, onExternal, onConflictCopy });
  cb.current = { onSaved, onWriting, onStateChange, onCollab, onPeers, onExternal, onConflictCopy };
  const seed = useRef(initial);
  // The bytes this editor opened with, so a later `initial` can be told apart
  // from the one that mounted us.
  const opened = useRef(initial);
  const shared = useRef<SharedFile | null>(null);
  const editor = useRef<EditorView | null>(null);

  /* The file changed under us. Fold it in rather than leaving the buffer on
     bytes that no longer exist anywhere — the splice leaves untouched text
     (and the cursor sitting in it) alone, and refuses outright when it would
     have to overwrite unsaved edits. Nothing here re-seeds the document.

     Depends on `initial` ALONE. It is the live ["text", file] query, so a
     write to this path — ours, a co-editor's, an agent's — refetches it. */
  useEffect(() => {
    if (initial === opened.current) return;
    seed.current = initial;
    const f = shared.current;
    // Before the editor is up there is nothing to splice into, and the room
    // may still be seeding from bytes we now know are stale: say so.
    cb.current.onExternal?.(
      f && editor.current ? f.merge(initial) : "blocked",
    );
  }, [initial]);
  const meRef = useRef(me);
  meRef.current = me;
  // Read through a ref for the same reason `seed` is: the effect must depend
  // on the document alone, and this changes on every save.
  const shaRef = useRef(baseSha);
  shaRef.current = baseSha;

  useEffect(() => {
    if (!host.current) return;
    let view: EditorView | null = null;

    // The extensions both mounts share. Only the collaborative binding and
    // the change listener differ between them.
    const baseExtensions = [
      lineNumbers(),
      highlightActiveLine(),
      history(),
      markdown(),
      syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
      keymap.of([...defaultKeymap, ...historyKeymap, indentWithTab]),
      EditorView.lineWrapping,
    ];

    // Without the CRDT there is no shared document to observe, so the save
    // timer hangs off CodeMirror's own updates instead.
    const soloListener = EditorView.updateListener.of((u) => {
      if (!u.docChanged) return;
      cb.current.onStateChange?.("dirty");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => void file.saveNow(), SAVE_IDLE_MS);
    });

    // The fallback surface: the same editor without the Yjs binding, seeded
    // from the file. Its edits still reach everyone through the ordinary save.
    const mountSolo = () => {
      if (view || !host.current) return;
      view = new EditorView({
        parent: host.current,
        state: EditorState.create({
          doc: seed.current,
          extensions: [...baseExtensions, soloListener],
        }),
      });
      editor.current = view;
      view.focus();
    };

    // Mounted only once the relay has answered, so CodeMirror binds to a
    // document that already holds either the file's text or the room's —
    // never to an empty one that would then have text appear underneath it.
    const mount = () => {
      if (view || !host.current) return;
      view = new EditorView({
        parent: host.current,
        state: EditorState.create({
          doc: file.collab.text.toString(),
          extensions: [
            ...baseExtensions,
            // The binding: every local edit becomes a Yjs update, every
            // remote update becomes a CodeMirror transaction, and remote
            // cursors are drawn from awareness.
            yCollab(file.collab.text, file.collab.awareness),
          ],
        }),
      });
      editor.current = view;
      view.focus();
    };

    // The room, the CRDT and the idle save — everything about having this file
    // open that is not about a keyboard. Shared with the visual editor, which
    // joins the same room over the same Y.Text, so the two co-edit each other.
    const file = openSharedFile({
      apiBase,
      path,
      seed: seed.current,
      baseSha: shaRef.current,
      who: meRef.current?.name,
      me: meRef.current,
      onConflictCopy: (p) => cb.current.onConflictCopy?.(p),
      onReady: () => mount(),
      // No relay: an older hub, or a desktop build that does not proxy the
      // route. Editing is single-writer then — exactly what it was before
      // co-editing existed — rather than a pane that never appears.
      onSolo: () => mountSolo(),
      onState: (s) => cb.current.onStateChange?.(s),
      onCollab: (s) => cb.current.onCollab?.(s),
      onPeers: (n) => cb.current.onPeers?.(n),
      onSaved: (t) => cb.current.onSaved?.(t),
      onWriting: () => cb.current.onWriting?.(),
      soloText: () => view?.state.doc.toString() ?? "",
      // No CRDT to splice into, so the outside write goes straight into
      // CodeMirror — which maps the cursor through it for free.
      soloApply: (e) => view?.dispatch({ changes: e }),
    });
    shared.current = file;

    return () => {
      if (timer.current) clearTimeout(timer.current);
      shared.current = null;
      editor.current = null;
      file.destroy();
      view?.destroy();
    };
    // The document, and nothing else. Opening another file rebuilds the
    // editor (correct); a re-render must not.
  }, [apiBase, path]);

  return <div ref={host} id="editor" className="cm-host" />;
}
