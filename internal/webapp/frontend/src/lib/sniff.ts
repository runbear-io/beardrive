// Text or not, decided on the bytes and never on the extension — agents
// write plenty of extensionless files (Dockerfile, LICENSE, .bdriveignore).
// Pure so `npm test` (node's runner) can import it without React Query.

// A diff holds both sides in memory at once, so this bound is load-bearing,
// not cosmetic.
export const MAX_BYTES = 1 << 20; // 1 MB
export const SNIFF = 8192;

export type BlobText =
  | { kind: "text"; text: string }
  | { kind: "binary" }
  | { kind: "too-large"; size: number };

export function sniffBytes(buf: Uint8Array): BlobText {
  if (buf.byteLength > MAX_BYTES) return { kind: "too-large", size: buf.byteLength };
  if (buf.subarray(0, SNIFF).includes(0)) return { kind: "binary" };
  try {
    return { kind: "text", text: new TextDecoder("utf-8", { fatal: true }).decode(buf) };
  } catch {
    return { kind: "binary" };
  }
}
