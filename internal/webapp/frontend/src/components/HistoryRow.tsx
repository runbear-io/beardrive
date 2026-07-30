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

// Un-creating a file a run added — restore's other half, and the only op the
// hub writes that takes content away, so it always goes through a
// confirmation the caller owns. Same visibility rule as RestoreAction.
export type RemoveAction = {
  onRemove: (path: string) => void;
  busy?: string; // path currently in flight
};

// Linkify http(s) URLs (e.g. a Claude session link); everything else stays
// plain text — notes are user/agent input, never markup. Shared with the run
// card's header, which shows the same note.
export function NoteText({ text }: { text: string }) {
  return (
    <>
      {text.split(/(https?:\/\/\S+)/).map((tok, i) =>
        /^https?:\/\//.test(tok) ? (
          <a key={i} href={tok} target="_blank" rel="noopener">
            {tok}
          </a>
        ) : (
          tok
        ),
      )}
    </>
  );
}

export function HistoryRow({
  entry: e,
  apiBase,
  onOpen,
  diff,
  restore,
  remove,
  restoreSha,
  inRun,
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
  restore?: RestoreAction;
  // Only ever offered on an add inside a run card, where "this run created
  // the file" is a statement we can make.
  remove?: RemoveAction;
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
  // back would be a no-op, so the undo it wants is a removal instead.
  // Everywhere else (the per-file version list) a first version is very much
  // restorable — it is usually the version someone wants back.
  const createdByRun = !!inRun && kind === "add";
  const restorable = !!restore && !!restoreSha && !createdByRun;
  const removable = !!remove && createdByRun;
  const busy = !!restore?.busy && restore.busy === e.path + restoreSha;
  const removing = !!remove?.busy && remove.busy === e.path;
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
        {/* The run created this file, so the undo is a removal, not a
            restore. It reaches every synced device, so onRemove confirms
            before it fires. */}
        {removable && (
          <button
            type="button"
            className="hremove-btn"
            disabled={removing}
            title={"Remove " + e.path + " — this run created it"}
            onClick={(ev) => {
              ev.stopPropagation();
              remove!.onRemove(e.path);
            }}
            onKeyDown={(ev) => ev.stopPropagation()}
          >
            <Icon name="trash" />
            {removing ? "removing…" : "undo — remove file"}
          </button>
        )}
      </div>
      {/* Inside a run card the note is the card's header — repeating it on
          every row says the same thing N times. */}
      {e.note && !inRun && (
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
          <NoteText text={e.note} />
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
