import { useEffect, useRef } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { fileURLFor } from "./useBlob";

/* Live change notification.
   The hub streams "these paths changed" over server-sent events, so a
   teammate's edit lands in about a second — and an OPEN file updates at all,
   which it previously never did: file content has no refetch interval (see
   useBlob), so a body fetched once stayed on screen until the reader
   navigated away.

   This IS load-bearing now, which it was not when it was written. It used to
   be an accelerator on top of a 15s tree poll, a 60s heat poll and a 30s
   project-list poll, all of which would have covered for it. Those are gone
   (docs/network-efficiency-prd.md): they re-sent the whole project four times
   a minute to say nothing had changed. What remains underneath is a 5-minute
   tree refetch, which is insurance against a stream that died quietly rather
   than a second source of truth.

   ONE stream per browser, not per tab. Every tab of a project receives the
   identical fan-out, so the second tab onward used to buy nothing and cost a
   slot at both ends: a permanently in-flight request against the hub's finite
   per-instance concurrency, and one of the browser's ~6 per-origin HTTP/1.1
   sockets — six tabs wedged the whole app, streams that never finish being
   the worst possible thing to spend that pool on. So one tab holds the
   EventSource and relays each frame verbatim to the others, which handle it
   exactly as if they had read it off the wire themselves.

   Leader election is a Web Lock, held for the leader's lifetime. The browser
   hands it to the next waiter when that tab dies — including a crash or a
   force-quit, which is the case a heartbeat-and-TTL scheme gets wrong. There
   is no stale leader to detect and no timeout to tune.

   Frames published in the gap between a leader dying and its successor
   connecting are lost. That is the poll's job, as it was before any of this
   existed. Where either API is missing, every tab opens its own stream and
   behaves exactly as it did before.

   Deliberately NOT shared: /collab. Two tabs editing one document are two
   distinct CRDT peers with their own awareness state — sharing that stream
   would be wrong, not thrifty. */

type ChangeEvent = {
  type: "change" | "resync" | "presence" | "scope";
  paths?: string[];
  more?: boolean;
  people?: { name: string; path?: string }[];
};

export function useProjectEvents(
  apiBase: string,
  enabled = true,
  // Presence rides the same stream rather than a second connection: it is the
  // same fan-out, the same permission, and one EventSource per project is
  // already one more than zero.
  onPresence?: (people: { name: string; path?: string }[]) => void,
) {
  const qc = useQueryClient();
  // Read through a ref so a caller passing an inline arrow does not tear the
  // stream down and rebuild it on every render.
  const onPresenceRef = useRef(onPresence);
  onPresenceRef.current = onPresence;

  useEffect(() => {
    if (!enabled || typeof EventSource === "undefined") return;

  /* What a change frame actually invalidates, and what it does not.

     HEAT IS NOT IN HERE. Heat is READ telemetry — it moves when somebody
     opens a file, which no write can tell us — so refreshing it on every
     write re-sent the whole map (133 KB on a real project) to say exactly
     what it said before. The surfaces that show it refresh it when they open
     (Browser.tsx) and it carries its own staleTime.

     HISTORY IS. It is genuinely derived from the same journal, and it costs
     nothing when no history view is mounted: invalidateQueries refetches
     ACTIVE queries and only marks inactive ones stale.

     THE TREE IS, BUT ON A DELAY. It is the single biggest response the hub
     serves — 1.65 MB raw, ~148 KB compressed, for 5,700 nodes — and a sync
     push or a multi-file agent run announces itself as a burst of frames.
     Coalescing them costs a couple of seconds of staleness in a listing and
     saves that payload N-1 times.

     Per-path bodies are NOT delayed: an open file updating is the thing a
     reader actually notices, and one body is small. */
    const COALESCE_MS = 2_000;
    let coalesce: ReturnType<typeof setTimeout> | null = null;
    const invalidateProject = () => {
      if (coalesce) return;
      coalesce = setTimeout(() => {
        coalesce = null;
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        qc.invalidateQueries({ queryKey: ["history", apiBase] });
      }, COALESCE_MS);
    };

    // One frame, from the socket this tab owns or from the tab that owns it.
    // Identical either way — which is the point, and why the relay ships the
    // raw `data` string rather than anything it has already interpreted.
    const handle = (data: string) => {
      let ev: ChangeEvent;
      try {
        ev = JSON.parse(data);
      } catch {
        return; // a frame we can't read is not a reason to tear the stream down
      }
      // Presence is not a file change: it invalidates nothing.
      if (ev.type === "presence") {
        onPresenceRef.current?.(ev.people ?? []);
        return;
      }
      /* Scope moved: someone changed this project's permissions or a folder
         rule. Nothing was written, so this must NOT take the generic path
         below — that dispatches "a peer wrote your file" at an open editor,
         and no peer wrote anything.

         What it does change is what this account can SEE, so the listing and
         the permission views are genuinely stale. Bodies go too: a file that
         just became invisible should stop rendering rather than sit there
         from cache. The frame deliberately carries no prefix (see
         publishScope) — the hub answers per reader, so the honest move is to
         re-ask rather than guess which subtree moved. */
      if (ev.type === "scope") {
        qc.invalidateQueries({ queryKey: ["tree", apiBase] });
        // These two are keyed on the project id, not apiBase (useHub.ts), and
        // they are small and usually unmounted — invalidateQueries only marks
        // an inactive query stale — so the bare prefix is the cheap, correct
        // key rather than a second way to spell the project.
        qc.invalidateQueries({ queryKey: ["folders"] });
        qc.invalidateQueries({ queryKey: ["permissions"] });
        qc.invalidateQueries({ queryKey: ["render", apiBase] });
        qc.invalidateQueries({ queryKey: ["text"] });
        return;
      }
      // An open editor must know a peer wrote its file, but must NOT be
      // re-seeded from the server: that would reset the buffer under the
      // typist's cursor. It listens for this and shows a banner instead.
      // A DOM event rather than a prop chain — the editor is several levels
      // down and this is the only thing it needs from up here.
      window.dispatchEvent(
        new CustomEvent("bdrive:changed", { detail: ev.paths ?? [] }),
      );
      invalidateProject();
      // "resync" means frames were dropped and what was missed is unknowable,
      // and "more" means the frame was truncated. Either way the only honest
      // move is to drop every cached body rather than guess at a path list.
      if (ev.type === "resync" || ev.more || !ev.paths?.length) {
        qc.invalidateQueries({ queryKey: ["render", apiBase] });
        qc.invalidateQueries({ queryKey: ["text"] });
        return;
      }
      for (const p of ev.paths) {
        qc.invalidateQueries({ queryKey: ["render", apiBase, p] });
        /* The BODY of the named path, not every body in the browser.

           This used to be a bare ["text"] prefix — twice per frame — which
           dropped every cached file in every project open in this tab,
           because one path in one of them changed. The live URL is a pure
           function of the path (fileURLFor), so the key is knowable here;
           a ?v= URL is content-addressed, keyed on its sha, and cannot go
           stale at all. */
        qc.invalidateQueries({ queryKey: ["text", fileURLFor(apiBase, p)] });
      }
    };

    // Scoped to the project: tabs on different projects share nothing, and
    // two hubs open in one browser never cross.
    const key = `bdrive:events:${apiBase}`;
    let es: EventSource | null = null;
    let chan: BroadcastChannel | null = null;
    // Resolving this frees the Web Lock, which is what hands leadership on.
    let release: (() => void) | null = null;
    const giveUp = new AbortController();
    let done = false;

    const connect = () => {
      es = new EventSource(apiBase + "events");
      es.onmessage = (e) => {
        // Followers first: a tab that is mid-render should not delay the
        // others. BroadcastChannel never echoes to its own sender, so the
        // leader still has to handle the frame itself.
        chan?.postMessage(e.data);
        handle(e.data);
      };
      // Errors are expected and self-healing: EventSource retries on its own,
      // and the poll covers the gap. Logging here would mean a line per hub
      // restart, per sleeping laptop, forever.
      es.onerror = () => {};
    };

    if (typeof BroadcastChannel === "undefined" || !navigator.locks) {
      connect(); // no way to share; behave exactly as this hook always did
    } else {
      chan = new BroadcastChannel(key);
      chan.onmessage = (e: MessageEvent<string>) => handle(e.data);
      navigator.locks
        .request(`${key}:leader`, { signal: giveUp.signal }, () => {
          // Held until this promise settles, so it resolves only on cleanup.
          return new Promise<void>((resolve) => {
            if (done) return resolve(); // unmounted while the lock was pending
            release = resolve;
            connect();
          });
        })
        // AbortError is the ordinary path for a follower that closes before
        // ever leading. Nothing here is load-bearing enough to report.
        .catch(() => {});
    }

    return () => {
      done = true;
      giveUp.abort(); // drop a still-pending request to lead
      release?.(); // or, if we are leading, pass it to the next tab
      es?.close();
      chan?.close();
    };
  }, [apiBase, enabled, qc]);
}
