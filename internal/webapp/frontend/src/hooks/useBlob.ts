import { useQuery } from "@tanstack/react-query";
import { getResponse } from "../api/http";

// One exact version's bytes, decoded as text when they are text. Blobs are
// content-addressed and immutable, so a sha is a perfect cache key and
// staleness never applies — re-expanding a history row costs no request.

// Both sides of a diff are held in memory at once, so this bound is
// load-bearing, not cosmetic.
const MAX_BYTES = 1 << 20; // 1 MB
const SNIFF = 8192;

export type BlobText =
  | { kind: "text"; text: string }
  | { kind: "binary" }
  | { kind: "too-large" };

export function blobURL(apiBase: string, sha: string, name?: string, download?: boolean): string {
  let u = apiBase + "blob?sha=" + encodeURIComponent(sha);
  if (name) u += "&name=" + encodeURIComponent(name);
  if (download) u += "&download=1";
  return u;
}

async function fetchBlobText(url: string): Promise<BlobText> {
  const r = await getResponse(url);
  // Cheap out before reading the body when the server tells us the size.
  const len = Number(r.headers.get("Content-Length"));
  if (len > MAX_BYTES) return { kind: "too-large" };
  const buf = new Uint8Array(await r.arrayBuffer());
  if (buf.byteLength > MAX_BYTES) return { kind: "too-large" };
  // Decide on the bytes, never the extension — agents write plenty of
  // extensionless files.
  if (buf.subarray(0, SNIFF).includes(0)) return { kind: "binary" };
  try {
    return { kind: "text", text: new TextDecoder("utf-8", { fatal: true }).decode(buf) };
  } catch {
    return { kind: "binary" };
  }
}

export function useBlobText(apiBase: string, sha: string | undefined, enabled: boolean) {
  return useQuery({
    queryKey: ["blob", apiBase, sha],
    queryFn: () => fetchBlobText(blobURL(apiBase, sha!)),
    enabled: enabled && !!sha,
    staleTime: Infinity,
    gcTime: Infinity,
    retry: false,
  });
}
