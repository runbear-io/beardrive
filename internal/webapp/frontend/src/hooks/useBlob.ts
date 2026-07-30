import { useQuery } from "@tanstack/react-query";
import { getResponse } from "../api/http";
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

async function fetchBlobText(url: string): Promise<BlobText> {
  const r = await getResponse(url);
  // Cheap out before reading the body when the server tells us the size.
  // Content-Length is a hint, not a guarantee (a chunked or proxied
  // response may omit it), so sniffBytes checks the real length too.
  const len = Number(r.headers.get("Content-Length"));
  if (len > MAX_BYTES) return { kind: "too-large", size: len };
  return sniffBytes(new Uint8Array(await r.arrayBuffer()));
}

// `immutable` is for content-addressed URLs: a sha's bytes never change, so
// staleness cannot apply and re-expanding a history row costs no request. A
// live path must never be pinned that way — a teammate's edit would keep
// serving the old body.
export function useTextAt(url: string, key: unknown[], enabled: boolean, immutable: boolean) {
  return useQuery({
    queryKey: key,
    queryFn: () => fetchBlobText(url),
    enabled,
    ...(immutable ? { staleTime: Infinity, gcTime: Infinity } : {}),
    retry: false,
  });
}

export function useBlobText(apiBase: string, sha: string | undefined, enabled: boolean) {
  return useTextAt(sha ? blobURL(apiBase, sha) : "", ["blob", apiBase, sha], enabled && !!sha, true);
}
