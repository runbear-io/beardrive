import { useEffect, useLayoutEffect, useRef } from "react";
import { Compartment, EditorState, Prec, Transaction, type Extension } from "@codemirror/state";
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
import { json } from "@codemirror/lang-json";
import { javascript } from "@codemirror/lang-javascript";
import { python } from "@codemirror/lang-python";
import {
  syntaxHighlighting,
  defaultHighlightStyle,
} from "@codemirror/language";
import { completionKeymap } from "@codemirror/autocomplete";
import { foldKeymap } from "@codemirror/language";
import { yCollab, yUndoManagerKeymap } from "y-codemirror.next";
import * as Y from "yjs";
import { type CollabDoc, type CollabStatus } from "../lib/collab";
import { meetingSections } from "../lib/cm-meeting";
import { slashMenu } from "../lib/cm-slash";
import { openItems, previousNote, sections } from "../lib/meeting";
import { fetchBlobText, fileURLFor } from "../hooks/useBlob";
import { toast } from "../toast";
import { askShrink, withdrawShrink } from "./ShrinkDialog";
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

// How the source is shown. Both are the same CodeMirror on the same room;
// switching reconfigures the view and never remounts it.
export type EditMode = "write" | "markdown";

// What the chrome around the editor reads from it on every change.
export type EditorInfo = {
  text: string;
  line: number;
  col: number;
  collab: CollabDoc | null;
};

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
  mode = "markdown",
  meeting = false,
  files,
  onInfo,
  onPending,
  handle,
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
  mode?: EditMode;
  // A meeting note (lib/meeting.ts isMeetingNote): per-person sections and
  // the meeting commands in the / menu.
  meeting?: boolean;
  // Project file paths, for [[ links and finding the previous meeting.
  files?: string[];
  onInfo?: (i: EditorInfo) => void;
  onPending?: (n: number) => void;
  // The open file, for a caller that writes it from outside the editor and
  // must fold that write in itself (EditView's restore).
  handle?: { current: SharedFile | null };
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
  const cb = useRef({ onSaved, onWriting, onStateChange, onCollab, onPeers, onExternal, onConflictCopy, onInfo, onPending });
  cb.current = { onSaved, onWriting, onStateChange, onCollab, onPeers, onExternal, onConflictCopy, onInfo, onPending };
  const filesRef = useRef(files ?? []);
  filesRef.current = files ?? [];
  const modeRef = useRef(mode);
  modeRef.current = mode;
  const meetingRef = useRef(meeting);
  meetingRef.current = meeting;
  const live = useRef<CollabStatus>("connecting");
  const modeSlot = useRef(new Compartment());
  const seed = useRef(initial);
  // Read through a ref for the same reason `seed` is: the effect must depend
  // on the document alone, and this changes on every save.
  const shaRef = useRef(baseSha);
  shaRef.current = baseSha;
  // The (bytes, version) last handed to this editor, so a later pair can be
  // told apart from the one already in it — including the same bytes at a
  // new sha (our own save landing), which moves the base a save writes on.
  const applied = useRef({ text: initial, sha: baseSha });
  const shared = useRef<SharedFile | null>(null);
  const editor = useRef<EditorView | null>(null);

  /* The file changed under us. Fold it in rather than leaving the buffer on
     bytes that no longer exist anywhere — the splice leaves untouched text
     (and the cursor sitting in it) alone, and refuses outright when it would
     have to overwrite unsaved edits. Nothing here re-seeds the document.

     Depends on `initial` and `baseSha` alone. They are the live ["text",
     file] query, so a write to this path — ours, a co-editor's, an agent's —
     refetches them, and a background revalidation of the cached copy the
     editor opened on lands here the same way. */
  useEffect(() => {
    if (initial === applied.current.text && baseSha === applied.current.sha) return;
    applied.current = { text: initial, sha: baseSha };
    seed.current = initial;
    const f = shared.current;
    // The sha travels with the bytes: both come from the same ["text", file]
    // query, so this is the version `initial` IS. Without it a co-editor's
    // save leaves us writing against a base the hub has moved past.
    cb.current.onExternal?.(f && editor.current ? f.merge(initial, baseSha) : "blocked");
  }, [initial, baseSha]);
  const meRef = useRef(me);
  meRef.current = me;

  /* One view for the life of the document, built before the first paint.

     It used to wait for the room: a plain-text preview stood in until the
     relay answered, and then CodeMirror replaced it — a visible swap from
     unstyled text to the editor, and on a slow room a SECOND swap from the
     stall buffer to the CRDT-bound view. Now the styled, typeable editor is
     on screen from the first frame, seeded from the file, and the room is
     attached to that same view when it is ready: its text is spliced in as
     an ordinary change, then the binding is switched on without touching the
     document, so the caret, selection and scroll all survive the join. */
  useLayoutEffect(() => {
    if (!host.current) return;
    let view: EditorView | null = null;
    // Bound to the room. Until then the buffer is the caller's own: solo, or
    // staging (its typing checkpointed into the room over HTTP and merged
    // into the shared document on the join — sharedfile.ts). There is no
    // onStalled: a slow room has nothing to mount, because the user has been
    // typing into this view since the first frame.
    let viaRoom = false;
    // Our own splice of the room's text into the buffer is not typing.
    let splicing = false;
    const collabSlot = new Compartment();
    const langSlot = new Compartment();
    const meetingSlot = new Compartment();

    // The room, the CRDT and the idle save — everything about having this file
    // open that is not about a keyboard. Shared with the visual editor, which
    // joins the same room over the same Y.Text, so the two co-edit each other.
    // None of its callbacks fire before this effect returns (the socket is
    // asynchronous, and a relay-less hub's onSolo is a microtask), so the
    // view below exists by the time any of them runs.
    const file = openSharedFile({
      apiBase,
      path,
      seed: seed.current,
      baseSha: shaRef.current,
      who: meRef.current?.name,
      me: meRef.current,
      onConflictCopy: (p) => cb.current.onConflictCopy?.(p),
      onShrinkRefused: (ask) => askShrink(path, ask),
      onShrinkWithdrawn: withdrawShrink,
      onReady: () => attach(),
      // No relay: an older hub, or a desktop build that does not proxy the
      // route. Editing is single-writer then — exactly what it was before
      // co-editing existed. The view is already up and stays the buffer.
      onSolo: () => {
        if (view) report(view);
      },
      onState: (s) => cb.current.onStateChange?.(s),
      onCollab: (s) => {
        live.current = s;
        cb.current.onCollab?.(s);
      },
      onPending: (n) => cb.current.onPending?.(n),
      onPeers: (n) => cb.current.onPeers?.(n),
      onSaved: (t) => cb.current.onSaved?.(t),
      onWriting: () => cb.current.onWriting?.(),
      soloText: () => (view ? view.state.doc.toString() : seed.current),
      // No CRDT to splice into, so the outside write goes straight into
      // CodeMirror — which maps the cursor through it for free.
      soloApply: (e) => view?.dispatch({ changes: e }),
    });
    shared.current = file;
    if (handle) handle.current = file;

    const who = meRef.current?.name ?? "";
    const report = (v: EditorView) => {
      const head = v.state.selection.main.head;
      const line = v.state.doc.lineAt(head);
      cb.current.onInfo?.({
        text: v.state.doc.toString(),
        line: line.number,
        col: head - line.from + 1,
        collab: view === v && viaRoom ? file.collab : null,
      });
    };
    const meetingExt = (collab: CollabDoc | null) =>
      meetingRef.current
        ? meetingSections({
            me: who,
            collab,
            offline: () => viaRoom && live.current !== "live",
            onBlocked: () => toast("Offline: other people's sections are read-only until you reconnect."),
          })
        : [];

    // Without the CRDT there is no shared document to observe, so the save
    // timer hangs off CodeMirror's own updates instead — for a solo buffer
    // (written to the file) and a staging one (checkpointed into the room).
    // Bound to the room, the shared document drives saving instead.
    const soloListener = EditorView.updateListener.of((u) => {
      if (!u.docChanged || viaRoom || splicing) return;
      cb.current.onStateChange?.("dirty");
      if (timer.current) clearTimeout(timer.current);
      timer.current = setTimeout(() => void file.saveNow(), SAVE_IDLE_MS);
    });

    view = new EditorView({
      parent: host.current,
      state: EditorState.create({
        doc: seed.current,
        extensions: [
          modeSlot.current.of(modeExtensions(modeRef.current)),
          highlightActiveLine(),
          history(),
          langSlot.of(languageFor(path)),
          syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
          slashMenu({
            me: who,
            files: () => filesRef.current,
            people: () => {
              const names = sections(view?.state.doc.toString() ?? "").map((x) => x.name);
              file.collab.awareness.getStates().forEach((st) => {
                const n = (st as { user?: { name?: string } }).user?.name;
                if (n) names.push(n);
              });
              return names;
            },
            carryOver: meetingRef.current
              ? async () => {
                  const prev = previousNote(path, filesRef.current);
                  if (!prev) return null;
                  const got = await fetchBlobText(fileURLFor(apiBase, prev));
                  return { from: prev, items: got.kind === "text" ? openItems(got.text) : [] };
                }
              : undefined,
            onNotice: (m) => toast(m),
          }),
          keymap.of([
            // Save now, not after the idle wait — and never the browser's own
            // "Save Page As" dialog. In a live room this hands the hub the
            // room's state; clean, it sends nothing.
            {
              key: "Mod-s",
              preventDefault: true,
              run: () => {
                void file.saveNow();
                return true;
              },
            },
            ...completionKeymap,
            ...foldKeymap,
            ...defaultKeymap,
            ...historyKeymap,
            indentWithTab,
          ]),
          EditorView.lineWrapping,
          EditorView.updateListener.of((u) => {
            if (u.docChanged || u.selectionSet || u.focusChanged) report(u.view);
          }),
          collabSlot.of([]),
          meetingSlot.of(meetingExt(null)),
          soloListener,
        ],
      }),
    });
    editor.current = view;
    view.focus();
    report(view);

    /* The room has the document (sharedfile.ts has already merged anything
       typed into the staging buffer into it). Bring this view onto it.

       CollabDoc calls this again on every reconnect: once bound, the binding
       is already carrying the document, and re-attaching would reset the
       undo history and splice text that is already there. */
    const attach = () => {
      if (viaRoom || !view) return;
      clearTimeout(timer.current ?? undefined);
      timer.current = null;
      const remote = file.collab.text.toString();
      const local = view.state.doc.toString();
      if (local !== remote) {
        // The smallest change that turns the buffer into the room's text,
        // applied while the binding is still off: bound, y-sync would push
        // it back into the document it came from and the text would double.
        let from = 0;
        const max = Math.min(local.length, remote.length);
        while (from < max && local.charCodeAt(from) === remote.charCodeAt(from)) from++;
        let tail = 0;
        while (
          tail < max - from &&
          local.charCodeAt(local.length - 1 - tail) === remote.charCodeAt(remote.length - 1 - tail)
        )
          tail++;
        splicing = true;
        try {
          view.dispatch({
            changes: { from, to: local.length - tail, insert: remote.slice(from, remote.length - tail) },
            annotations: [Transaction.addToHistory.of(false)],
          });
        } finally {
          splicing = false;
        }
      }
      // Undo follows the shared document from here: only this client's own
      // changes, never a co-editor's.
      const undoManager = new Y.UndoManager(file.collab.text);
      // Effects only — the document does not change, so nothing is synced.
      view.dispatch({
        effects: [
          collabSlot.reconfigure([
            // The binding: every local edit becomes a Yjs update, every
            // remote update becomes a CodeMirror transaction, and remote
            // cursors are drawn from awareness.
            yCollab(file.collab.text, file.collab.awareness, { undoManager }),
            Prec.high(keymap.of(yUndoManagerKeymap)),
          ]),
          meetingSlot.reconfigure(meetingExt(file.collab)),
        ],
      });
      // An empty update so the remote-selection plugin publishes this caret
      // and paints the peers already in the room.
      view.dispatch({ annotations: [Transaction.addToHistory.of(false)] });
      viaRoom = true;
      report(view);
    };

    return () => {
      if (timer.current) clearTimeout(timer.current);
      shared.current = null;
      if (handle?.current === file) handle.current = null;
      editor.current = null;
      file.destroy();
      view?.destroy();
      view = null;
    };
    // The document, and nothing else. Opening another file rebuilds the
    // editor (correct); a re-render must not.
  }, [apiBase, path]);

  useEffect(() => {
    editor.current?.dispatch({ effects: modeSlot.current.reconfigure(modeExtensions(mode)) });
  }, [mode]);

  return <div ref={host} id="editor" className={"cm-host cm-mode-" + mode} data-mode={mode} />;
}

// Syntax for the file's kind; plain text for anything else.
function languageFor(path: string): Extension {
  if (/\.(md|markdown)$/i.test(path)) return markdown();
  if (/\.jsonl?$/i.test(path)) return json();
  if (/\.(js|jsx|ts|tsx|mjs|cjs)$/i.test(path)) {
    return javascript({ jsx: /\.[jt]sx$/i.test(path), typescript: /\.tsx?$/i.test(path) });
  }
  if (/\.py$/i.test(path)) return python();
  return [];
}

// Write is the note as prose — no gutter, the reading measure; Markdown is the
// source with line numbers.
function modeExtensions(m: EditMode) {
  return m === "markdown" ? [lineNumbers()] : [EditorView.contentAttributes.of({ "data-write": "" })];
}
