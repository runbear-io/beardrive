import * as Y from "yjs";
import {
  EditorState,
  StateField,
  Transaction,
  type Extension,
  type Range,
} from "@codemirror/state";
import {
  Decoration,
  EditorView,
  ViewPlugin,
  WidgetType,
  type DecorationSet,
  type ViewUpdate,
} from "@codemirror/view";
import { codeFolding, foldEffect, foldedRanges, unfoldEffect } from "@codemirror/language";
import { ySyncAnnotation } from "y-codemirror.next";
import type { CollabDoc } from "./collab";
import { offlineEditable, personHue, sameName, sections, summary, type Section } from "./meeting";

/* Per-person sections in a meeting note, inside CodeMirror.

   Every `## Name` heading gets an avatar, a fold toggle and a status
   (typing… / updated HH:MM / not started). Status is derived here, in this
   browser, from what the shared document and awareness already carry: a
   remote cursor inside the section, and the time of the last change this
   client saw land in it. Nothing is stored and nothing is sent.

   Collapsing is CodeMirror folding — a decoration over the view, never an
   edit — so a collapsed section is still the same bytes in the document and
   in everyone else's editor. */

export type MeetingOptions = {
  me: string;
  collab: CollabDoc | null;
  // True while the co-editing socket is down: edits outside your own section
  // are refused then, so a stale copy cannot overwrite a teammate's live
  // section when it merges back.
  offline: () => boolean;
  onBlocked?: () => void;
};

const sectionsField = StateField.define<Section[]>({
  create: (s) => sections(s.doc.toString()),
  update: (v, tr) => (tr.docChanged ? sections(tr.newDoc.toString()) : v),
});

export function sectionsOf(state: EditorState): Section[] {
  return state.field(sectionsField, false) ?? sections(state.doc.toString());
}

const TYPING_MS = 5_000;

type Status = { kind: "typing" | "updated" | "empty" | "idle" | "you"; who?: string; at?: number };

function initials(name: string): string {
  const words = name.trim().split(/[\s@._-]+/).filter(Boolean);
  if (!words.length) return "?";
  return (words[0][0] + (words[1]?.[0] ?? "")).toUpperCase();
}

export function avatarEl(name: string, size = 20): HTMLElement {
  const el = document.createElement("span");
  const h = personHue(name);
  el.className = "pavatar";
  el.textContent = initials(name);
  el.style.setProperty("--ph", String(h));
  el.style.width = el.style.height = size + "px";
  return el;
}

class HeadWidget extends WidgetType {
  constructor(
    readonly name: string,
    readonly folded: boolean,
    readonly foldable: boolean,
  ) {
    super();
  }
  eq(o: HeadWidget) {
    return o.name === this.name && o.folded === this.folded && o.foldable === this.foldable;
  }
  toDOM() {
    const wrap = document.createElement("span");
    wrap.className = "ms-head";
    const btn = document.createElement("button");
    btn.type = "button";
    btn.className = "ms-toggle";
    btn.dataset.section = this.name;
    btn.setAttribute("aria-label", (this.folded ? "Expand " : "Collapse ") + this.name);
    btn.setAttribute("aria-expanded", String(!this.folded));
    btn.disabled = !this.foldable;
    wrap.append(btn, avatarEl(this.name));
    return wrap;
  }
  ignoreEvent() {
    return false;
  }
}

class StatusWidget extends WidgetType {
  constructor(readonly s: Status) {
    super();
  }
  eq(o: StatusWidget) {
    return o.s.kind === this.s.kind && o.s.who === this.s.who && o.s.at === this.s.at;
  }
  toDOM() {
    const el = document.createElement("span");
    el.className = "ms-status ms-" + this.s.kind;
    el.dataset.status = this.s.kind;
    const t = this.s.at
      ? new Date(this.s.at).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" })
      : "";
    el.textContent =
      this.s.kind === "typing"
        ? `${this.s.who || "Someone"} is typing…`
        : this.s.kind === "updated"
          ? `Updated ${t}`
          : this.s.kind === "empty"
            ? "Not started"
            : this.s.kind === "you"
              ? "You"
              : "";
    if (this.s.kind === "typing" && this.s.who) el.style.setProperty("--ph", String(personHue(this.s.who)));
    return el;
  }
}

// remoteCursors is where each OTHER editor's caret sits, by name.
export function remoteCursors(collab: CollabDoc | null): Array<{ name: string; pos: number }> {
  if (!collab) return [];
  const out: Array<{ name: string; pos: number }> = [];
  collab.awareness.getStates().forEach((st, id) => {
    if (id === collab.doc.clientID) return;
    const c = (st as { cursor?: { head?: unknown }; user?: { name?: string } }).cursor;
    if (!c?.head) return;
    const abs = Y.createAbsolutePositionFromRelativePosition(
      Y.createRelativePositionFromJSON(c.head),
      collab.doc,
    );
    if (abs) out.push({ name: (st as { user?: { name?: string } }).user?.name ?? "Someone", pos: abs.index });
  });
  return out;
}

function isFolded(state: EditorState, s: Section): boolean {
  let hit = false;
  foldedRanges(state).between(s.headingEnd, s.headingEnd + 1, (from) => {
    if (from === s.headingEnd) hit = true;
  });
  return hit;
}

function foldable(s: Section): boolean {
  return s.to > s.headingEnd + 1;
}

export function toggleSection(view: EditorView, name: string) {
  const s = sectionsOf(view.state).find((x) => x.name === name);
  if (!s || !foldable(s)) return;
  const range = { from: s.headingEnd, to: s.to };
  view.dispatch({ effects: isFolded(view.state, s) ? unfoldEffect.of(range) : foldEffect.of(range) });
}

export function meetingSections(o: MeetingOptions): Extension {
  const plugin = ViewPlugin.fromClass(
    class {
      decorations: DecorationSet = Decoration.none;
      // Last change seen per section, and whether it came from a peer.
      last = new Map<string, { at: number; remote: boolean }>();
      tick: ReturnType<typeof setInterval>;
      offAwareness: () => void;

      constructor(readonly view: EditorView) {
        this.decorations = this.build();
        // Your own section open, everyone else's that has content collapsed,
        // once, when the editor opens. After that folding is the reader's.
        setTimeout(() => {
          const effects = sectionsOf(view.state)
            .filter((s) => foldable(s) && !sameName(s.name, o.me))
            .map((s) => foldEffect.of({ from: s.headingEnd, to: s.to }));
          if (effects.length) view.dispatch({ effects });
        });
        this.tick = setInterval(() => this.refresh(), 2_000);
        const onAw = () => setTimeout(() => this.refresh());
        o.collab?.awareness.on("change", onAw);
        this.offAwareness = () => o.collab?.awareness.off("change", onAw);
      }

      refresh() {
        // An empty transaction re-runs update(), which rebuilds the widgets.
        try {
          this.view.dispatch({});
        } catch {
          /* the view is mid-update or gone; the next tick catches up */
        }
      }

      update(u: ViewUpdate) {
        if (u.docChanged) {
          const secs = sectionsOf(u.state);
          for (const tr of u.transactions) {
            if (!tr.docChanged) continue;
            const remote = !!tr.annotation(ySyncAnnotation);
            tr.changes.iterChangedRanges((_fa, _ta, fb) => {
              const s = secs.find((x) => fb >= x.from && fb <= x.to);
              if (s) this.last.set(s.name, { at: Date.now(), remote });
            });
          }
        }
        this.decorations = this.build();
      }

      status(s: Section, cursors: Array<{ name: string; pos: number }>): Status {
        const inside = cursors.find((c) => c.pos >= s.from && c.pos <= s.to);
        const l = this.last.get(s.name);
        if (inside && l && l.remote && Date.now() - l.at < TYPING_MS) return { kind: "typing", who: inside.name };
        if (sameName(s.name, o.me)) return { kind: "you" };
        if (l) return { kind: "updated", at: l.at };
        if (!s.body) return { kind: "empty" };
        return { kind: "idle" };
      }

      build(): DecorationSet {
        const state = this.view.state;
        const cursors = remoteCursors(o.collab);
        const out: Range<Decoration>[] = [];
        for (const s of sectionsOf(state)) {
          out.push(
            Decoration.line({
              class: "ms-line" + (sameName(s.name, o.me) ? " ms-mine" : ""),
              attributes: { "data-section": s.name },
            }).range(s.from),
          );
          out.push(
            Decoration.widget({ widget: new HeadWidget(s.name, isFolded(state, s), foldable(s)), side: -1 }).range(
              s.from,
            ),
          );
          out.push(Decoration.widget({ widget: new StatusWidget(this.status(s, cursors)), side: 1 }).range(s.headingEnd));
        }
        return Decoration.set(out, true);
      }

      destroy() {
        clearInterval(this.tick);
        this.offAwareness();
      }
    },
    {
      decorations: (v) => v.decorations,
      eventHandlers: {
        mousedown(e, view) {
          const t = (e.target as HTMLElement).closest?.(".ms-toggle") as HTMLElement | null;
          if (!t?.dataset.section) return false;
          e.preventDefault();
          toggleSection(view, t.dataset.section);
          return true;
        },
      },
    },
  );

  // Offline, only your own section takes typing (offlineEditable). Remote
  // updates carry no user event, so this never refuses what arrives from the
  // hub.
  const guard = EditorState.transactionFilter.of((tr) => {
    if (!tr.docChanged || !tr.annotation(Transaction.userEvent) || !o.offline()) return tr;
    const ranges: Array<[number, number]> = [];
    tr.changes.iterChangedRanges((fa, ta) => ranges.push([fa, ta]));
    if (offlineEditable(sectionsOf(tr.startState), o.me, ranges)) return tr;
    o.onBlocked?.();
    return [];
  });

  return [
    sectionsField,
    codeFolding({
      preparePlaceholder: (state, range) => {
        const s = sectionsOf(state).find((x) => x.headingEnd === range.from);
        return s ? summary(s) : "";
      },
      placeholderDOM: (_view, onclick, prepared) => {
        const el = document.createElement("span");
        el.className = "ms-summary";
        el.textContent = (prepared as string) || "…";
        el.title = "Expand";
        el.onclick = onclick;
        return el;
      },
    }),
    plugin,
    guard,
  ];
}
