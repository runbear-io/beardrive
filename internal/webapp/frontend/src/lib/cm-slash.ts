import {
  autocompletion,
  startCompletion,
  type Completion,
  type CompletionContext,
  type CompletionResult,
} from "@codemirror/autocomplete";
import type { EditorView } from "@codemirror/view";
import type { Extension } from "@codemirror/state";

/* The editor's `/` menu and the two pickers it opens.

   `/` at the start of a line (or after a list marker) offers the meeting
   commands; `@` completes a person; `[[` completes a file from the project
   tree. All three are CodeMirror completions, so the insertions are ordinary
   local edits and reach the shared document like typing does. */

export type SlashOptions = {
  // Display names to offer after `@`: the note's section headings and the
  // people in the room.
  people: () => string[];
  // Project file paths, for `[[`.
  files: () => string[];
  me: string;
  // The previous meeting note's open `- [ ]` lines, or null when there is
  // no previous note. Absent outside meeting notes.
  carryOver?: () => Promise<{ from: string; items: string[] } | null>;
  onNotice?: (msg: string) => void;
};

const handle = (name: string) => name.trim().split(/\s+/)[0];

function insert(view: EditorView, from: number, to: number, text: string, cursor = text.length) {
  view.dispatch({
    changes: { from, to, insert: text },
    selection: { anchor: from + cursor },
    userEvent: "input.complete",
  });
}

export function slashMenu(o: SlashOptions): Extension {
  const commands = (ctx: CompletionContext): CompletionResult | null => {
    const m = ctx.matchBefore(/(?:^|\s)\/\w*$/);
    if (!m) return null;
    const line = ctx.state.doc.lineAt(ctx.pos);
    const before = ctx.state.sliceDoc(line.from, m.from + (m.text.startsWith("/") ? 0 : 1));
    if (!/^\s*(?:[-*+]\s+(?:\[[ xX]\]\s+)?)?$/.test(before)) return null;
    const from = m.from + (m.text.startsWith("/") ? 0 : 1);
    const listed = /[-*+]\s+$/.test(before);
    const tasked = /\]\s+$/.test(before);
    const opts: Completion[] = [
      {
        label: "/action",
        displayLabel: "Action item",
        detail: "- [ ] with an owner",
        section: "Meeting",
        apply: (view, _c, f, t) => {
          const lead = tasked ? "" : listed ? "[ ] " : "- [ ] ";
          const owner = ` @${handle(o.me)}`;
          insert(view, f, t, lead + owner, lead.length);
        },
      },
      {
        label: "/mention",
        displayLabel: "Mention someone",
        detail: "@name",
        section: "Meeting",
        apply: (view, _c, f, t) => {
          insert(view, f, t, "@");
          startCompletion(view);
        },
      },
      {
        label: "/link",
        displayLabel: "Link a file",
        detail: "[[ file from this project",
        section: "Meeting",
        apply: (view, _c, f, t) => {
          insert(view, f, t, "[[]]", 2);
          startCompletion(view);
        },
      },
    ];
    if (o.carryOver) {
      const carry = o.carryOver;
      opts.push({
        label: "/carry",
        displayLabel: "Carry over from last meeting",
        detail: "open - [ ] items",
        section: "Meeting",
        apply: (view, _c, f, t) => {
          insert(view, f, t, "");
          void carry().then((r) => {
            if (!r) return o.onNotice?.("No earlier meeting note in this folder.");
            if (!r.items.length) return o.onNotice?.(`No open items in ${r.from.split("/").pop()}.`);
            const at = view.state.selection.main.head;
            const ln = view.state.doc.lineAt(at);
            // Replace an empty bullet the menu was opened on; otherwise insert
            // the list on its own lines.
            const blank = /^\s*(?:[-*+]\s*(?:\[[ xX]\]\s*)?)?$/.test(ln.text);
            const text = r.items.join("\n");
            if (blank) insert(view, ln.from, ln.to, text);
            else insert(view, at, at, "\n" + text);
          });
        },
      });
    }
    return { from, options: opts, filter: true };
  };

  const mentions = (ctx: CompletionContext): CompletionResult | null => {
    const m = ctx.matchBefore(/(?:^|[\s(])@[\w.-]*$/);
    if (!m) return null;
    const from = m.from + m.text.indexOf("@");
    const names = [...new Set(o.people().map(handle).filter(Boolean))];
    return {
      from,
      options: names.map((n) => ({ label: "@" + n, type: "variable", apply: "@" + n + " " })),
    };
  };

  const files = (ctx: CompletionContext): CompletionResult | null => {
    const m = ctx.matchBefore(/\[\[[^\]\n]*$/);
    if (!m) return null;
    const after = ctx.state.sliceDoc(ctx.pos, ctx.pos + 2);
    return {
      from: m.from + 2,
      options: o.files().map((p) => {
        const name = (p.split("/").pop() ?? p).replace(/\.md$/i, "");
        return {
          label: name,
          detail: p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "",
          apply: (view: EditorView, _c: Completion, f: number, t: number) =>
            insert(view, f, t + (after === "]]" ? 2 : 0), name + "]]"),
        };
      }),
    };
  };

  return autocompletion({
    override: [commands, mentions, files],
    activateOnTyping: true,
    icons: false,
  });
}
