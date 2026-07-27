import { useState } from "react";
import type { HistoryEntry } from "../api/types";
import { humanSize } from "../util";

/* One change as a row: what happened (added / edited / deleted), to which
   file, by whom, from where — with the note (session link) expandable.

   The kind is a word in a badge, never a +/✕ glyph in the row's leftmost
   slot: sitting there, inside a role="button" row, it read as a disclosure
   toggle it never was. The toggle slot stays free for a real per-row
   disclosure; the only genuine expansion here is the note, which owns its
   own control and its own aria-expanded. */
const KIND_LABEL: Record<string, string> = { add: "added", edit: "edited", delete: "deleted" };

export function HistoryRow({
  entry: e,
  onOpen,
}: {
  entry: HistoryEntry;
  onOpen: (path: string) => void;
}) {
  const [noteOpen, setNoteOpen] = useState(false);
  const kind = e.kind === "put" ? "edit" : e.kind; // older servers report raw "put" ops
  const who = e.user_name ? `${e.user_name} <${e.user}>` : e.user || e.author || "unknown";
  const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
  const clickable = kind !== "delete";
  const open = (ev: React.MouseEvent | React.KeyboardEvent) => {
    if ((ev.target as HTMLElement).tagName === "A") return;
    if (clickable) onOpen(e.path);
  };
  return (
    <div
      className={"hentry " + kind + (clickable ? " clickable" : "")}
      tabIndex={clickable ? 0 : undefined}
      role={clickable ? "button" : undefined}
      onClick={open}
      onKeyDown={(ev) => {
        if (clickable && (ev.key === "Enter" || ev.key === " ")) {
          ev.preventDefault();
          onOpen(e.path);
        }
      }}
    >
      <div className="hline">
        <span className="hkind">{KIND_LABEL[kind] || kind}</span>
        <span className="hpath">{e.path}</span>
        <span className="htime">{new Date(e.time).toLocaleString()}</span>
      </div>
      <div className="hmeta">
        <span className="hwho">{who}</span>
        <span className="hdev">{dev}</span>
        <span className="hsize">{e.size ? humanSize(e.size) : ""}</span>
      </div>
      {e.note && (
        <div
          className={"hnote" + (noteOpen ? " open" : "")}
          tabIndex={0}
          role="button"
          title={noteOpen ? "Collapse note" : "Show full note"}
          aria-expanded={noteOpen}
          onClick={(ev) => {
            ev.stopPropagation(); // expanding a note is not a navigation
            if ((ev.target as HTMLElement).tagName === "A") return;
            setNoteOpen(!noteOpen);
          }}
          onKeyDown={(ev) => {
            if (ev.key === "Enter" || ev.key === " ") {
              ev.preventDefault();
              ev.stopPropagation();
              setNoteOpen(!noteOpen);
            }
          }}
        >
          {/* Linkify http(s) URLs (e.g. a Claude session link); everything
              else stays plain text — notes are user/agent input, never
              markup. */}
          {e.note.split(/(https?:\/\/\S+)/).map((tok, i) =>
            /^https?:\/\//.test(tok) ? (
              <a key={i} href={tok} target="_blank" rel="noopener">
                {tok}
              </a>
            ) : (
              tok
            ),
          )}
        </div>
      )}
    </div>
  );
}
