// Run with `npm test` (node's built-in runner; node ≥ 23 strips the types).
// Excluded from tsconfig's include — it imports node: builtins, which the
// app's DOM-only lib set does not know about.
import { test } from "node:test";
import assert from "node:assert/strict";
import { sniffBytes, MAX_BYTES, SNIFF } from "./sniff.ts";

const bytes = (s: string) => new TextEncoder().encode(s);

test("UTF-8 text decodes, multi-byte included", () => {
  assert.deepEqual(sniffBytes(bytes("FROM alpine\nRUN apk add curl\n")), {
    kind: "text",
    text: "FROM alpine\nRUN apk add curl\n",
  });
  assert.deepEqual(sniffBytes(bytes("héllo — ✅")), { kind: "text", text: "héllo — ✅" });
});

test("empty buffer is empty text", () => {
  assert.deepEqual(sniffBytes(new Uint8Array(0)), { kind: "text", text: "" });
});

test("a NUL inside the sniff window is binary", () => {
  const buf = new Uint8Array(100);
  buf.set(bytes("ELF"), 0);
  assert.deepEqual(sniffBytes(buf), { kind: "binary" });
});

test("a NUL past the sniff window is text — the window is the trade-off", () => {
  const head = bytes("a".repeat(SNIFF));
  const buf = new Uint8Array(SNIFF + 1);
  buf.set(head, 0); // trailing byte stays 0
  assert.equal(sniffBytes(buf).kind, "text");
});

test("invalid UTF-8 is binary, never mojibake", () => {
  assert.deepEqual(sniffBytes(new Uint8Array([0xff, 0xfe, 0xff])), { kind: "binary" });
});

test("size boundary: exactly MAX_BYTES is text, one more is too-large", () => {
  const ok = new Uint8Array(MAX_BYTES).fill(0x61);
  assert.equal(sniffBytes(ok).kind, "text");
  const big = new Uint8Array(MAX_BYTES + 1).fill(0x61);
  assert.deepEqual(sniffBytes(big), { kind: "too-large", size: MAX_BYTES + 1 });
});
