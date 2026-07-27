import { useState } from "react";
import type { HistoryEntry } from "../api/types";
import { humanSize } from "../util";
import { Icon } from "./shell";
import { DiffView } from "./DiffView";

/* One change as a row: what happened (added / edited / deleted), to which
   file, by whom, from where — with the note (session link) expandable. */
const KIND_ICON: Record<string, string> = { add: "plus", edit: "edit", delete: "x" };
const KIND_LABEL: Record<string, string> = { add: "added", edit: "edited", delete: "deleted" };

export function HistoryRow({
  entry: e,
  onOpen,
  diff,
}: {
  entry: HistoryEntry;
  onOpen: (path: string) => void;
  // Present only in the per-file history view, where "the previous version"
  // is unambiguous. `prev` is the sha of the entry before this one on the
  // same path; absent means this is the first version.
  diff?: { apiBase: string; prev?: string };
}) {
  const [noteOpen, setNoteOpen] = useState(false);
  const [diffOpen, setDiffOpen] = useState(false);
  const kind = e.kind === "put" ? "edit" : e.kind; // older servers report raw "put" ops
  const who = e.user_name ? `${e.user_name} <${e.user}>` : e.user || e.author || "unknown";
  const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
  const clickable = kind !== "delete";
  // A delete has no content, and a first version has nothing behind it.
  const diffable = !!diff && kind !== "delete" && !!e.blob;
  const toggleDiff = () => setDiffOpen(!diffOpen);
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
        <span className="hkind">
          <Icon name={KIND_ICON[kind] || "dot"} />
        </span>
        <span className="hpath">{e.path}</span>
        <span className="htag">{KIND_LABEL[kind] || kind}</span>
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
      {/* Its own control, never the kind glyph: expanding a row must not
          navigate, so this stops the click from reaching the row. */}
      {diffable &&
        (diff!.prev ? (
          <>
            <button
              type="button"
              className={"hdiff-btn" + (diffOpen ? " open" : "")}
              aria-expanded={diffOpen}
              onClick={(ev) => {
                ev.stopPropagation();
                toggleDiff();
              }}
              onKeyDown={(ev) => ev.stopPropagation()}
            >
              <Icon name={diffOpen ? "chevd" : "chev"} />
              {diffOpen ? "hide changes" : "show changes"}
            </button>
            {diffOpen && (
              <div onClick={(ev) => ev.stopPropagation()}>
                <DiffView apiBase={diff!.apiBase} path={e.path} prev={diff!.prev} cur={e.blob!} />
              </div>
            )}
          </>
        ) : (
          <div className="hdiff-none">First version — nothing to compare against</div>
        ))}
    </div>
  );
}
