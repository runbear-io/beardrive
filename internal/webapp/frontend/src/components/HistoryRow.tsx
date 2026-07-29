import { useState } from "react";
import type { HistoryEntry } from "../api/types";
import { humanSize, whoChanged } from "../util";
import { Icon } from "./shell";
import { DiffView } from "./DiffView";

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
  apiBase,
  onOpen,
  diff,
}: {
  entry: HistoryEntry;
  // Its own prop, not something nested in `diff`: the version controls below
  // belong on every feed, and `diff` is per-file-only by design.
  apiBase: string;
  // The row's own version (e.blob) rides along: a row is an address for the
  // bytes it describes, not a shortcut to whatever the file says now.
  onOpen: (path: string, version?: string) => void;
  // Present only in the per-file history view, where "the previous version"
  // is unambiguous. `prev` is the sha of the entry before this one on the
  // same path; absent means this is the first version.
  diff?: { apiBase: string; prev?: string };
}) {
  const [noteOpen, setNoteOpen] = useState(false);
  const [diffOpen, setDiffOpen] = useState(false);
  const kind = e.kind === "put" ? "edit" : e.kind; // older servers report raw "put" ops
  const who = whoChanged(e);
  const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
  const clickable = kind !== "delete";
  // A delete has no content, and a first version has nothing behind it.
  const diffable = !!diff && kind !== "delete" && !!e.blob;
  // The row already *is* a link to its version — but a bare div says so to
  // nobody, so the version gets visible handles too (BEA-26). Gated on
  // content, never on `diff`, or the subtree and folder feeds lose them.
  const versioned = clickable && !!e.blob;
  const base = e.path.split("/").pop() || e.path;
  const when = new Date(e.time).toLocaleString();
  const dl = apiBase + "blob?sha=" + e.blob + "&name=" + encodeURIComponent(base) + "&download=1";
  const toggleDiff = () => setDiffOpen(!diffOpen);
  const open = (ev: React.MouseEvent | React.KeyboardEvent) => {
    if ((ev.target as HTMLElement).tagName === "A") return;
    if (clickable) onOpen(e.path, e.blob);
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
          onOpen(e.path, e.blob);
        }
      }}
    >
      <div className="hline">
        <span className="hkind">{KIND_LABEL[kind] || kind}</span>
        <span className="hpath">{e.path}</span>
        <span className="htime">{when}</span>
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
      {/* Each control is its own: acting on a row must not double as
          navigating it, so every one stops the click reaching the row.
          None of them claims aria-expanded — only the note and the diff
          disclosure actually expand anything. */}
      {(diffable || versioned) && (
        <div className="hactions">
          {diffable &&
            (diff!.prev ? (
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
            ) : (
              <div className="hdiff-none">First version — nothing to compare against</div>
            ))}
          {versioned && (
            <>
              <button
                type="button"
                className="hver-btn"
                aria-label={`Open ${base} as of ${when}`}
                onClick={(ev) => {
                  ev.stopPropagation();
                  onOpen(e.path, e.blob);
                }}
                onKeyDown={(ev) => ev.stopPropagation()}
              >
                <Icon name="clock" />
                Open this version
              </button>
              <a
                className="hver-btn"
                download
                href={dl}
                aria-label={`Download ${base} as of ${when}`}
                onClick={(ev) => ev.stopPropagation()}
                onKeyDown={(ev) => {
                  ev.stopPropagation();
                  // Enter follows a link natively; Space does not, and the
                  // row's own handler must not get it either way.
                  if (ev.key === " ") {
                    ev.preventDefault();
                    ev.currentTarget.click();
                  }
                }}
              >
                <Icon name="download" />
                Download
              </a>
            </>
          )}
        </div>
      )}
      {diffable && diff!.prev && diffOpen && (
        <div onClick={(ev) => ev.stopPropagation()}>
          <DiffView apiBase={diff!.apiBase} path={e.path} prev={diff!.prev} cur={e.blob!} />
        </div>
      )}
    </div>
  );
}
