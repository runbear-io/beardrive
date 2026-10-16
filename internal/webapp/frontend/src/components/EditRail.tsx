import { useMemo, useState } from "react";
import { useQuery, useQueryClient } from "@tanstack/react-query";
import { getJSON, postJSON } from "../api/http";
import type { HistoryEntry } from "../api/types";
import { actionItems, itemsMarkdown, sameName, sections } from "../lib/meeting";
import { copyText } from "../util";
import { toast } from "../toast";
import { useWideRail } from "./FileRail";
import { Avatar } from "./PresenceBar";

/* The right rail while a markdown file is being edited: who has written their
   section (meeting notes only), the action items in the text, and the file's
   recent versions with a one-click restore.

   Everything is read from the live text and the history API. Restoring posts
   to the restore route, which journals the old blob as a NEW version — the
   overwritten one stays in history and can itself be restored. */

const VERSIONS = 6;

export function EditRail(props: {
  apiBase: string;
  path: string;
  text: string;
  meeting: boolean;
  // Who is in the room, by display name.
  editors: string[];
  me: string;
  // The section a remote cursor is typing in, by heading name.
  typing: Set<string>;
  // History and restore are per-project hub routes; a single-volume server
  // has neither, so it gets no versions list.
  versions: boolean;
  // A restore landed: the file now holds that version, and the open document
  // must be brought to it.
  onRestored?: (folded: boolean) => Promise<void>;
}) {
  const { apiBase, path, text, meeting } = props;
  const wide = useWideRail();
  const [open, setOpen] = useState(false);
  const items = useMemo(() => actionItems(text), [text]);
  const secs = useMemo(() => (meeting ? sections(text) : []), [text, meeting]);

  const body = (
    <>
      {meeting && secs.length > 0 && <Attendees {...props} secs={secs} />}
      <section className="fv-sec er-items" aria-label="Action items">
        <h4>
          Action items · {items.filter((i) => !i.done).length}
          {items.length > 0 && (
            <button
              type="button"
              className="fv-sec-link er-copy"
              onClick={async () => {
                const ok = await copyText(itemsMarkdown(items));
                toast(ok ? "Copied — paste into Linear or any tracker." : "Could not copy.", !ok);
              }}
            >
              Copy as markdown
            </button>
          )}
        </h4>
        {items.length === 0 ? (
          <p className="er-empty">
            None yet. Type <kbd>/</kbd> and pick Action item, or write <code>- [ ] … @owner</code>.
          </p>
        ) : (
          <ul className="er-list">
            {items.map((a) => (
              <li key={a.line} className={a.done ? "done" : ""} data-owner={a.owner}>
                <span className="er-box" aria-hidden="true">
                  {a.done ? "✓" : ""}
                </span>
                <span className="er-text">
                  {a.text || "(untitled)"}
                  <small>
                    {a.owner ? "@" + a.owner : "no owner"}
                    {a.section ? " · " + a.section : ""}
                  </small>
                </span>
              </li>
            ))}
          </ul>
        )}
      </section>
      {props.versions && <Versions apiBase={apiBase} path={path} onRestored={props.onRestored} size={new TextEncoder().encode(text).length} />}
    </>
  );

  if (wide) {
    return (
      <aside className="fv-rail er-rail" data-bd-ignore="" aria-label="About this note">
        <div className="fv-rail-in">{body}</div>
      </aside>
    );
  }
  return (
    <details className="fv-rail narrow er-rail" data-bd-ignore="" open={open}>
      <summary
        onClick={(e) => {
          e.preventDefault();
          setOpen(!open);
        }}
      >
        Action items, versions
      </summary>
      <div className="fv-rail-in">{body}</div>
    </details>
  );
}

function Attendees(props: {
  secs: ReturnType<typeof sections>;
  editors: string[];
  me: string;
  typing: Set<string>;
}) {
  // Discussion-style headings are not people; a section counts as an
  // attendee when its heading is a short name.
  const people = props.secs.filter((s) => s.name.split(/\s+/).length <= 3 && !/discussion|agenda|notes?$/i.test(s.name));
  const done = people.filter((s) => s.body).length;
  return (
    <section className="fv-sec er-att" aria-label="Attendees">
      <h4>
        Attendees · {done} of {people.length}
      </h4>
      <div className="er-bar" role="img" aria-label={`${done} of ${people.length} have written`}>
        <span style={{ width: people.length ? (done / people.length) * 100 + "%" : 0 }} />
      </div>
      <ul className="er-people">
        {people.map((s) => {
          const mine = sameName(s.name, props.me);
          const typing = props.typing.has(s.name);
          const here = props.editors.some((e) => sameName(s.name, e));
          return (
            <li key={s.name + s.from}>
              <Avatar name={s.name} />
              <span className="er-name">{s.name}</span>
              <span className={"er-state " + (typing ? "typing" : s.body ? "done" : "none")}>
                {mine && here ? "You · editing" : typing ? "Typing…" : s.body ? "Done" : "Not started"}
              </span>
            </li>
          );
        })}
      </ul>
    </section>
  );
}

function Versions({
  apiBase,
  path,
  size,
  onRestored,
}: {
  apiBase: string;
  path: string;
  size: number;
  onRestored?: (folded: boolean) => Promise<void>;
}) {
  const qc = useQueryClient();
  const [busy, setBusy] = useState("");
  const { data } = useQuery({
    queryKey: ["history", apiBase, "edit-rail", path],
    queryFn: () =>
      getJSON<{ entries: HistoryEntry[] }>(apiBase + "history?path=" + encodeURIComponent(path) + "&n=" + (VERSIONS + 1)),
  });
  const entries = (data?.entries ?? []).filter((e) => e.path === path && e.blob && e.kind !== "delete");
  // The newest version is the file as it stands; everything after it is a
  // version the file has moved past and can be put back.
  const head = entries[0]?.blob;
  const restore = async (e: HistoryEntry) => {
    // One click, no confirmation: a restore is a new version on top, so it
    // is undone the same way it was done.
    const when = new Date(e.time).toLocaleString();
    setBusy(e.blob!);
    try {
      const out = await postJSON<{ folded?: boolean }>(apiBase + "restore", { path, sha: e.blob });
      toast("Restored the version from " + when + ".");
      void qc.invalidateQueries({ queryKey: ["history", apiBase] });
      await onRestored?.(!!out?.folded);
    } catch (err) {
      toast("Could not restore: " + (err as Error).message, true);
    } finally {
      setBusy("");
    }
  };
  return (
    <section className="fv-sec er-versions" aria-label="Recent versions">
      <h4>Recent versions</h4>
      <ul className="er-vlist">
        <li className="now">
          <i className="er-dot live" aria-hidden="true" />
          <span className="er-vtime">Now</span>
          <span className="er-vwho">Live</span>
          <span className="er-vsize">{size.toLocaleString()} B</span>
        </li>
        {entries.slice(0, VERSIONS).map((e) => {
          const empty = (e.size ?? 0) === 0;
          return (
            <li key={e.time + e.blob} className={empty ? "empty" : ""} data-sha={e.blob}>
              <i className={"er-dot" + (empty ? " bad" : "")} aria-hidden="true" />
              <span className="er-vtime" title={new Date(e.time).toLocaleString()}>
                {new Date(e.time).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}
              </span>
              <span className="er-vwho">{e.user_name || e.user || e.author || e.device?.name || ""}</span>
              <span className="er-vsize">{(e.size ?? 0).toLocaleString()} B</span>
              {e.blob !== head && (
                <button
                  type="button"
                  className="er-restore"
                  disabled={!!busy}
                  title="Restore this version as a new version"
                  onClick={() => void restore(e)}
                >
                  {busy === e.blob ? "Restoring…" : "Restore"}
                </button>
              )}
            </li>
          );
        })}
      </ul>
      <p className="er-note">Restoring creates a new version — nothing is deleted.</p>
    </section>
  );
}
