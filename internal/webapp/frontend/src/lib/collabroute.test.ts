import { test } from "node:test";
import assert from "node:assert/strict";
import { hasCollabRoute } from "./collabroute.ts";

test("a project's api base has a co-editing route", () => {
  assert.equal(hasCollabRoute("/api/p/abc123/"), true);
});

test("a single-volume server opens solo without asking", () => {
  assert.equal(hasCollabRoute("/api/"), false);
});
