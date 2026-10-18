/* Pure helpers for editing meeting notes: the per-person sections, the action
   items, carrying open items over from the previous meeting, and the shrink
   rule the hub enforces on a save. Nothing here touches the DOM or the CRDT,
   so every rule is unit-tested in meeting.test.ts. */

/* A file is a meeting note when any folder on its path is `daily-meetings`
   (the team convention: `areas/team/daily-meetings/2026-10/2026-10-16.md`).
   One rule, on the path alone, so the editor can decide before the text
   has loaded and a note never changes kind while somebody types. */
export function isMeetingNote(path: string): boolean {
  return /\.md$/i.test(path) && path.split("/").slice(0, -1).includes("daily-meetings");
}

export type Section = {
  // The heading text after "## ", trimmed.
  name: string;
  // Offsets into the document: the heading line starts at `from`, and the
  // section runs to the next level-1/2 heading or the end of the text.
  from: number;
  headingEnd: number;
  to: number;
  // The body with blank lines removed; empty means the person has not
  // written anything yet.
  body: string;
};

// sections splits text into its `## Name` sections. Headings inside fenced
// code blocks are not headings.
export function sections(text: string): Section[] {
  const out: Section[] = [];
  let pos = 0;
  let fence = false;
  let cur: Section | null = null;
  const close = (end: number) => {
    if (!cur) return;
    cur.to = end;
    cur.body = text
      .slice(cur.headingEnd, end)
      .split("\n")
      .filter((l) => l.trim())
      .join("\n");
    out.push(cur);
    cur = null;
  };
  for (const line of text.split("\n")) {
    const end = pos + line.length;
    if (/^\s*(```|~~~)/.test(line)) fence = !fence;
    else if (!fence) {
      const m = /^(#{1,2})\s+(.+?)\s*#*\s*$/.exec(line);
      if (m) {
        close(pos > 0 ? pos - 1 : 0);
        if (m[1].length === 2) cur = { name: m[2], from: pos, headingEnd: end, to: end, body: "" };
      }
    }
    pos = end + 1;
  }
  close(text.length);
  return out;
}

// sectionAt is the section containing offset, if any.
export function sectionAt(list: Section[], offset: number): Section | undefined {
  return list.find((s) => offset >= s.from && offset <= s.to);
}

// summary is a section's one-line form for its collapsed state.
export function summary(s: Section, max = 90): string {
  const flat = s.body
    .split("\n")
    .map((l) => l.replace(/^\s*(?:[-*+]|\d+\.)\s+(?:\[[ xX]\]\s+)?/, "").trim())
    .filter(Boolean)
    .join(" · ");
  return flat.length > max ? flat.slice(0, max - 1) + "…" : flat;
}

export type ActionItem = {
  text: string;
  owner: string;
  done: boolean;
  // The `## ` section the line sits in, if any.
  section: string;
  line: number;
};

const TASK = /^\s*[-*+]\s+\[([ xX])\]\s+(.*)$/;

// actionItems collects every `- [ ] …` task line. The owner is the first
// @mention on the line; a task without one still counts, with no owner.
export function actionItems(text: string): ActionItem[] {
  const secs = sections(text);
  const out: ActionItem[] = [];
  let pos = 0;
  text.split("\n").forEach((line, i) => {
    const m = TASK.exec(line);
    if (m) {
      const owner = /(?:^|\s)@([\w.-]+)/.exec(m[2])?.[1] ?? "";
      const body = m[2].replace(/(?:^|\s)@[\w.-]+/g, "").trim();
      out.push({
        text: body,
        owner,
        done: m[1] !== " ",
        section: sectionAt(secs, pos)?.name ?? "",
        line: i + 1,
      });
    }
    pos += line.length + 1;
  });
  return out;
}

// itemsMarkdown renders action items as a markdown task list, for pasting
// into a tracker or another note.
export function itemsMarkdown(items: ActionItem[]): string {
  return items
    .map((a) => `- [${a.done ? "x" : " "}] ${a.text}${a.owner ? " @" + a.owner : ""}`)
    .join("\n");
}

// openItems are the unchecked task lines of a note, verbatim (indent
// dropped), in order — what "carry over" copies forward.
export function openItems(text: string): string[] {
  return text
    .split("\n")
    .filter((l) => TASK.exec(l)?.[1] === " ")
    .map((l) => l.trim());
}

/* previousNote finds the meeting note before `path`: the latest file in the
   same daily-meetings tree whose name is a date earlier than this one. The
   tree is searched, not just the folder, because notes are filed per month
   (`2026-10/2026-10-01.md`'s previous note is in `2026-09/`). */
export function previousNote(path: string, files: string[]): string | null {
  const date = (p: string) => /(\d{4}-\d{2}-\d{2})[^/]*\.md$/i.exec(p)?.[1] ?? "";
  const mine = date(path);
  const segs = path.split("/");
  const root = segs.slice(0, segs.lastIndexOf("daily-meetings") + 1).join("/") + "/";
  if (!mine || root === "/") return null;
  let best: string | null = null;
  let bestDate = "";
  for (const f of files) {
    if (f === path || !f.startsWith(root)) continue;
    const d = date(f);
    if (d && d < mine && d > bestDate) {
      best = f;
      bestDate = d;
    }
  }
  return best;
}

export const SHRINK_RATIO_FLOOR = 256;

/* sharpShrink is the hub's rule (upload.go) for a save that needs explicit
   confirmation: any non-empty file going to empty, or a file of at least
   SHRINK_RATIO_FLOOR bytes losing more than four fifths. Sizes are bytes. */
export function sharpShrink(was: number, now: number): boolean {
  if (was <= 0 || now >= was) return false;
  if (now === 0) return true;
  return was >= SHRINK_RATIO_FLOOR && now * 5 < was;
}

export const byteLength = (s: string) => new TextEncoder().encode(s).length;

export function wordCount(s: string): number {
  return s.split(/\s+/).filter((w) => /[\p{L}\p{N}]/u.test(w)).length;
}

// agoShort formats a past instant relative to now: "just now", "12s ago",
// "4m ago", then a clock time.
export function agoShort(t: number, now = Date.now()): string {
  const s = Math.max(0, Math.round((now - t) / 1000));
  if (s < 3) return "just now";
  if (s < 60) return `${s}s ago`;
  if (s < 3600) return `${Math.floor(s / 60)}m ago`;
  return new Date(t).toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
}

// personHue is the colour hash PresenceBar and the editor caret share, so a
// person is the same hue on every surface.
export function personHue(name: string): number {
  let h = 0;
  for (let i = 0; i < name.length; i++) h = (h * 31 + name.charCodeAt(i)) % 360;
  return h;
}

// sameName compares a section heading with a person's display name: case-
// and space-insensitive, and a heading may carry only the first name.
export function sameName(heading: string, person: string): boolean {
  const n = (s: string) => s.trim().toLowerCase().replace(/\s+/g, " ");
  const h = n(heading);
  const p = n(person);
  if (!h || !p) return false;
  return h === p || p.split(/[ @.]/)[0] === h;
}

/* Whether an edit made while the room is offline may go through. Only the
   body of your own section is editable then: nobody else's section, not the
   preamble, and nothing at all in a note with no section of yours — any of
   those could be under someone else's live typing that this tab cannot see
   until it reconnects. Ranges are [from, to] in the document before the
   edit. */
export function offlineEditable(secs: Section[], me: string, ranges: Array<[number, number]>): boolean {
  const mine = secs.find((s) => sameName(s.name, me));
  if (!mine) return false;
  return ranges.every(([from, to]) => from >= mine.headingEnd && to <= mine.to);
}
