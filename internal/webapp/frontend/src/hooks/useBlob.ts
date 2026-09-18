import { useQuery } from "@tanstack/react-query";
import { getResponse, HttpError } from "../api/http";
import { MAX_BYTES, sniffBytes, type BlobText } from "../lib/sniff";

// One URL's bytes, decoded as text when they are text. The sniff itself
// lives in lib/sniff.ts (pure, so the unit test can import it without
// dragging in React Query); what stays here is the HTTP part — the
// Content-Length cheap-out — and the caching policy.

export type { BlobText };

export function blobURL(apiBase: string, sha: string, name?: string, download?: boolean): string {
  let u = apiBase + "blob?sha=" + encodeURIComponent(sha);
  if (name) u += "&name=" + encodeURIComponent(name);
  if (download) u += "&download=1";
  return u;
}

export async function fetchBlobText(url: string): Promise<BlobText> {
  const r = await getResponse(url);
  // Cheap out before reading the body when the server tells us the size.
  // Content-Length is a hint, not a guarantee (a chunked or proxied
  // response may omit it), so sniffBytes checks the real length too.
  // A compressed response has no Content-Length at all — it describes bytes
  // on the wire, and the hub moves the plaintext size aside under its own
  // name so this check survives compression (webapp/compress.go).
  const len = Number(
    r.headers.get("Content-Length") ?? r.headers.get("X-Uncompressed-Length"),
  );
  if (len > MAX_BYTES) return { kind: "too-large", size: len };
  return sniffBytes(new Uint8Array(await r.arrayBuffer()));
}

// The URL a file page reads its bytes from: content-addressed when a version
// is pinned, the live path otherwise. Exported so Copy builds the same URL
// the view does instead of a second copy of the expression that can drift.
// A version is served by content hash; ?name= is what makes the server set a
// real Content-Type, so images and text render instead of downloading as
// octet-stream. Note `file?path=`, not the `download?path=` an <a download>
// points at — same bytes, but Content-Disposition is meaningless to a fetch.
export function fileURLFor(apiBase: string, path: string, version?: string): string {
  return version ? blobURL(apiBase, version, path) : apiBase + "file?path=" + encodeURIComponent(path);
}

// `immutable` is for content-addressed URLs: a sha's bytes never change, so
// staleness cannot apply and re-expanding a history row costs no request. A
// live path must never be pinned that way — a teammate's edit would keep
// serving the old body.
/* Worth another try, or an answer?

   429 and 5xx are the server saying "not now"; a dropped connection is not an
   answer at all. 403 and 404 ARE answers, and retrying them just spends
   requests on a hub that is already telling us to slow down. */
function transient(e: unknown): boolean {
  return !(e instanceof HttpError) || e.status === 429 || e.status >= 500;
}

export function useTextAt(url: string, key: unknown[], enabled: boolean, immutable: boolean) {
  return useQuery({
    queryKey: key,
    queryFn: () => fetchBlobText(url),
    enabled,
    ...(immutable ? { staleTime: Infinity, gcTime: Infinity } : {}),
    // A pinned ?v= URL is content-addressed: if it failed it failed, and
    // there is no editor open on it to protect. A LIVE path is different —
    // one 429 used to end an editing session (see EditView), so it backs off
    // and tries again rather than turning into a dead end.
    retry: immutable ? false : (count, e) => count < 3 && transient(e),
    retryDelay: (count) => Math.min(1_000 * 2 ** count, 8_000),
  });
}

export function useBlobText(apiBase: string, sha: string | undefined, enabled: boolean) {
  return useTextAt(sha ? blobURL(apiBase, sha) : "", ["blob", apiBase, sha], enabled && !!sha, true);
}
