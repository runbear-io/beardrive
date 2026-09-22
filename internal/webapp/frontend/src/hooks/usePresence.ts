import { useEffect, useRef, useState } from "react";
import { postJSON } from "../api/http";

/* Who else is looking at this project.

   This used to be a 10-second heartbeat, per TAB. Unlike the change stream —
   which is leader-elected, one per browser — six open tabs meant six POSTs
   every ten seconds, forever, from somebody who was reading. In production it
   was ~19% of every request the hub served, and one idle tab spent 8,640
   requests a day saying it still existed (docs/hub-load-prd.md).

   None of that was news. A member holding an open change stream is here by
   definition: the hub has their connection in its hands and tracks presence
   against it (webapp/presence.go, streamGone). So the timer is gone. What is
   left is an uplink when the answer actually changes — arriving, and moving to
   another file — which is an event, not a clock.

   The hub keeps its 15s TTL as a backstop for clients that report presence
   without holding a stream. Every browser here holds one, including non-leader
   tabs: liveness is keyed on the ACCOUNT, and the leader's stream vouches for
   every tab of that account.

   The roster ARRIVES on the SSE stream (see useProjectEvents, which routes
   "presence" frames here through onRoster). The POST's own response is used
   only for the first paint, so a tab that opens into a quiet project still
   sees who is there without waiting for someone else to move. */

export type Person = { name: string; path?: string };

export function usePresence(apiBase: string, path: string, enabled = true) {
  const [people, setPeople] = useState<Person[]>([]);
  // Read through a ref by the unmount path, which must report the path this
  // tab was actually on rather than whatever it was when the effect ran.
  const pathRef = useRef(path);
  pathRef.current = path;

  useEffect(() => {
    if (!enabled) return;
    let live = true;
    const announce = async (leave = false) => {
      try {
        const out = await postJSON<{ people: Person[] }>(apiBase + "presence", {
          path: pathRef.current,
          ...(leave ? { leave: true } : {}),
        });
        if (live && !leave) setPeople(out.people ?? []);
      } catch {
        // A hub too old to know the route, or a blip. Presence is decoration:
        // it must never surface an error or retry loudly. There is no retry
        // here on purpose — the next navigation is the next attempt, and a
        // failed announcement costs a roster row, not correctness.
      }
    };
    // `path` is a dependency now rather than a ref read by a timer: a
    // navigation IS the reason to send one of these, so re-running the effect
    // is the whole mechanism rather than something to avoid.
    void announce();
    return () => {
      live = false;
    };
  }, [apiBase, enabled, path]);

  /* Leaving is its own effect, and deliberately does NOT depend on `path`.

     Folded into the one above it would fire on every navigation — a "leave"
     racing the "arrive" that follows it, which drops you off your own
     teammates' rosters whenever the two land out of order. This one runs when
     the project view really goes away. */
  useEffect(() => {
    if (!enabled) return;
    return () => {
      // Fire and forget: the hub drops you when your last stream ends either
      // way (streamGone), so this only buys promptness.
      void postJSON(apiBase + "presence", {
        path: pathRef.current,
        leave: true,
      }).catch(() => {});
    };
  }, [apiBase, enabled]);

  return { people, setPeople };
}
