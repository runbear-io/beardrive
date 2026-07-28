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

// Putting an old version back. Absent when the viewer can't write (or this
// isn't a hub project), in which case no Restore button is drawn at all —
// better than one that 403s.
export type RestoreAction = {
  onRestore: (path: string, sha: string) => void;
  busy?: string; // path+sha currently in flight
};

export function HistoryRow({
  entry: e,
  onOpen,
  diff,
  restore,
  restoreSha,
  inRun,
}: {
  entry: HistoryEntry;
  // The row's own version (e.blob) rides along: a row is an address for the
  // bytes it describes, not a shortcut to whatever the file says now.
  onOpen: (path: string, version?: string) => void;
  // Present only in the per-file history view, where "the previous version"
  // is unambiguous. `prev` is the sha of the entry before this one on the
  // same path; absent means this is the first version.
  diff?: { apiBase: string; prev?: string };
  restore?: RestoreAction;
  // The version this row puts back: its own bytes, or — for a delete — the
  // content it removed. The view computes it, since it needs the whole feed.
  restoreSha?: string;
  // Inside a run card, where "this run created the file" is a statement we
  // can actually make.
  inRun?: boolean;
}) {
  const [noteOpen, setNoteOpen] = useState(false);
  const [diffOpen, setDiffOpen] = useState(false);
  const kind = e.kind === "put" ? "edit" : e.kind; // older servers report raw "put" ops
  const who = whoChanged(e);
  const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
  const clickable = kind !== "delete";
  // A delete has no content, and a first version has nothing behind it.
  const diffable = !!diff && kind !== "delete" && !!e.blob;
  // Inside a run card an "add" is a file the run CREATED: putting its bytes
  // back would be a no-op, and removing it is the thing we can't do yet — so
  // that row explains itself instead of offering a button. Everywhere else
  // (the per-file version list) a first version is very much restorable —
  // it is usually the version someone wants back.
  const createdByRun = !!inRun && kind === "add";
  const restorable = !!restore && !!restoreSha && !createdByRun;
  const busy = !!restore?.busy && restore.busy === e.path + restoreSha;
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
        <span className="htime">{new Date(e.time).toLocaleString()}</span>
      </div>
      <div className="hmeta">
        <span className="hwho">{who}</span>
        <span className="hdev">{dev}</span>
        <span className="hsize">{e.size ? humanSize(e.size) : ""}</span>
        {/* Restoring is not navigating, so the click stops here — same rule
            the note and diff controls follow. */}
        {restorable && (
          <button
            type="button"
            className="hrestore-btn"
            disabled={busy}
            title={"Put this version of " + e.path + " back as a new change"}
            onClick={(ev) => {
              ev.stopPropagation();
              restore!.onRestore(e.path, restoreSha!);
            }}
            onKeyDown={(ev) => ev.stopPropagation()}
          >
            <Icon name="hist" />
            {busy ? "restoring…" : "restore"}
          </button>
        )}
        {/* Never a missing button with no explanation: nothing in the hub
            writes a delete op yet, so a file a run created can't be
            un-created. Say so where the button would have been. */}
        {restore && createdByRun && (
          <span className="hrestore-gap" title="Restore puts old content back; it can't remove a file yet.">
            created by this run — can't be undone yet
          </span>
        )}
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
