import { queryOptions, useQuery } from "@tanstack/react-query";
import { getJSON, getResponse, HttpError, latestProjectReceipt } from "../api/http";
import { MAX_BYTES, sniffBytes, type BlobText } from "../lib/sniff";

// One URL's bytes, decoded as text when they are text. The sniff itself
// lives in lib/sniff.ts (pure, so the unit test can import it without
// dragging in React Query); what stays here is the HTTP part — the
// Content-Length cheap-out — and the caching policy.

export type { BlobText };

const FILE_SHA = /^[0-9a-f]{64}$/;

export function blobURL(apiBase: string, sha: string, name?: string, download?: boolean): string {
  let u = apiBase + "blob?sha=" + encodeURIComponent(sha);
  if (name) u += "&name=" + encodeURIComponent(name);
  if (download) u += "&download=1";
  return u;
}

// `quiet` is for the boot prefetch, which must never navigate: a refusal is
// a plain error left in the cache, and the mounting hook's own read (not
// quiet) is what sends a signed-out visitor to sign in.
export async function fetchBlobText(url: string, quiet = false): Promise<BlobText> {
  let r: Response;
  if (quiet) {
    r = await fetch(url);
    if (!r.ok) throw new Error(url + ": " + r.status);
  } else {
    r = await getResponse(url);
  }
  // Cheap out before reading the body when the server tells us the size.
  // Content-Length is a hint, not a guarantee (a chunked or proxied
  // response may omit it), so sniffBytes checks the real length too.
  // A compressed response has no Content-Length at all — it describes bytes
  // on the wire, and the hub moves the plaintext size aside under its own
  // name so this check survives compression (webapp/compress.go).
  const len = Number(
    r.headers.get("Content-Length") ?? r.headers.get("X-Uncompressed-Length"),
  );
  // The ETag is the content hash, including for binary and oversized files.
  const etag = r.headers.get("ETag")?.replace(/^W\/|"/g, "");
  if (len > MAX_BYTES) return { kind: "too-large", size: len, sha: etag };
  const out = sniffBytes(new Uint8Array(await r.arrayBuffer()));
  return etag ? { ...out, sha: etag } : out;
}

export async function fetchFileHeadSha(url: string): Promise<{ exists: true; sha: string } | { exists: false }> {
  let r: Response;
  try {
    r = await getResponse(url, { method: "HEAD" });
  } catch (e) {
    if (e instanceof HttpError && e.status === 404) return { exists: false };
    throw e;
  }
  const sha = r.headers.get("ETag")?.replace(/^W\/|"/g, "");
  if (r.status !== 200 || !sha || !FILE_SHA.test(sha)) {
    throw new Error("Could not verify the current version of the target file");
  }
  return { exists: true, sha };
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
export function isTransientBlobError(e: unknown): boolean {
  return !(e instanceof HttpError) || e.status === 429 || e.status >= 500;
}

/* The one query every reader of a file's bytes shares. ["text", url] has a
   single shape — a BlobText — no matter who asks: the reading views, the
   editor's seed, Copy, the boot prefetch and hover prefetches all build it
   here, so no caller can park a raw string under the key another reads as an
   object (React error #31 was exactly that: a `string` and a BlobText
   sharing one cache slot). */
export function textQueryOptions(url: string, immutable = false) {
  return queryOptions({
    queryKey: ["text", url] as const,
    queryFn: () => fetchBlobText(url),
    ...textPolicy(immutable),
  });
}

function textPolicy(immutable: boolean) {
  return {
    ...(immutable ? { staleTime: Infinity, gcTime: Infinity } : {}),
    // A pinned ?v= URL is content-addressed: if it failed it failed, and
    // there is no editor open on it to protect. A LIVE path is different —
    // one 429 used to end an editing session (see EditView), so it backs off
    // and tries again rather than turning into a dead end.
    retry: immutable ? false : (count: number, e: unknown) => count < 3 && isTransientBlobError(e),
    retryDelay: (count: number) => Math.min(1_000 * 2 ** count, 8_000),
  };
}

export function useTextAt(url: string, key: unknown[], enabled: boolean, immutable: boolean) {
  return useQuery({ queryKey: key, queryFn: () => fetchBlobText(url), enabled, ...textPolicy(immutable) });
}

export function useBlobText(apiBase: string, sha: string | undefined, enabled: boolean) {
  return useTextAt(sha ? blobURL(apiBase, sha) : "", ["blob", apiBase, sha], enabled && !!sha, true);
}

/* The URL an HTML page renders from in its sandboxed iframe.

   /file?path= cannot serve one: the page's relative src="a.png" resolves
   against /api/p/<id>/, not the page's folder, and the sandbox's opaque origin
   means the browser sends no session cookie for it anyway. The hub mints a
   path-shaped URL with its own short-lived credential instead (frame.go), so
   siblings resolve by ordinary URL rules. url is undefined while it is being
   minted, and the plain file URL if minting fails, which renders the page
   without its relative files rather than not at all. A pinned version keeps
   its blob URL.

   fresh() is for a reload: it resolves to a URL with time left on it, minting
   a new one when the held one is near its expiry, so a tab left open for hours
   does not reload into a dead token. */
export function useFrameURL(apiBase: string, path: string, fileURL: string, version?: string, enabled = true) {
  const { data, isError, refetch } = useQuery({
    queryKey: ["frame", apiBase, path],
    queryFn: () =>
      getJSON<{ url: string; expires: number }>(apiBase + "frame?path=" + encodeURIComponent(path)),
    // Only a surface that actually frames the page mints one: an editor on
    // Markdown or code has no iframe, and asking anyway is a wasted request.
    enabled: enabled && !version,
    staleTime: Infinity,
    retry: 1,
  });
  const url = version || isError ? fileURL : data?.url;
  const fresh = async () => {
    if (version || !data || data.expires - Date.now() > 10 * 60 * 1000) return;
    await refetch();
  };
  return { url, fresh };
}

/* `url` carrying this project's live journal receipts as `?r=`. An iframe
   load cannot send the X-Bdrive-Receipt header, so a page just saved here and
   served by another hub process would render the bytes from before the save;
   the query param makes that process fold far enough first. Callers compute
   it once per (re)mount: a receipt arriving mid-view must not re-point a live
   frame. */
export function withReceipt(url: string, apiBase: string): string {
  const r = latestProjectReceipt(apiBase);
  return r ? url + (url.includes("?") ? "&" : "?") + "r=" + encodeURIComponent(r) : url;
}
