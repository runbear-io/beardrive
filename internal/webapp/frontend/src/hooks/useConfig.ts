import { useQuery } from "@tanstack/react-query";
import { getJSON } from "../api/http";
import { initAnalytics } from "../analytics";
import type { ServerConfig } from "../api/types";

// The first request the app makes; everything else keys off its answer.
// If auth is on and there is no session, redirect to the login page here
// rather than letting every authed call 401 (noisy console, and the
// redirect happens anyway). /api/config reports `me` only when signed in.
export function useConfig() {
  return useQuery({
    queryKey: ["config"],
    queryFn: async () => {
      const cfg = await getJSON<ServerConfig>("/api/config");
      if (cfg.auth.enabled && !cfg.me) {
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
