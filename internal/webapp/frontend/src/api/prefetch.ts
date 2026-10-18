import type { QueryClient } from "@tanstack/react-query";
import { parseRoute } from "../router";
import { MD_EXT } from "../util";
import { bootRequests } from "./bootRequests";

/* A shared link is a cold page load, and each request the page needs used to
   wait for the one before it: config, then the project list and orgs, then
   the tree, then the document. The URL already names the project and the
   file, so the requests that depend only on the URL start here, before React
   mounts, under the query keys the hooks use. A hook that mounts later joins
   the request in flight instead of starting its own.

   Config is not among them: its query function is what sends a signed-out
   visitor to the login page, and answering it from here would skip that.

   Nothing here can navigate. A failed request is left in the cache as an
   error, which a mounting hook refetches through getJSON, so the sign-in
   redirect and its loop guard stay where they are. */

async function quietJSON<T>(url: string): Promise<T> {
  const r = await fetch(url, { headers: { Accept: "application/json" } });
  if (!r.ok) throw new Error(url + ": " + r.status);
  return r.json();
}

export function prefetchForURL(qc: QueryClient, url: string) {
  for (const { queryKey, url: u } of bootRequests(parseRoute(url, "hub"), (p) => MD_EXT.test(p))) {
    qc.prefetchQuery({ queryKey, queryFn: () => quietJSON(u), retry: false });
  }
}
