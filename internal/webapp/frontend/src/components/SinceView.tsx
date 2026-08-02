import { useCallback, useRef, useState } from "react";
import { HistoryView } from "./HistoryView";
import type { RemoveAction, RestoreAction } from "./HistoryRow";
import { lastVisit, stampVisit } from "../lib/lastVisit";

/* ---- what's new ----
   The change feed, cut to what landed since you were last here. Everything
   below the header is HistoryView unchanged — same run cards, same rows,
   same diffs — so this file is only the anchor: read the marker once, show
   what it means, then move it.

   Two things are the whole reason this wrapper exists:

   * the baseline comes from a useState INITIALIZER, so it is frozen for the
     life of the mount. Read it during render or in an effect and the page
     empties itself while you are reading it — it would re-read the marker it
     just wrote;
   * stamping is behind a ref guard, so "Load more" cannot restamp. Harmless
     either way (the baseline is frozen) but the invariant should be visible.

   This is the only call site of stampVisit: no other project page moves the
   marker, which is what makes "what's new" mean anything. */
export function SinceView(props: {
  apiBase: string;
  projectId: string;
  account?: string;
  isFolder: (p: string) => boolean;
  onOpen: (path: string, version?: string) => void;
  onMeta: (meta: string) => void;
  onRendered?: () => void;
  restore?: RestoreAction;
  remove?: RemoveAction;
}) {
  const { projectId, account } = props;
  const [base] = useState(() => lastVisit(projectId, account));
  const [loaded, setLoaded] = useState<{ n: number; more: boolean } | null>(null);
  const stamped = useRef(false);

  const onLoaded = useCallback(
    (n: number, more: boolean) => {
      setLoaded({ n, more });
      if (stamped.current) return;
      stamped.current = true;
      stampVisit(projectId, account);
    },
    [projectId, account],
  );

  const when = new Date(base.since).toLocaleString([], {
    month: "short",
    day: "numeric",
    hour: "2-digit",
    minute: "2-digit",
  });
  let sub: string;
  if (!loaded) sub = "Looking for changes…";
  else if (loaded.n === 0) sub = "Nothing new since your last visit · " + when;
  else {
    const n = loaded.n + (loaded.more ? "+" : "");
    sub = `${n} change${loaded.n === 1 && !loaded.more ? "" : "s"} since your last visit · ${when}`;
  }

  return (
    <div className="since">
      <h1 className="since-head">What's new</h1>
      <div className="since-sub">
        {sub}
        {base.first && (
          <span className="since-first"> — showing the last 7 days; we'll remember this visit.</span>
        )}
      </div>
      <HistoryView
        apiBase={props.apiBase}
        target=""
        isFolder={props.isFolder}
        onOpen={props.onOpen}
        onMeta={props.onMeta}
        onRendered={props.onRendered}
        restore={props.restore}
        remove={props.remove}
        since={base.since}
        emptyText="" /* the subline above already says it */
        onLoaded={onLoaded}
      />
    </div>
  );
}
