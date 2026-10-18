import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import { initAnalytics } from "../analytics";
import type { ServerConfig } from "../api/types";

// The request started by startConfig, consumed by the first query run.
let early: Promise<ServerConfig> | null = null;

// startConfig sends /api/config before anything else on the page asks for
// anything, so it is first in line for a connection: the app renders nothing
// until it answers. The query below still decides what the answer means.
export function startConfig() {
  early ??= getJSON<ServerConfig>("/api/config");
}

// The first request the app makes; everything else keys off its answer.
// If auth is on and there is no session, redirect to the login page here
// rather than letting every authed call 401 (noisy console, and the
// redirect happens anyway). /api/config reports `me` only when signed in.
export function useConfig() {
  return useQuery({
    queryKey: ["config"],
    queryFn: async () => {
      const pending = early;
      early = null;
      const cfg = await (pending ?? getJSON<ServerConfig>("/api/config"));
      if (cfg.auth.enabled && !cfg.me) {
        /* Same loop guard as toLogin, and for the same reason: /auth/login
           will happily send a signed-in user straight back here, so if the
           hub says "no session" twice running, bouncing again just reloads
           the page forever. Throw instead — the app shows the error and the
           user can act on it. */
        const KEY = "bdrive:login-bounce";
        let bounced = 0;
        try {
          bounced = Number(sessionStorage.getItem(KEY) || 0);
        } catch {
          /* no storage: behave as before */
        }
        if (bounced && Date.now() - bounced < 30_000) {
          throw new Error(
            "The server does not recognise your session, and signing in again " +
              "did not help. It is probably having a bad moment — wait a few " +
              "seconds and reload.",
          );
        }
        try {
          sessionStorage.setItem(KEY, String(Date.now()));
        } catch {
          /* no storage: behave as before */
        }
        location.href =
          "/auth/login?next=" +
          encodeURIComponent(location.pathname + location.search);
        await new Promise(() => {}); // never resolve; we're navigating away
      }
      // The one place the config lands, and it lands once (staleTime
      // Infinity) — no effect needed, and identify() gets the user in the
      // same breath. A no-op unless the server configured analytics.
      initAnalytics(cfg);
      return cfg;
    },
    staleTime: Infinity,
  });
}
