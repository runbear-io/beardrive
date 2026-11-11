/* A text this tab once wrote can legitimately come back: History restores
   B to A. It must merge like any other outside write, not be dismissed as a
   late echo of something already moved past.

   Run with `npm test` (node's built-in runner), or alone:
   node --test src/lib/sharedfile_restore.test.ts
   Excluded from tsconfig's include — it imports node: builtins, which the
   app's DOM-only lib set does not know about. */

import { test, mock } from "node:test";
import assert from "node:assert/strict";
import { createRequire, registerHooks } from "node:module";
import { existsSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import type { SharedFile, ShrinkAsk } from "./sharedfile.ts";

/* The modules under test are app code: extensionless imports, and TypeScript
   node cannot strip on its own (collab.ts's parameter properties). So every
   file under src/ is resolved the way the bundler does and transpiled with
   the project's own TypeScript; analytics is a no-op, as in the bundle's
   tests it has always been. Registered before anything under test loads. */
const SRC = new URL("../", import.meta.url).href;
const ts = createRequire(import.meta.url)("typescript");
registerHooks({
  resolve(specifier, context, nextResolve) {
    const parent = context.parentURL;
    if (parent?.startsWith(SRC) && /^\.\.?\//.test(specifier) && !/\.[cm]?[jt]sx?$/.test(specifier)) {
      for (const ext of [".ts", ".tsx", "/index.ts"]) {
        const url = new URL(specifier + ext, parent);
        if (!existsSync(fileURLToPath(url))) continue;
        if (url.href === SRC + "analytics.ts") {
          return { url: "data:text/javascript,export function track() {}", shortCircuit: true };
        }
        return { url: url.href, shortCircuit: true };
      }
    }
    return nextResolve(specifier, context);
  },
  load(url, context, nextLoad) {
    if (url.startsWith(SRC) && /\.tsx?$/.test(url)) {
      const fileName = fileURLToPath(url);
      const out = ts.transpileModule(readFileSync(fileName, "utf8"), {
        fileName,
        compilerOptions: { module: ts.ModuleKind.ESNext, target: ts.ScriptTarget.ES2022, jsx: ts.JsxEmit.ReactJSX },
      });
      return { format: "module", source: out.outputText, shortCircuit: true };
    }
    return nextLoad(url, context);
  },
});

// Dynamic, not static: static imports are linked before the hooks above exist.
const { CollabDoc } = await import("./collab.ts");
const { openSharedFile, SAVE_RETRY_MS } = await import("./sharedfile.ts");
const { parseConflict } = await import("./conflict.ts");
const { merge3, merge3PreferRight, merge3ForPath, validMergedText } = await import("./merge3.ts");
const { frameChannel } = await import("./frameflush.ts");

/* toEqual's semantics: a property that is undefined is a property that is
   absent. Only plain objects and arrays are rebuilt; anything else (a
   CollabDoc) is compared as itself. */
function plain(v: unknown): unknown {
  if (Array.isArray(v)) return v.map(plain);
  if (v !== null && typeof v === "object" && Object.getPrototypeOf(v) === Object.prototype) {
    return Object.fromEntries(
      Object.entries(v)
        .filter(([, x]) => x !== undefined)
        .map(([k, x]) => [k, plain(x)]),
    );
  }
  return v;
}
function same(actual: unknown, expected: unknown) {
  assert.deepEqual(plain(actual), plain(expected));
}

/* Fake timers: node's mock clock, plus the due time of every pending timer,
   so advance() runs them one at a time in order and lets the promises each
   one starts settle before the next — a retry armed by a failed request
   lands at exactly its delay, not at the end of a coarse tick. */
const realTimerFns = {
  setTimeout: globalThis.setTimeout,
  clearTimeout: globalThis.clearTimeout,
  setInterval: globalThis.setInterval,
  clearInterval: globalThis.clearInterval,
};
const dues = new Map<unknown, { due: number; every?: number }>();
function fakeTimers() {
  mock.timers.enable({ apis: ["setTimeout", "setInterval", "Date"], now: Date.now() });
  const st = globalThis.setTimeout;
  const ct = globalThis.clearTimeout;
  const si = globalThis.setInterval;
  const ci = globalThis.clearInterval;
  globalThis.setTimeout = ((fn: (...a: unknown[]) => void, ms = 0, ...args: unknown[]) => {
    const id = st(() => {
      dues.delete(id);
      fn(...args);
    }, ms);
    dues.set(id, { due: Date.now() + Math.max(0, ms) });
    return id;
  }) as typeof setTimeout;
  globalThis.clearTimeout = ((id?: unknown) => {
    dues.delete(id);
    if (id != null) ct(id as Parameters<typeof clearTimeout>[0]);
  }) as typeof clearTimeout;
  globalThis.setInterval = ((fn: (...a: unknown[]) => void, ms = 0, ...args: unknown[]) => {
    const every = Math.max(1, ms);
    const id = si(() => {
      const d = dues.get(id);
      if (d) d.due += every;
      fn(...args);
    }, every);
    dues.set(id, { due: Date.now() + every, every });
    return id;
  }) as typeof setInterval;
  globalThis.clearInterval = ((id?: unknown) => {
    dues.delete(id);
    if (id != null) ci(id as Parameters<typeof clearInterval>[0]);
  }) as typeof clearInterval;
}
function realTimers() {
  mock.timers.reset();
  dues.clear();
  Object.assign(globalThis, realTimerFns);
}
function setSystemTime(d: Date) {
  mock.timers.setTime(d.getTime());
}
async function settle() {
  for (let i = 0; i < 10; i++) await new Promise((r) => setImmediate(r));
}
async function advance(ms: number) {
  const target = Date.now() + ms;
  await settle();
  for (let guard = 0; ; guard++) {
    if (guard > 100_000) throw new Error("advance: timers never settle");
    let next = Infinity;
    for (const d of dues.values()) next = Math.min(next, d.due);
    if (next > target) break;
    mock.timers.tick(Math.max(0, next - Date.now()));
    await settle();
  }
  if (target > Date.now()) mock.timers.tick(target - Date.now());
  await settle();
}
async function waitFor(check: () => void) {
  for (let i = 0; ; i++) {
    try {
      check();
      return;
    } catch (e) {
      if (i > 1_000) throw e;
    }
    await new Promise((r) => setImmediate(r));
  }
}

test("A -> save B -> outside restore to A merges A back into the buffer", async () => {
  const puts: Array<{ body: string; ifMatch?: string }> = [];
  globalThis.fetch = (async (_url: RequestInfo | URL, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    puts.push({ body: String(init?.body), ifMatch: headers["If-Match"] });
    return new Response(JSON.stringify({ sha: "sha-b" }), { status: 200 });
  }) as typeof fetch;

  let buffer = "A\n";
  let solo = false;
  const file = openSharedFile({
    apiBase: "/api/", // no co-editing route: solo, saved with If-Match
    path: "notes.md",
    seed: "A\n",
    baseSha: "sha-a",
    me: { name: "t", colour: "oklch(0.7 0.1 200)" },
    onReady: () => {},
    onSolo: () => {
      solo = true;
    },
    soloText: () => buffer,
    soloApply: (e) => {
      buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
    },
  });
  await waitFor(() => assert.equal(solo, true));

  buffer = "B\n";
  await file.saveNow();
  same(puts, [{ body: "B\n", ifMatch: "sha-a" }]);

  // Somebody restores the earlier version from History.
  assert.equal(file.merge("A\n", "sha-a2"), "merged");
  assert.equal(buffer, "A\n");
  assert.equal(file.current(), "A\n");

  // And it is now the version the buffer is built on: nothing left to save.
  await file.saveNow();
  assert.equal(puts.length, 1);

  // The save's own echo while it is in flight is still recognised as ours.
  buffer = "C\n";
  const gate = Promise.withResolvers<void>();
  globalThis.fetch = (async (_url: RequestInfo | URL, init?: RequestInit) => {
    puts.push({ body: String(init?.body) });
    await gate.promise;
    return new Response(JSON.stringify({ sha: "sha-c" }), { status: 200 });
  }) as typeof fetch;
  const saving = file.saveNow();
  await waitFor(() => assert.equal(puts.length, 2));
  assert.equal(file.merge("C\n", "sha-c"), "same");
  gate.resolve();
  await saving;
  assert.equal(buffer, "C\n");
  file.destroy();
});

/* A staging checkpoint the hub answers 503 (a write it could not land yet)
   is re-sent on its own once the typing has stopped: nothing else would
   ever send it. A permanent refusal is not retried. */
test("staging checkpoint refused 503 once is retried until it saves", async () => {
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  fakeTimers();
  try {
    const sent: string[] = [];
    let answer = 503;
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      sent.push(String(url));
      const body = JSON.parse(String(init?.body)) as { text: string };
      if (answer !== 200) return new Response(JSON.stringify({ error: "the edit is not written yet", retry: true }), { status: answer });
      return new Response(JSON.stringify({ sha: "sha-b", text: body.text }), { status: 200 });
    }) as typeof fetch;

    let buffer = "A\n";
    const states: string[] = [];
    const savedTexts: string[] = [];
    const file = openSharedFile({
      apiBase: "/api/p/proj/", // a hub with a room route: staging until live
      path: "notes.md",
      seed: "A\n",
      baseSha: "sha-a",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: () => {},
      onSolo: () => {},
      onState: (s) => states.push(s),
      onSaved: (t) => savedTexts.push(t),
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    assert.ok(connect.mock.callCount() > 0);

    buffer = "B\n";
    assert.equal(await file.saveNow(), false); // unsaved: Done stays on the editor
    assert.equal(sent.length, 1);
    assert.ok(sent[0].includes("ycollab/sync"));
    assert.equal(states.at(-1), "error");
    same(savedTexts, []);

    // The typing has stopped; nothing else asks. The endpoint recovers.
    answer = 200;
    await advance(SAVE_RETRY_MS[0]);
    assert.equal(sent.length, 2);
    same(savedTexts, ["B\n"]);
    assert.equal(states.at(-1), "clean");
    assert.equal(await file.saveNow(), true); // nothing owed: no request

    // Saved: no further attempts.
    await advance(60_000);
    assert.equal(sent.length, 2);

    // A permanent refusal is not looped on.
    answer = 403;
    buffer = "C\n";
    await file.saveNow();
    assert.equal(sent.length, 3);
    await advance(60_000);
    assert.equal(sent.length, 3);

    // A transient one keeps backing off, and destroy() ends it.
    answer = 503;
    await file.saveNow();
    assert.equal(sent.length, 4);
    await advance(SAVE_RETRY_MS[0]);
    assert.equal(sent.length, 5);
    await advance(SAVE_RETRY_MS[1] - 1);
    assert.equal(sent.length, 5);
    await advance(1);
    assert.equal(sent.length, 6);
    file.destroy(); // sends the buffer once more, keepalive
    const atDestroy = sent.length;
    await advance(60_000);
    assert.equal(sent.length, atDestroy);
  } finally {
    realTimers();
    connect.mock.restore();
  }
});

/* A PUT refused 409 rebases onto the file it lost to. When that read fails
   for a moment, the stopped typing must still be saved once the hub
   recovers: the retry timer is what sends it, not a next keystroke. */
test("PUT 409 -> GET /file 503 -> stopped typing is retried and saved", async () => {
  fakeTimers();
  try {
    const log: string[] = [];
    let getAnswer = 503;
    let head = "A\nX\n";
    let headSha = "sha-x";
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = String(url);
      const method = init?.method ?? "GET";
      log.push(method + " " + u);
      if (method === "GET") {
        if (getAnswer !== 200) return new Response("unavailable", { status: getAnswer });
        return new Response(head, { status: 200, headers: { ETag: `"${headSha}"` } });
      }
      const headers = (init?.headers ?? {}) as Record<string, string>;
      if (headers["If-Match"] !== headSha) {
        return new Response(JSON.stringify({ error: "changed", sha: headSha }), { status: 409 });
      }
      head = String(init?.body);
      headSha = "sha-saved";
      return new Response(JSON.stringify({ sha: headSha }), { status: 200 });
    }) as typeof fetch;

    let buffer = "A\n";
    let solo = false;
    const states: string[] = [];
    const savedTexts: string[] = [];
    const file = openSharedFile({
      apiBase: "/api/", // no co-editing route: solo, saved with If-Match
      path: "notes.md",
      seed: "A\n",
      baseSha: "sha-a",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: () => {},
      onSolo: () => {
        solo = true;
      },
      onState: (s) => states.push(s),
      onSaved: (t) => savedTexts.push(t),
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    await waitFor(() => assert.equal(solo, true));

    buffer = "B\n";
    await file.saveNow();
    // PUT refused 409, then the head read failed: nothing written, unsaved.
    assert.equal((log.filter((l) => l.startsWith("PUT"))).length, 1);
    assert.equal((log.filter((l) => l.startsWith("GET"))).length, 1);
    assert.equal(states.at(-1), "error");
    same(savedTexts, []);

    // The typing has stopped. The hub recovers; the retry alone saves.
    getAnswer = 200;
    await advance(SAVE_RETRY_MS[0]);
    assert.equal(head, "B\nX\n");
    assert.equal(buffer, "B\nX\n");
    assert.equal(savedTexts.at(-1), "B\nX\n");
    assert.equal(states.at(-1), "clean");

    // Saved: no further attempts.
    const settled = log.length;
    await advance(60_000);
    assert.equal(log.length, settled);
    file.destroy();
  } finally {
    realTimers();
  }
});

/* A buffer typed while the room's socket stalled, joining a room whose text
   changed the same words another way, is parked as a conflict copy — and the
   editor stays on that buffer until the copy has actually landed. A 503 on
   the park is retried on the save backoff; only a stored copy swaps the
   editor onto the shared document. */
function joinHarness(who = "tester") {
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  const puts: Array<{ url: string; body: string; keepalive?: boolean }> = [];
  const posts: string[] = [];
  const net = { putAnswer: 503 };
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    const u = String(url);
    if (init?.method === "PUT") {
      puts.push({ url: u, body: String(init.body), keepalive: init.keepalive });
      if (net.putAnswer !== 200) return new Response("unavailable", { status: net.putAnswer });
      return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
    }
    posts.push(u);
    return new Response(JSON.stringify({ sha: "sha-room", text: "" }), { status: 200 });
  }) as typeof fetch;

  const state = { buffer: "the cat sat\n" };
  const ready: unknown[] = [];
  const copies: string[] = [];
  const file = openSharedFile({
    apiBase: "/api/p/proj/", // a hub with a room route: staging until live
    path: "notes.md",
    seed: "the cat sat\n",
    baseSha: "sha-a",
    who,
    me: { name: "t", colour: "oklch(0.7 0.1 200)" },
    onReady: (c) => ready.push(c),
    onSolo: () => {},
    onConflictCopy: (c) => copies.push(c),
    soloText: () => state.buffer,
    soloApply: (e) => {
      state.buffer = state.buffer.slice(0, e.from) + e.insert + state.buffer.slice(e.to);
    },
  });
  // The room's document arrives over the socket: a remote-origin update.
  const sock = { destroy() {} };
  const syncRoom = (text: string) => {
    const c = file.collab as unknown as {
      ws: unknown;
      onStatus: (s: string) => void;
      onReady: () => void;
    };
    c.ws = sock;
    file.collab.doc.transact(() => file.collab.text.insert(0, text), sock);
    c.onStatus("live");
    c.onReady();
  };
  return { connect, puts, posts, net, state, ready, copies, file, syncRoom };
}

test("late join conflict: staging is kept until the conflict copy durably lands", async () => {
  fakeTimers();
  const h = joinHarness();
  try {
    // Socket stalled: the user types in staging, changing the same word.
    h.state.buffer = "the dog sat\n";
    h.syncRoom("the bird sat\n");
    await advance(0);

    // merge3 cannot reconcile; the park was refused 503.
    assert.equal(h.puts.length, 1);
    assert.ok(h.puts[0].url.includes("upload/content?path="));
    assert.ok((decodeURIComponent(h.puts[0].url)).includes("notes.md.bdrive-conflict-tester-"));
    assert.equal(h.puts[0].body, "the dog sat\n");
    // Still on the staging buffer: nothing swapped, nothing lost.
    same(h.ready, []);
    same(h.copies, []);
    assert.equal(h.state.buffer, "the dog sat\n");
    assert.equal(h.file.current(), "the dog sat\n");
    // A save nudge while waiting writes nothing of its own to the room.
    await advance(SAVE_RETRY_MS[0] - 1);
    assert.equal(h.puts.length, 1);
    same(h.posts, []);

    // The hub recovers: the retry parks the same copy and only then swaps.
    h.net.putAnswer = 200;
    await advance(1);
    assert.equal(h.puts.length, 2);
    assert.equal(h.puts[1].url, h.puts[0].url);
    assert.equal(h.puts[1].body, "the dog sat\n");
    same(h.copies, [decodeURIComponent(h.puts[0].url.split("path=")[1])]);
    same(h.ready, [h.file.collab]);
    assert.equal(h.file.current(), "the bird sat\n");

    // Joined: no further attempts.
    await advance(60_000);
    assert.equal(h.puts.length, 2);
    same(h.posts, []);
    h.file.destroy();
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("late join conflict: closing before the copy lands parks the buffer keepalive", async () => {
  fakeTimers();
  const h = joinHarness();
  try {
    h.state.buffer = "the dog sat\n";
    h.syncRoom("the bird sat\n");
    await advance(0);
    assert.equal(h.puts.length, 1);
    same(h.ready, []);

    h.file.destroy();
    assert.equal(h.puts.length, 2);
    same(h.puts[1], { url: h.puts[0].url, body: "the dog sat\n", keepalive: true });
    // destroy() ended the retry.
    await advance(60_000);
    assert.equal(h.puts.length, 2);
    same(h.ready, []);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* A park still on the wire when the tab closes with newer text: the slow PUT
   may land AFTER the teardown keepalive. It must never overwrite the newer
   words, so the teardown writes a name of its own — one second later, under
   the user's own device name, for EVERY device name (a 32-character name
   ending in "-close" is the one a suffix scheme would collide on). */
for (const who of ["tester", "abcdefghijklmnopqrstuvwxyz-close"]) {
  test(`late join conflict: a slow in-flight park never overwrites the teardown copy (who=${who})`, async () => {
    fakeTimers();
    setSystemTime(new Date("2026-11-11T10:20:30.400Z"));
    const h = joinHarness(who);
    try {
      const server = new Map<string, string>();
      const order: string[] = [];
      const slow = Promise.withResolvers<void>();
      let first = true;
      globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
        const u = decodeURIComponent(String(url));
        if (init?.method === "PUT") {
          const body = String(init.body);
          h.puts.push({ url: u, body, keepalive: init.keepalive });
          if (first) {
            first = false;
            await slow.promise; // PUT #1 hangs on the wire
          }
          server.set(u, body);
          order.push(body);
          return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
        }
        h.posts.push(u);
        return new Response(JSON.stringify({ sha: "sha-room", text: "" }), { status: 200 });
      }) as typeof fetch;

      h.state.buffer = "v1 dog\n";
      h.syncRoom("v0 bird\n");
      await advance(0);
      assert.equal(h.puts.length, 1);
      assert.equal(h.puts[0].body, "v1 dog\n");

      // The user keeps typing, then closes the tab in the SAME second while
      // PUT #1 is in flight.
      h.state.buffer = "v2 cat\n";
      h.file.destroy();
      await advance(0);
      assert.equal(h.puts.length, 2);
      assert.equal(h.puts[1].body, "v2 cat\n");
      assert.equal(h.puts[1].keepalive, true);
      assert.notEqual(h.puts[1].url, h.puts[0].url);
      same(order, ["v2 cat\n"]);

      // The slow PUT #1 lands after the teardown copy.
      slow.resolve();
      await advance(0);
      same(order, ["v2 cat\n", "v1 dog\n"]);
      assert.equal(server.get(h.puts[1].url), "v2 cat\n");
      assert.equal(server.get(h.puts[0].url), "v1 dog\n");

      // Both are well-formed copies under the user's own device name; the
      // teardown one is stamped one second after the park.
      const park = parseConflict(h.puts[0].url.split("path=")[1]);
      const close = parseConflict(h.puts[1].url.split("path=")[1]);
      assert.equal(park?.device, who);
      assert.equal(close?.device, who);
      assert.equal(park?.original, "notes.md");
      assert.equal(close?.original, "notes.md");
      assert.equal(close!.when.getTime() - park!.when.getTime(), 1000);
    } finally {
      realTimers();
      h.connect.mock.restore();
    }
  });
}

/* Go textmerge.Merge parity: two different insertions at one anchor are both
   kept (a log line appended again is a second line, not a retry), and over
   an empty ancestor two different bodies are two documents: a conflict. */
test("merge3 keeps both same-anchor insertions, matching textmerge.Merge", () => {
  same(merge3("log\n", "log\nx\n", "log\nx\nx\n"), { ok: true, text: "log\nx\nx\nx\n" });
  same(merge3("log\n", "log\nx\nx\n", "log\nx\n"), { ok: true, text: "log\nx\nx\nx\n" });
  same(merge3("log\n", "log\nb\n", "log\na\n"), { ok: true, text: "log\na\nb\n" });
  same(merge3("log\n", "log\nx\n", "log\nx\n"), { ok: true, text: "log\nx\n" });
  same(merge3("", "x\n", "y\n"), { ok: false });
  same(merge3("", "y\n", "x\n"), { ok: false });
  same(merge3("", "x\n", "x\ny\n"), { ok: true, text: "x\ny\n" });
  same(merge3("", "x\ny\n", "x\n"), { ok: true, text: "x\ny\n" });
});

/* Unterminated last lines are lines too: textmerge.Merge splits on "\n" and
   decides the final newline on its own, so an appended line never glues onto
   an unterminated one — in either argument order. */
test("merge3 keeps an unterminated same-anchor insertion its own line, matching textmerge.Merge", () => {
  same(merge3("log\n", "log\nx", "log\nx\nx"), { ok: true, text: "log\nx\nx\nx" });
  same(merge3("log\n", "log\nx\nx", "log\nx"), { ok: true, text: "log\nx\nx\nx" });
  same(merge3("log\n", "log\nx", "log\ny\n"), { ok: true, text: "log\nx\ny" });
  same(merge3("log\n", "log\ny\n", "log\nx"), { ok: true, text: "log\nx\ny" });
  same(merge3("a\nb\n", "a\nb", "a\nb\nc\n"), { ok: true, text: "a\nb\nc" });
  same(merge3("a\nb\n", "a\nb\nc\n", "a\nb"), { ok: true, text: "a\nb\nc" });
});

/* VisualEdit's patch against a co-editor's typing in the same element: when
   both changed the same word, only that word takes the patch (`right`); a
   word only the co-editor changed survives, and so does the next keystroke. */
test("merge3PreferRight lets only the colliding words take the patch", () => {
  same(merge3("cat sales", "dog revenue", "bird sales"), { ok: false });
  assert.equal(merge3PreferRight("cat sales", "dog revenue", "bird sales"), "bird revenue");
  assert.equal(merge3PreferRight("bird sales", "bird revenue", "bird! sales"), "bird! revenue");
  assert.equal(
    merge3PreferRight("line1\ncat sales\nline3", "PEER1\ndog revenue\nline3", "line1\nbird sales\nMY3"),
    "PEER1\nbird revenue\nMY3",
  );
  assert.equal(
    merge3PreferRight("<b>cat</b> and <i>sales</i>", "<b>dog</b> and <i>revenue</i>", "<b>bird</b> and <i>sales</i>"),
    "<b>bird</b> and <i>revenue</i>",
  );
  // Fast paths and clean merges are merge3's answer.
  assert.equal(merge3PreferRight("a b", "a b", "a c"), "a c");
  assert.equal(merge3PreferRight("a b", "x b", "a b"), "x b");
  assert.equal(merge3PreferRight("a b", "x b", "a y"), "x y");
  // Same-anchor insertions keep both, as merge3 does.
  assert.equal(merge3PreferRight("log\n", "log\nb\n", "log\na\n"), "log\na\nb\n");
});

/* The hub's validMergedText, in the browser: line merges of two valid JSON
   edits can be invalid JSON, and only a REAL merge is held to it. */
test("validMergedText and merge3ForPath hold .json/.jsonl merges to valid JSON", () => {
  // Two inserts at one anchor: a line merge, but not JSON.
  same(merge3("[\n]\n", "[\n1\n]\n", "[\n2\n]\n"), { ok: true, text: "[\n1\n2\n]\n" });
  assert.equal(validMergedText("data.json", "[\n1\n2\n]\n"), false);
  assert.equal(validMergedText("DIR/Data.JSON", "[\n1\n2\n]\n"), false); // extension case-insensitive
  assert.equal(validMergedText("notes.md", "[\n1\n2\n]\n"), true);
  assert.equal(validMergedText("data.json.bdrive-conflict-x-20261111T102030Z", "[\n1\n2\n]\n"), true);
  same(merge3ForPath("data.json", "[\n]\n", "[\n1\n]\n", "[\n2\n]\n"), { ok: false });
  same(merge3ForPath("notes.md", "[\n]\n", "[\n1\n]\n", "[\n2\n]\n"), { ok: true, text: "[\n1\n2\n]\n" });
  // One side alone is its own text, valid or mid-typing: never a conflict.
  same(merge3ForPath("data.json", "[\n]\n", "[\n]\n", "[\n2,\n"), { ok: true, text: "[\n2,\n" });

  // JSONL: two valid lines at one anchor merge; a broken line does not.
  same(merge3ForPath("events.jsonl", '{"x":0}\n', '{"x":0}\n{"a":1}\n', '{"x":0}\n{"b":2}\n'), {
    ok: true,
    text: '{"x":0}\n{"a":1}\n{"b":2}\n',
  });
  assert.equal(validMergedText("events.jsonl", '{"a":1}\n\n  \n{"b":2}'), true);
  same(merge3ForPath("events.jsonl", '{"x":0}\n', '{"x":0}\n{"a":1}\n', '{"x":0}\n{"b":\n'), { ok: false });
  assert.equal(validMergedText("events.jsonl", '{"a":1}\n{"b":'), false);
});

/* A staging checkpoint the room refuses 409 while newer typing waits behind
   it (its idle timer already fired into the in-flight loop). The refusal is
   settled for the buffer as it is NOW: the conflict copy carries the newer
   words, typing that lands while the copy is on the wire goes to the room
   next, and a tab closed mid-park parks the newest words keepalive. */
function stagingConflictHarness(o: { path?: string; seed?: string } = {}) {
  const seed = o.seed ?? "v0\n";
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  const posts: { base_text?: string; text?: string }[] = [];
  const puts: { url: string; body: string; keepalive?: boolean }[] = [];
  const roomAnswers: Array<PromiseWithResolvers<Response>> = [];
  const putAnswers: Array<PromiseWithResolvers<Response>> = [];
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    if (init?.method === "PUT") {
      puts.push({ url: decodeURIComponent(String(url)), body: String(init.body), keepalive: init.keepalive });
      const d = Promise.withResolvers<Response>();
      putAnswers.push(d);
      return d.promise;
    }
    posts.push(JSON.parse(String(init?.body)) as { base_text?: string; text?: string });
    const d = Promise.withResolvers<Response>();
    roomAnswers.push(d);
    return d.promise;
  }) as typeof fetch;
  const state = { buffer: seed };
  const copies: string[] = [];
  const file = openSharedFile({
    apiBase: "/api/p/proj/",
    path: o.path ?? "notes.md",
    seed,
    baseSha: "sha-0",
    who: "tester",
    me: { name: "t", colour: "oklch(0.7 0.1 200)" },
    onReady: () => {},
    onSolo: () => {},
    onConflictCopy: (c) => copies.push(c),
    soloText: () => state.buffer,
    soloApply: (e) => {
      state.buffer = state.buffer.slice(0, e.from) + e.insert + state.buffer.slice(e.to);
    },
  });
  const clash = () =>
    new Response(JSON.stringify({ error: "the same lines changed in the live document", sha: "sha-room", text: "v1 theirs\n" }), {
      status: 409,
    });
  const ok = (body: object) => new Response(JSON.stringify(body), { status: 200 });
  return { connect, posts, puts, roomAnswers, putAnswers, state, copies, file, clash, ok };
}

test("staging 409 parks the NEWER buffer typed while the checkpoint was out, never the stale one", async () => {
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    h.state.buffer = "v1 conflict\n";
    void h.file.saveNow();
    await advance(0);
    same(h.posts, [{ base: "sha-0", base_text: "v0\n", text: "v1 conflict\n" }]);

    // Typing continues and its idle timer fires while checkpoint #1 is out.
    h.state.buffer = "v1 conflict + more important notes\n";
    void h.file.saveNow();
    await advance(700);
    assert.equal(h.posts.length, 1); // single-flight

    // #1 is refused: the room changed the same line.
    h.roomAnswers[0].resolve(h.clash());
    await advance(0);
    assert.equal(h.puts.length, 1);
    assert.equal(h.puts[0].body, "v1 conflict + more important notes\n");
    assert.ok(h.puts[0].url.includes("notes.md.bdrive-conflict-tester-"));

    // Even more typing lands while the copy is on the wire.
    h.state.buffer = "v1 conflict + more important notes + p.s.\n";
    h.putAnswers[0].resolve(h.ok({ sha: "sha-copy" }));
    await advance(0);
    assert.equal(h.copies.length, 1);
    // ...so it is not stranded: it goes to the room, built on what was parked.
    assert.equal(h.posts.length, 2);
    same(h.posts[1], {
      base: "sha-room",
      base_text: "v1 conflict + more important notes\n",
      text: "v1 conflict + more important notes + p.s.\n",
    });
    h.roomAnswers[1].resolve(h.ok({ sha: "sha-room-2", text: "v1 theirs + p.s.\n" }));
    await advance(0);
    assert.equal(h.posts.length, 2);
    assert.equal(h.puts.length, 1);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* data.json: a staging checkpoint refused 409 by a room that inserted "1"
   where the buffer inserted "2". The line merge "[1 2]" is not JSON, so the
   buffer is parked beside the file — never rebased and re-sent as one. */
test("staging 409 on a .json file parks an invalid JSON merge instead of sending it", async () => {
  fakeTimers();
  const h = stagingConflictHarness({ path: "data.json", seed: "[\n]\n" });
  try {
    h.state.buffer = "[\n2\n]\n";
    void h.file.saveNow();
    await advance(0);
    same(h.posts, [{ base: "sha-0", base_text: "[\n]\n", text: "[\n2\n]\n" }]);
    h.roomAnswers[0].resolve(
      new Response(JSON.stringify({ error: "the same lines changed", sha: "sha-1", text: "[\n1\n]\n" }), { status: 409 }),
    );
    await advance(0);
    assert.equal(h.puts.length, 1);
    assert.ok(h.puts[0].url.includes("data.json.bdrive-conflict-tester-"));
    assert.equal(h.puts[0].body, "[\n2\n]\n");
    assert.equal(h.state.buffer, "[\n2\n]\n"); // never rebased into "[1 2]"
    h.putAnswers[0].resolve(h.ok({ sha: "sha-copy" }));
    await advance(60_000);
    assert.equal(h.copies.length, 1);
    assert.equal(h.posts.length, 1);
    h.file.destroy();
    assert.ok(h.posts.every((p) => p.text !== "[\n1\n2\n]\n"));
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("solo 409 on a .json file parks an invalid JSON merge instead of writing it", async () => {
  fakeTimers();
  try {
    const puts: Array<{ url: string; body: string }> = [];
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      const u = decodeURIComponent(String(url));
      const method = init?.method ?? "GET";
      if (method === "GET") return new Response("[\n1\n]\n", { status: 200, headers: { ETag: `"sha-1"` } });
      puts.push({ url: u, body: String(init?.body) });
      if (u.includes("bdrive-conflict")) return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
      return new Response(JSON.stringify({ error: "changed", sha: "sha-1" }), { status: 409 });
    }) as typeof fetch;
    let buffer = "[\n]\n";
    let solo = false;
    const states: string[] = [];
    const copies: string[] = [];
    const file = openSharedFile({
      apiBase: "/api/",
      path: "data.json",
      seed: "[\n]\n",
      baseSha: "sha-0",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: () => {},
      onSolo: () => {
        solo = true;
      },
      onState: (s) => states.push(s),
      onConflictCopy: (c) => copies.push(c),
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    await waitFor(() => assert.equal(solo, true));

    buffer = "[\n2\n]\n";
    await file.saveNow();
    assert.equal(puts.length, 2);
    assert.ok(puts[0].url.includes("upload/content?path=data.json"));
    assert.equal(puts[0].body, "[\n2\n]\n");
    assert.ok(puts[1].url.includes("data.json.bdrive-conflict-"));
    assert.equal(puts[1].body, "[\n2\n]\n");
    assert.equal(buffer, "[\n2\n]\n");
    assert.equal(copies.length, 1);
    assert.equal(states.at(-1), "clean");
    await advance(60_000);
    assert.ok(puts.every((p) => p.body !== "[\n1\n2\n]\n"));
    file.destroy();
  } finally {
    realTimers();
  }
});

test("late join on a .json file parks an invalid JSON merge and takes the room's text", async () => {
  fakeTimers();
  const h = stagingConflictHarness({ path: "data.json", seed: "[\n]\n" });
  try {
    h.state.buffer = "[\n2\n]\n";
    const sock = { destroy() {} };
    const c = h.file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };
    c.ws = sock;
    h.file.collab.doc.transact(() => h.file.collab.text.insert(0, "[\n1\n]\n"), sock);
    c.onStatus("live");
    c.onReady();
    await advance(0);
    assert.equal(h.puts.length, 1);
    assert.ok(h.puts[0].url.includes("data.json.bdrive-conflict-tester-"));
    assert.equal(h.puts[0].body, "[\n2\n]\n");
    assert.equal(h.file.collab.text.toString(), "[\n1\n]\n"); // nothing spliced in
    h.putAnswers[0].resolve(h.ok({ sha: "sha-copy" }));
    await advance(0);
    assert.equal(h.copies.length, 1);
    assert.equal(h.file.collab.text.toString(), "[\n1\n]\n");
    assert.equal(h.file.current(), "[\n1\n]\n");
    same(h.posts, []);
    h.file.destroy();
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("staging 409 whose newer typing clears the clash is rebased onto the room, not parked", async () => {
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    h.state.buffer = "v1 conflict\n";
    void h.file.saveNow();
    await advance(0);
    // The user puts the line back and writes below it instead.
    h.state.buffer = "v0\nnew line\n";
    void h.file.saveNow();
    h.roomAnswers[0].resolve(h.clash());
    await advance(0);
    same(h.puts, []);
    assert.equal(h.state.buffer, "v1 theirs\nnew line\n");
    same(h.posts[1], { base: "sha-room", base_text: "v1 theirs\n", text: "v1 theirs\nnew line\n" });
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("staging 409: closing the tab before the conflict copy lands parks the newest words keepalive", async () => {
  fakeTimers();
  setSystemTime(new Date("2026-11-11T10:20:30.400Z"));
  const h = stagingConflictHarness();
  try {
    h.state.buffer = "v1 conflict\n";
    void h.file.saveNow();
    await advance(0);
    h.state.buffer = "v1 conflict + more important notes\n";
    void h.file.saveNow();
    await advance(700);
    h.roomAnswers[0].resolve(h.clash());
    await advance(0);
    assert.equal(h.puts.length, 1); // the park, on the wire

    // Typed more, then closed, while the park still hangs.
    h.state.buffer = "v1 conflict + more important notes + last word\n";
    h.file.destroy();
    assert.equal(h.posts.length, 1); // not sent to the room to be refused again
    assert.equal(h.puts.length, 2);
    assert.equal(h.puts[1].body, "v1 conflict + more important notes + last word\n");
    assert.equal(h.puts[1].keepalive, true);
    assert.ok(h.puts[1].url.includes("notes.md.bdrive-conflict-tester-"));
    assert.notEqual(h.puts[1].url, h.puts[0].url);

    h.putAnswers[0].resolve(h.ok({ sha: "sha-copy" }));
    h.putAnswers[1].resolve(h.ok({ sha: "sha-copy-2" }));
    await advance(60_000);
    assert.equal(h.posts.length, 1);
    assert.equal(h.puts.length, 2);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* R10: a staging checkpoint refused 409 starts a slow park of "v1"; the user
   types "v2" in the SAME second and the socket syncs, so join() parks "v2"
   too. They must never share a name: conflictName stamps whole seconds, and
   a "v1" PUT landing last on the same path would put the older words back
   over the newer copy. (The join waits for the staging park — it is part of
   the save loop the join lets finish first, R11 — so the two land in order,
   but their names still differ.) */
test("staging 409 park and join park in the same second never share a conflict path", async () => {
  fakeTimers();
  setSystemTime(new Date("2026-11-11T10:20:30.400Z"));
  const h = stagingConflictHarness();
  try {
    const sock = { destroy() {} };
    const syncRoom = (text: string) => {
      const c = h.file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };
      c.ws = sock;
      h.file.collab.doc.transact(() => h.file.collab.text.insert(0, text), sock);
      c.onStatus("live");
      c.onReady();
    };
    const server = new Map<string, string>();
    const land = async (i: number) => {
      server.set(h.puts[i].url, h.puts[i].body);
      h.putAnswers[i].resolve(h.ok({ sha: `sha-copy-${i}` }));
      await advance(0);
    };

    h.state.buffer = "v1\n";
    void h.file.saveNow();
    await advance(0);
    assert.equal(h.posts.length, 1);
    h.roomAnswers[0].resolve(h.clash());
    await advance(0);
    // PUT #1: the staging park of "v1", slow.
    assert.equal(h.puts.length, 1);
    assert.equal(h.puts[0].body, "v1\n");

    // Same second: the user types, and the room's document arrives.
    h.state.buffer = "v2\n";
    // (The room changed the very word "v1" became, so "v2" clashes with it
    // even built on the parked "v1".)
    syncRoom("v9 theirs\n");
    await advance(0);
    // The join waits for the staging park on the wire.
    assert.equal(h.puts.length, 1);

    // PUT #1 lands; PUT #2: join() parks the newer buffer, under a new name.
    await land(0);
    assert.equal(h.puts.length, 2);
    assert.equal(h.puts[1].body, "v2\n");
    assert.notEqual(h.puts[1].url, h.puts[0].url);
    await land(1);

    assert.equal(server.get(h.puts[1].url), "v2\n");
    assert.equal(server.get(h.puts[0].url), "v1\n");
    const p1 = parseConflict(h.puts[0].url.split("path=")[1]);
    const p2 = parseConflict(h.puts[1].url.split("path=")[1]);
    assert.equal(p1?.original, "notes.md");
    assert.equal(p2?.original, "notes.md");
    assert.equal(p1?.device, "tester");
    assert.equal(p2?.device, "tester");
    assert.ok((p2!.when.getTime()) > p1!.when.getTime());
    same(h.copies.sort(), [h.puts[0].url, h.puts[1].url].map((u) => u.split("path=")[1]).sort());

    // The room holds the buffer now; the late "v1" landing settled nothing:
    // no further writes, and closing the tab writes nothing either.
    assert.equal(h.file.current(), "v9 theirs\n");
    await advance(60_000);
    assert.equal(h.posts.length, 1);
    assert.equal(h.puts.length, 2);
    h.file.destroy();
    assert.equal(h.puts.length, 2);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* Solo: a PUT refused 409 whose head clashes is parked beside the file. A
   park refused 503 keeps the buffer unsaved ("error") and is retried on the
   save backoff onto the SAME copy, with no new typing and no new write to the
   file; closing the file while that retry waits parks it there keepalive. */
for (const closeBeforeRetry of [false, true]) {
  test(`solo 409 clash: a park refused 503 is retried onto the same copy (close before retry: ${closeBeforeRetry})`, async () => {
    fakeTimers();
    try {
      const puts: Array<{ url: string; body: string; keepalive?: boolean }> = [];
      let copyAnswer = 503;
      globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
        const u = decodeURIComponent(String(url));
        const method = init?.method ?? "GET";
        if (method === "GET") return new Response("Z\n", { status: 200, headers: { ETag: `"sha-z"` } });
        puts.push({ url: u, body: String(init?.body), keepalive: init?.keepalive });
        if (u.includes("bdrive-conflict")) {
          if (copyAnswer !== 200) return new Response("unavailable", { status: copyAnswer });
          return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
        }
        return new Response(JSON.stringify({ error: "changed", sha: "sha-z" }), { status: 409 });
      }) as typeof fetch;

      let buffer = "A\n";
      let solo = false;
      const states: string[] = [];
      const copies: string[] = [];
      const file = openSharedFile({
        apiBase: "/api/", // no co-editing route: solo, saved with If-Match
        path: "notes.md",
        seed: "A\n",
        baseSha: "sha-a",
        me: { name: "t", colour: "oklch(0.7 0.1 200)" },
        onReady: () => {},
        onSolo: () => {
          solo = true;
        },
        onState: (s) => states.push(s),
        onConflictCopy: (c) => copies.push(c),
        soloText: () => buffer,
        soloApply: (e) => {
          buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
        },
      });
      await waitFor(() => assert.equal(solo, true));

      buffer = "B\n";
      assert.equal(await file.saveNow(), false); // the park failed: Done stays
      // The file refused it, the head clashes, and the park was refused.
      assert.equal(puts.length, 2);
      assert.ok(!puts[0].url.includes("bdrive-conflict"));
      assert.ok(puts[1].url.includes("notes.md.bdrive-conflict-"));
      assert.equal(puts[1].body, "B\n");
      assert.equal(states.at(-1), "error");
      same(copies, []);

      copyAnswer = 200;
      if (closeBeforeRetry) {
        file.destroy();
        assert.equal(puts.length, 3);
        assert.equal(puts[2].url, puts[1].url);
        assert.equal(puts[2].body, "B\n");
        assert.equal(puts[2].keepalive, true);
        await advance(60_000);
        assert.equal(puts.length, 3);
        return;
      }

      // No typing: the backoff alone re-sends the park, to the same path.
      await advance(SAVE_RETRY_MS[0]);
      assert.equal(puts.length, 3);
      assert.equal(puts[2].url, puts[1].url);
      assert.equal(puts[2].body, "B\n");
      assert.equal(copies.length, 1);
      assert.ok(puts[2].url.includes(copies[0]));
      assert.equal(states.at(-1), "clean");

      // Parked: nothing more is sent.
      await advance(60_000);
      assert.equal(puts.length, 3);
      assert.equal(await file.saveNow(), true); // parked: nothing owed
      assert.equal(puts.length, 3);
      file.destroy();
      assert.equal(puts.length, 3);
    } finally {
      realTimers();
    }
  });
}

/* The page is going: its last flush asks the hub to park a clash itself, in
   the same request — a discarded page never runs the JS that would send a
   second one. An older hub refuses 409 instead, and a page still alive parks
   from here, keepalive. A head that already carries our edit needs no copy. */
test("staging pagehide asks the hub to park a clash in the same request", async () => {
  Reflect.set(globalThis, "window", new EventTarget());
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    h.state.buffer = "v1 conflict\n";
    window.dispatchEvent(new Event("pagehide"));
    assert.equal(h.posts.length, 1);
    same(h.posts[0], { base: "sha-0", base_text: "v0\n", text: "v1 conflict\n", park_on_conflict: true });
    const parked = "notes.md.bdrive-conflict-tester-20261111-102030";
    h.roomAnswers[0].resolve(h.ok({ ok: true, path: "notes.md", sha: "sha-room", text: "v1 theirs\n", parked }));
    await advance(0);
    same(h.copies, [parked]);
    same(h.puts, []); // one round trip: nothing parked from here
    h.file.destroy();
    await advance(60_000);
    assert.equal(h.posts.length, 1);
    same(h.puts, []);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("staging pagehide refused 409 by an older hub parks the buffer keepalive at once", async () => {
  Reflect.set(globalThis, "window", new EventTarget());
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    h.state.buffer = "v1 conflict\n";
    window.dispatchEvent(new Event("pagehide"));
    assert.equal(h.posts.length, 1);
    same(h.posts[0], { base: "sha-0", base_text: "v0\n", text: "v1 conflict\n", park_on_conflict: true });
    h.roomAnswers[0].resolve(h.clash());
    await advance(0);
    assert.equal(h.puts.length, 1);
    assert.ok(h.puts[0].url.includes("notes.md.bdrive-conflict-tester-"));
    assert.equal(h.puts[0].body, "v1 conflict\n");
    assert.equal(h.puts[0].keepalive, true);
    h.putAnswers[0].resolve(h.ok({ sha: "sha-copy" }));
    await advance(0);
    assert.equal(h.copies.length, 1);
    assert.ok(h.puts[0].url.includes(h.copies[0]));
    h.file.destroy();
    await advance(60_000);
    assert.equal(h.posts.length, 1);
    assert.equal(h.puts.length, 1);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

test("staging pagehide refused 409 by a head that already holds the edit parks nothing", async () => {
  Reflect.set(globalThis, "window", new EventTarget());
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    // Our edit inserted a line; the room holds it, with a line after it.
    h.state.buffer = "v0\nv1 mine\n";
    window.dispatchEvent(new Event("pagehide"));
    assert.equal(h.posts.length, 1);
    h.roomAnswers[0].resolve(
      new Response(JSON.stringify({ error: "changed", sha: "sha-room", text: "v0\nv1 mine\nmore\n" }), { status: 409 }),
    );
    await advance(0);
    same(h.puts, []);
    same(h.copies, []);
    h.file.destroy();
    same(h.puts, []);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

for (const hubParks of [true, false]) {
  test(`solo pagehide PUT asks the hub to park a clash (${hubParks ? "hub parks it" : "older hub: 409, parked from here"})`, async () => {
    Reflect.set(globalThis, "window", new EventTarget());
    fakeTimers();
    try {
      const parked = "notes.md.bdrive-conflict-browser-20261111-102030";
      const puts: Array<{ url: string; body: string; keepalive?: boolean; park?: string }> = [];
      globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
        const u = decodeURIComponent(String(url));
        const headers = (init?.headers ?? {}) as Record<string, string>;
        puts.push({ url: u, body: String(init?.body), keepalive: init?.keepalive, park: headers["X-Bdrive-Park-On-Conflict"] });
        if (u.includes("bdrive-conflict")) return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
        if (hubParks) return new Response(JSON.stringify({ ok: true, path: "notes.md", sha: "sha-z", parked }), { status: 200 });
        return new Response(JSON.stringify({ error: "changed", sha: "sha-z" }), { status: 409 });
      }) as typeof fetch;
      let buffer = "A\n";
      let solo = false;
      const copies: string[] = [];
      const file = openSharedFile({
        apiBase: "/api/",
        path: "notes.md",
        seed: "A\n",
        baseSha: "sha-a",
        me: { name: "t", colour: "oklch(0.7 0.1 200)" },
        onReady: () => {},
        onSolo: () => {
          solo = true;
        },
        onConflictCopy: (c) => copies.push(c),
        soloText: () => buffer,
        soloApply: (e) => {
          buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
        },
      });
      await waitFor(() => assert.equal(solo, true));

      buffer = "B\n";
      window.dispatchEvent(new Event("pagehide"));
      assert.equal(puts.length, 1);
      assert.ok(puts[0].url.includes("upload/content?path=notes.md"));
      assert.equal(puts[0].keepalive, true);
      assert.equal(puts[0].park, "1");
      await advance(0);
      if (hubParks) {
        assert.equal(puts.length, 1); // one round trip
        same(copies, [parked]);
      } else {
        assert.equal(puts.length, 2);
        assert.ok(puts[1].url.includes("notes.md.bdrive-conflict-"));
        assert.equal(puts[1].body, "B\n");
        assert.equal(puts[1].keepalive, true);
        assert.equal(puts[1].park, "1"); // create-only, stepped by the hub if taken
        assert.equal(copies.length, 1);
      }
      const sent = puts.length;
      file.destroy();
      await advance(60_000);
      assert.equal(puts.length, sent);
    } finally {
      realTimers();
    }
  });
}

/* The hub's create-only PUT (If-None-Match: *): other bytes at the name are a
   409, the same bytes are 200 unchanged. `takenFirst`: another tab of this
   account already parked its own words at the first name this tab mints.
   `loseFirst`: the first copy is stored, but its answer never arrives. */
function copyServer(o: { takenFirst?: boolean; loseFirst?: boolean }) {
  const store = new Map<string, string>();
  const log: Array<{ path: string; ifNoneMatch?: string; status: number }> = [];
  const handle = (url: RequestInfo | URL, init?: RequestInit): Response => {
    const path = decodeURIComponent(String(url).split("path=")[1]);
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const body = String(init?.body);
    const first = log.length === 0;
    if (first && o.takenFirst) store.set(path, "another tab's words\n");
    const had = store.get(path);
    let res: Response;
    if (headers["If-None-Match"] === "*" && had !== undefined && had !== body) {
      res = new Response(JSON.stringify({ error: "exists" }), { status: 409 });
    } else {
      store.set(path, body);
      res =
        first && o.loseFirst
          ? new Response("unavailable", { status: 503 })
          : new Response(JSON.stringify({ ok: true, path, sha: "sha-copy", unchanged: had === body }), { status: 200 });
    }
    log.push({ path, ifNoneMatch: headers["If-None-Match"], status: res.status });
    return res;
  };
  return { store, log, handle };
}

for (const scenario of ["taken", "lost"] as const) {
  test(`solo clash park is create-only (${scenario === "taken" ? "a taken second steps to the next" : "a lost answer retries the same name, unchanged"})`, async () => {
    fakeTimers();
    setSystemTime(new Date("2026-11-11T10:20:30.400Z"));
    try {
      const server = copyServer(scenario === "taken" ? { takenFirst: true } : { loseFirst: true });
      globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? "GET";
        if (method === "GET") return new Response("Z\n", { status: 200, headers: { ETag: `"sha-z"` } });
        if (decodeURIComponent(String(url)).includes("bdrive-conflict")) return server.handle(url, init);
        return new Response(JSON.stringify({ error: "changed", sha: "sha-z" }), { status: 409 });
      }) as typeof fetch;
      let buffer = "A\n";
      let solo = false;
      const states: string[] = [];
      const copies: string[] = [];
      const file = openSharedFile({
        apiBase: "/api/",
        path: "notes.md",
        seed: "A\n",
        baseSha: "sha-a",
        me: { name: "t", colour: "oklch(0.7 0.1 200)" },
        onReady: () => {},
        onSolo: () => {
          solo = true;
        },
        onState: (s) => states.push(s),
        onConflictCopy: (c) => copies.push(c),
        soloText: () => buffer,
        soloApply: (e) => {
          buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
        },
      });
      await waitFor(() => assert.equal(solo, true));

      buffer = "B\n";
      await file.saveNow();
      if (scenario === "taken") {
        // Refused at the taken name, landed at once at the next second.
        same(server.log.map((l) => [l.ifNoneMatch, l.status]), [["*", 409], ["*", 200]]);
        const [taken, mine] = server.log.map((l) => l.path);
        assert.notEqual(taken, mine);
        assert.ok(mine.includes("notes.md.bdrive-conflict-"));
        assert.equal(server.store.get(taken), "another tab's words\n"); // never overwritten
        assert.equal(server.store.get(mine), "B\n");
        same(copies, [mine]);
        assert.equal(states.at(-1), "clean");
      } else {
        // Stored, but the answer was lost: unsaved, retried on the backoff.
        same(server.log.map((l) => l.status), [503]);
        assert.equal(states.at(-1), "error");
        same(copies, []);
        await advance(SAVE_RETRY_MS[0]);
        same(server.log.map((l) => [l.ifNoneMatch, l.status]), [["*", 503], ["*", 200]]);
        assert.equal(server.log[1].path, server.log[0].path); // the same name, not a second copy
        assert.equal(server.store.size, 1);
        assert.equal(server.store.get(server.log[0].path), "B\n");
        same(copies, [server.log[0].path]);
        assert.equal(states.at(-1), "clean");
      }
      await advance(60_000);
      assert.equal(server.log.length, 2);
      file.destroy();
    } finally {
      realTimers();
    }
  });
}

test("late join conflict park steps past a second another tab already took", async () => {
  fakeTimers();
  setSystemTime(new Date("2026-11-11T10:20:30.400Z"));
  const h = joinHarness();
  try {
    h.net.putAnswer = 200;
    const server = copyServer({ takenFirst: true });
    const inner = globalThis.fetch;
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT") {
        const res = server.handle(url, init);
        if (res.status !== 200) return res;
      }
      return inner(url, init);
    }) as typeof fetch;

    h.state.buffer = "the dog sat\n";
    h.syncRoom("the bird sat\n");
    await advance(0);

    same(server.log.map((l) => [l.ifNoneMatch, l.status]), [["*", 409], ["*", 200]]);
    const [taken, mine] = server.log.map((l) => l.path);
    assert.notEqual(taken, mine);
    assert.equal(server.store.get(taken), "another tab's words\n");
    assert.equal(server.store.get(mine), "the dog sat\n");
    same(h.copies, [mine]);
    same(h.ready, [h.file.collab]);
    assert.equal(h.file.current(), "the bird sat\n");
    await advance(60_000);
    assert.equal(server.log.length, 2);
    h.file.destroy();
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* Emptying a file is an edit: the way out sends "" rather than treating an
   empty buffer as nothing to say, in every mode and on both exits. */
for (const mode of ["staging", "solo"] as const) {
  for (const exit of ["pagehide", "destroy"] as const) {
    test(`an emptied file is flushed on the way out (${mode}, ${exit})`, async () => {
      Reflect.set(globalThis, "window", new EventTarget());
      fakeTimers();
      const connect = mock.method(CollabDoc.prototype, "connect", () => {});
      try {
        const reqs: Array<{ method: string; url: string; body: string }> = [];
        globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
          const method = init?.method ?? "GET";
          reqs.push({ method, url: decodeURIComponent(String(url)), body: String(init?.body) });
          if (method === "PUT") return new Response(JSON.stringify({ sha: "sha-2" }), { status: 200 });
          return new Response(JSON.stringify({ sha: "sha-2", text: "" }), { status: 200 });
        }) as typeof fetch;
        let buffer = "todo\n";
        let solo = false;
        const file = openSharedFile({
          apiBase: mode === "solo" ? "/api/" : "/api/p/proj/",
          path: "notes.md",
          seed: "todo\n",
          baseSha: "sha-a",
          who: "tester",
          me: { name: "t", colour: "oklch(0.7 0.1 200)" },
          onReady: () => {},
          onSolo: () => {
            solo = true;
          },
          soloText: () => buffer,
          soloApply: (e) => {
            buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
          },
        });
        if (mode === "solo") await waitFor(() => assert.equal(solo, true));

        buffer = "";
        if (exit === "pagehide") window.dispatchEvent(new Event("pagehide"));
        else file.destroy();
        await advance(0);
        assert.equal(reqs.length, 1);
        if (mode === "solo") {
          assert.equal(reqs[0].method, "PUT");
          assert.ok(reqs[0].url.includes("upload/content?path=notes.md"));
          assert.equal(reqs[0].body, "");
        } else {
          assert.equal(reqs[0].method, "POST");
          assert.ok(reqs[0].url.includes("ycollab/sync?path=notes.md"));
          const body = JSON.parse(reqs[0].body) as { base_text?: string; text?: string };
          assert.equal(body.text, "");
          assert.equal(body.base_text, "todo\n");
        }
        if (exit === "pagehide") {
          // Landed: the unmount that follows has nothing left to send.
          file.destroy();
          await advance(0);
          assert.equal(reqs.length, 1);
        }
      } finally {
        realTimers();
        connect.mock.restore();
      }
    });
  }
}

/* A pagehide flush refused for a sharp shrink, on a page that lives on (the
   bfcache, or no unload at all): the user is asked, as a save would ask. */
test("a pagehide flush refused 422 for a shrink asks the user when the page lives on", async () => {
  Reflect.set(globalThis, "window", new EventTarget());
  fakeTimers();
  try {
    const reqs: string[] = [];
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      reqs.push((init?.method ?? "GET") + " " + decodeURIComponent(String(url)));
      return new Response(JSON.stringify({ error: "would empty the file", shrink: true }), { status: 422 });
    }) as typeof fetch;
    let buffer = "todo\n";
    let solo = false;
    const asks: ShrinkAsk[] = [];
    const file = openSharedFile({
      apiBase: "/api/",
      path: "notes.md",
      seed: "todo\n",
      baseSha: "sha-a",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: () => {},
      onSolo: () => {
        solo = true;
      },
      onShrinkRefused: (a) => asks.push(a),
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    await waitFor(() => assert.equal(solo, true));
    buffer = "";
    window.dispatchEvent(new Event("pagehide"));
    await advance(0);
    assert.equal(reqs.length, 1);
    assert.equal(asks.length, 1);
    file.destroy();
  } finally {
    realTimers();
  }
});

/* A staging pagehide checkpoints "v0\nx\n"; the page is restored from the
   bfcache and the user types a second "x". Landed, the next checkpoint is
   built on what landed (two x lines at the hub, never three). Failed after
   it may have reached the room, the next one names it in prior_sent so the
   hub never applies it twice. Still unanswered, the next save waits for it
   rather than racing it to the room. */
for (const answer of ["landed", "502", "unsettled"] as const) {
  test(`a bfcache-restored page builds on its pagehide checkpoint (${answer})`, async () => {
    Reflect.set(globalThis, "window", new EventTarget());
    fakeTimers();
    const h = stagingConflictHarness();
    try {
      h.state.buffer = "v0\nx\n";
      window.dispatchEvent(new Event("pagehide"));
      assert.equal(h.posts.length, 1);
      same(h.posts[0], { base: "sha-0", base_text: "v0\n", text: "v0\nx\n", park_on_conflict: true });
      if (answer === "landed") h.roomAnswers[0].resolve(h.ok({ ok: true, sha: "sha-1", text: "v0\nx\n" }));
      if (answer === "502") h.roomAnswers[0].resolve(new Response("bad gateway", { status: 502 }));
      await advance(0);

      window.dispatchEvent(new Event("pageshow"));
      h.state.buffer = "v0\nx\nx\n";
      void h.file.saveNow();
      await advance(0);
      if (answer === "unsettled") {
        // Held behind the pagehide request until it settles, here as a 502.
        assert.equal(h.posts.length, 1);
        h.roomAnswers[0].resolve(new Response("bad gateway", { status: 502 }));
        await advance(0);
      }
      assert.equal(h.posts.length, 2);
      if (answer === "landed") {
        same(h.posts[1], { base: "sha-1", base_text: "v0\nx\n", text: "v0\nx\nx\n" });
      } else {
        same(h.posts[1], { base: "sha-0", base_text: "v0\n", text: "v0\nx\nx\n", prior_sent: ["v0\nx\n"] });
      }
    } finally {
      realTimers();
      h.connect.mock.restore();
    }
  });
}

/* Back from the bfcache with the pagehide checkpoint still on the wire, the
   socket syncs: the join waits for that checkpoint rather than peeking past
   it (a peek that beats it to the room would splice "x" in, and the late
   checkpoint would land a second one). It lands, and the line is there once. */
test("a join after a bfcache restore waits for the in-flight pagehide checkpoint", async () => {
  Reflect.set(globalThis, "window", new EventTarget());
  fakeTimers();
  const h = stagingConflictHarness();
  try {
    const sock = { destroy() {} };
    const c = h.file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };

    h.state.buffer = "v0\nx\n";
    window.dispatchEvent(new Event("pagehide"));
    assert.equal(h.posts.length, 1);
    window.dispatchEvent(new Event("pageshow"));

    // The room's document arrives before the checkpoint has reached it.
    c.ws = sock;
    h.file.collab.doc.transact(() => h.file.collab.text.insert(0, "v0\n"), sock);
    c.onStatus("live");
    c.onReady();
    await advance(0);
    // Waiting: no peek, no splice, no park.
    assert.equal(h.posts.length, 1);
    same(h.puts, []);
    assert.equal(h.file.collab.text.toString(), "v0\n");

    // The checkpoint lands, and the room broadcasts its line on the socket.
    h.roomAnswers[0].resolve(h.ok({ ok: true, sha: "sha-1", text: "v0\nx\n" }));
    h.file.collab.doc.transact(() => h.file.collab.text.insert(3, "x\n"), sock);
    await advance(0);
    assert.equal(h.file.collab.text.toString(), "v0\nx\n"); // once, never twice
    assert.equal(h.posts.length, 1);
    same(h.puts, []);
    await advance(60_000);
    assert.equal(h.file.collab.text.toString(), "v0\nx\n");
    assert.equal(h.posts.length, 1);
  } finally {
    realTimers();
    h.connect.mock.restore();
  }
});

/* R11: the socket syncs while a staging checkpoint is still on the wire. The
   room applies the checkpoint (its own item IDs), and the same items reach
   this tab twice — in the HTTP answer's `update` and on the socket. The join
   must wait for the checkpoint, take its items, and splice only the typing
   past it: two x lines, never three, and no second checkpoint. */
for (const { frameFirst, name } of [
  { frameFirst: false, name: "socket frame after the HTTP answer" },
  { frameFirst: true, name: "socket frame before the HTTP answer" },
]) test(`join waits for an in-flight staging checkpoint and lands it once (${name})`, async () => {
  const Y = await import("yjs");
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  fakeTimers();
  try {
    const b64 = (u8: Uint8Array) => btoa(String.fromCharCode(...u8));
    // The hub's room, holding the file as it was.
    const room = new Y.Doc();
    room.getText("body").insert(0, "log\n");
    const beforeCheckpoint = Y.encodeStateAsUpdate(room);

    const posts: Array<{ base_text?: string; text?: string }> = [];
    const puts: string[] = [];
    const gate = Promise.withResolvers<void>();
    let applied: Uint8Array | null = null;
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT") {
        puts.push(String(url));
        return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
      }
      const body = JSON.parse(String(init?.body)) as { base_text?: string; text?: string };
      posts.push(body);
      // The room applies the checkpoint (handleYCollabSync's splice) the
      // moment the request arrives; the answer is held back.
      const before = Y.encodeStateVector(room);
      const t = room.getText("body");
      t.delete(0, t.length);
      t.insert(0, body.text ?? "");
      applied = Y.encodeStateAsUpdate(room, before);
      await gate.promise;
      return new Response(
        JSON.stringify({ ok: true, sha: "sha-room", text: t.toString(), update: b64(Y.encodeStateAsUpdate(room)) }),
        { status: 200 },
      );
    }) as typeof fetch;

    let buffer = "log\n";
    const ready: unknown[] = [];
    const file = openSharedFile({
      apiBase: "/api/p/proj/", // a hub with a room route: staging until live
      path: "notes.md",
      seed: "log\n",
      baseSha: "sha-a",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: (c) => ready.push(c),
      onSolo: () => {},
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    assert.ok(connect.mock.callCount() > 0);

    // Staging: a checkpoint of the first line goes out and hangs.
    buffer = "log\nx\n";
    const saving = file.saveNow();
    await advance(0);
    same(posts, [{ base_text: "log\n", text: "log\nx\n", base: "sha-a" }]);

    // The user keeps typing; its idle timer asks for a save while one is out.
    buffer = "log\nx\nx\n";
    void file.saveNow();

    // The socket syncs with the room as it was BEFORE the checkpoint.
    const sock = { destroy() {} };
    const c = file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };
    c.ws = sock;
    Y.applyUpdate(file.collab.doc, beforeCheckpoint, sock);
    c.onStatus("live");
    c.onReady();
    await advance(0);
    // The join is waiting on the checkpoint: nothing merged, nothing ready.
    same(ready, []);

    const frame = () => Y.applyUpdate(file.collab.doc, applied!, sock);
    if (frameFirst) frame();
    gate.resolve();
    await saving;
    await advance(0);
    // Joined on the HTTP answer alone: the document already holds the
    // checkpoint (the room's items) and the typing past it, once each.
    same(ready, [file.collab]);
    assert.equal(file.collab.text.toString(), "log\nx\nx\n");
    if (!frameFirst) frame();

    same(ready, [file.collab]);
    assert.equal(file.collab.text.toString(), "log\nx\nx\n");
    assert.equal(file.current(), "log\nx\nx\n");
    // The room's state and this tab's converge on the same two lines.
    Y.applyUpdate(room, Y.encodeStateAsUpdate(file.collab.doc));
    assert.equal(room.getText("body").toString(), "log\nx\nx\n");

    // One checkpoint, ever: the join handed over the rest, the hub writes it.
    await advance(60_000);
    assert.equal(posts.length, 1);
    same(puts, []);
    file.destroy();
  } finally {
    realTimers();
    connect.mock.restore();
  }
});

/* R12: a staging checkpoint fails AMBIGUOUSLY — a 502 the hub sends after it
   already put the text into the room (its snapshot failed), or no answer at
   all. Its items may still be on their way down the socket, which syncs a
   stale room first. The join must ask the room (a read-only peek) before it
   merges: if the room holds the checkpoint, only the typing past it is
   spliced; if not, the whole buffer is. Two x lines either way, never three. */
for (const { mode, name } of [
  { mode: "landed", name: "502 after the room applied it, plus a peer edit" },
  { mode: "dropped", name: "request never reached the hub" },
  { mode: "peekRetry", name: "502 after the room applied it, peek 503 once" },
] as const) test(`ambiguous staging checkpoint is peeked before the join (${name})`, async () => {
  const Y = await import("yjs");
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  fakeTimers();
  try {
    const b64 = (u8: Uint8Array) => btoa(String.fromCharCode(...u8));
    const room = new Y.Doc();
    room.getText("body").insert(0, "log\n");
    const beforeCheckpoint = Y.encodeStateAsUpdate(room);

    const posts: Array<{ base_text?: string; text?: string }> = [];
    const peeks: number[] = [];
    const puts: string[] = [];
    let peekAnswer = mode === "peekRetry" ? 503 : 200;
    let applied: Uint8Array | null = null;
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      if (init?.method === "PUT") {
        puts.push(String(url));
        return new Response(JSON.stringify({ sha: "sha-copy" }), { status: 200 });
      }
      const body = JSON.parse(String(init?.body)) as { base_text?: string; text?: string; peek?: boolean };
      const t = room.getText("body");
      if (body.peek) {
        peeks.push(peekAnswer);
        if (peekAnswer !== 200) return new Response(JSON.stringify({ error: "busy", retry: true }), { status: peekAnswer });
        return new Response(
          JSON.stringify({ ok: true, sha: "sha-a", text: t.toString(), update: b64(Y.encodeStateAsUpdate(room)) }),
          { status: 200 },
        );
      }
      posts.push(body);
      if (mode === "dropped") throw new TypeError("Failed to fetch");
      // The room applies the checkpoint, then its snapshot fails: 502.
      const before = Y.encodeStateVector(room);
      t.delete(0, t.length);
      t.insert(0, body.text ?? "");
      if (mode === "landed") t.insert(0, "peer\n"); // a co-editor, at the top
      applied = Y.encodeStateAsUpdate(room, before);
      return new Response("co-editing: storage unavailable", { status: 502 });
    }) as typeof fetch;

    let buffer = "log\n";
    const ready: unknown[] = [];
    const file = openSharedFile({
      apiBase: "/api/p/proj/", // a hub with a room route: staging until live
      path: "notes.md",
      seed: "log\n",
      baseSha: "sha-a",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: (c) => ready.push(c),
      onSolo: () => {},
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    assert.ok(connect.mock.callCount() > 0);

    buffer = "log\nx\n";
    await file.saveNow();
    assert.equal(posts.length, 1);
    // The user keeps typing past the failed checkpoint.
    buffer = "log\nx\nx\n";

    // The socket syncs with the room as it was BEFORE the checkpoint; the
    // frame carrying the checkpoint's items is still in flight.
    const sock = { destroy() {} };
    const c = file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };
    c.ws = sock;
    Y.applyUpdate(file.collab.doc, beforeCheckpoint, sock);
    c.onStatus("live");
    c.onReady();
    await advance(0);

    if (mode === "peekRetry") {
      // The peek failed: nothing merged, the editor stays on the buffer.
      same(peeks, [503]);
      same(ready, []);
      assert.equal(file.collab.text.toString(), "log\n");
      peekAnswer = 200;
      await advance(SAVE_RETRY_MS[0] - 1);
      same(ready, []);
      await advance(1);
      same(peeks, [503, 200]);
    } else {
      same(peeks, [200]);
    }
    const want = mode === "landed" ? "peer\nlog\nx\nx\n" : "log\nx\nx\n";
    same(ready, [file.collab]);
    assert.equal(file.collab.text.toString(), want);

    // The delayed frame arrives: the same item IDs, a no-op.
    if (applied) Y.applyUpdate(file.collab.doc, applied, sock);
    assert.equal(file.collab.text.toString(), want);
    assert.equal(file.current(), want);
    Y.applyUpdate(room, Y.encodeStateAsUpdate(file.collab.doc));
    assert.equal(room.getText("body").toString(), want);

    // No second checkpoint, no conflict copy: the hub writes the room.
    await advance(60_000);
    assert.equal(posts.length, 1);
    same(puts, []);
    file.destroy();
  } finally {
    realTimers();
    connect.mock.restore();
  }
});

/* A file whose room the hub holds: the socket has synced `seed` and the join
   is done. Every request is recorded; sync answers with the room's text.
   `net` holds requests back (`hold`) or answers them with an error. */
type Req = { url: string; method: string; body: string; keepalive?: boolean; ifMatch?: string };
async function heldRoom(o: { apiBase?: string } = {}) {
  const connect = mock.method(CollabDoc.prototype, "connect", () => {});
  Reflect.set(globalThis, "window", new EventTarget()); // what pagehide is dispatched on
  const reqs: Req[] = [];
  const ref: { file: SharedFile | null } = { file: null };
  // `head`: what a read of the file answers (Keep puts these bytes back).
  const net: { hold: Promise<void> | null; status: number; error: string; head: string } = {
    hold: null,
    status: 200,
    error: "",
    head: "a\nb\nc\n",
  };
  globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
    const headers = (init?.headers ?? {}) as Record<string, string>;
    const method = init?.method ?? "GET";
    reqs.push({ url: String(url), method, body: String(init?.body ?? ""), keepalive: init?.keepalive, ifMatch: headers["If-Match"] });
    const sha = `sha-${reqs.length}`;
    if (method === "GET") return new Response(net.head, { status: 200, headers: { ETag: '"sha-head"' } });
    const { status, error, hold } = net;
    if (hold) await hold;
    if (status !== 200) return new Response(error, { status });
    if (method === "PUT") return new Response(JSON.stringify({ sha }), { status: 200 });
    const body = JSON.parse(String(init?.body ?? "{}")) as { text?: string };
    let room = "";
    try {
      room = ref.file?.collab.text.toString() ?? "";
    } catch {
      // destroyed
    }
    return new Response(JSON.stringify({ ok: true, path: "notes.md", sha, text: body.text ?? room, merged: false }), {
      status: 200,
    });
  }) as typeof fetch;

  const seed = "a\nb\nc\n";
  const state = { buffer: seed };
  const states: string[] = [];
  const pendings: number[] = [];
  const asks: unknown[] = [];
  const saved: string[] = [];
  const file = openSharedFile({
    apiBase: o.apiBase ?? "/api/p/proj/",
    path: "notes.md",
    seed,
    baseSha: "sha-a",
    me: { name: "t", colour: "oklch(0.7 0.1 200)" },
    onReady: () => {},
    onSolo: () => {},
    onState: (s) => states.push(s),
    onSaved: (t) => saved.push(t),
    onPending: (n) => pendings.push(n),
    onShrinkRefused: (a) => asks.push(a),
    soloText: () => state.buffer,
    soloApply: (e) => {
      state.buffer = state.buffer.slice(0, e.from) + e.insert + state.buffer.slice(e.to);
    },
  });
  ref.file = file;
  const sock = { destroy() {} };
  const c = file.collab as unknown as { ws: unknown; onStatus: (s: string) => void; onReady: () => void };
  const goLive = async () => {
    c.ws = sock;
    file.collab.doc.transact(() => file.collab.text.insert(0, seed), sock);
    c.onStatus("live");
    c.onReady();
    await advance(0);
  };
  type Text = InstanceType<typeof CollabDoc>["text"];
  const peerEdit = (fn: (t: Text) => void) => file.collab.doc.transact(() => fn(file.collab.text), sock);
  const localEdit = (fn: (t: Text) => void) => file.collab.doc.transact(() => fn(file.collab.text));
  const addPeer = () => file.collab.awareness.states.set(4242, { user: { name: "peer" } });
  let destroyed = false;
  const destroy = () => {
    if (destroyed) return;
    destroyed = true;
    file.destroy();
  };
  const done = () => {
    destroy();
    connect.mock.restore();
    Reflect.deleteProperty(globalThis, "window");
  };
  return { file, reqs, net, state, states, pendings, asks, saved, c, goLive, peerEdit, localEdit, addPeer, destroy, done };
}

const occurrences = (hay: string, needle: string) => hay.split(needle).length - 1;

/* Eight people in one meeting note: every snapshot the hub writes is behind
   the room by the time it comes back through the change stream — peers kept
   typing, and deleting. It is this room's own past, so it is never the
   "someone else changed this file" banner; it is the file now, so `saved` and
   `base` move to it; and once it carries this tab's typing, "dirty" ends. An
   outsider's rewrite that never went through the room still is the banner. */
test("8 peers: a room snapshot behind newer edits is our past, never an outside write", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.addPeer();
    h.localEdit((t) => t.insert(t.length, "L\n"));
    const withL = h.file.current();
    assert.equal(h.states.at(-1), "dirty");

    // Peers type past it — more changes than the room's texts remembered —
    // and one deletes a line.
    for (let i = 0; i < 40; i++) h.peerEdit((t) => t.insert(0, `p${i}\n`));
    h.peerEdit((t) => t.delete(t.toString().indexOf("b\n"), 2));
    const roomNow = h.file.current();

    // The hub's snapshot from before all that arrives late.
    assert.equal(h.file.merge(withL, "sha-1"), "same");
    assert.equal(h.states.at(-1), "clean");
    assert.equal(h.file.current(), roomNow); // nothing spliced back in
    assert.equal(occurrences(h.file.current(), "L\n"), 1);
    assert.equal(occurrences(h.file.current(), "b\n"), 0);

    // Typing past a snapshot stays dirty until a snapshot carries it.
    const beforeM = h.file.current();
    h.localEdit((t) => t.insert(t.length, "M\n"));
    const withM = h.file.current();
    h.peerEdit((t) => t.insert(0, "q\n"));
    assert.equal(h.file.merge(beforeM, "sha-2"), "same");
    assert.equal(h.states.at(-1), "dirty");
    assert.equal(h.file.merge(withM, "sha-3"), "same");
    assert.equal(h.states.at(-1), "clean");
    const now = h.file.current();
    assert.equal(now, "q\n" + withM);

    // None of it went over HTTP: in a live room the hub writes the file.
    await advance(60_000);
    same(h.reqs, []);

    // An unconditional outside rewrite over unsaved typing is still refused.
    h.localEdit((t) => t.insert(t.length, "Z\n"));
    assert.equal(h.file.merge("rewritten by a CLI\n", "sha-x"), "blocked");
    assert.equal(h.file.current(), now + "Z\n");

    // `base` moved with the snapshots: the next write is built on sha-3.
    h.c.onStatus("offline");
    h.localEdit((t) => t.insert(t.length, "O\n"));
    await h.file.saveNow();
    assert.equal(h.reqs.length, 1);
    assert.ok(h.reqs[0].url.includes("ycollab/sync?path=notes.md"));
    const body = JSON.parse(h.reqs[0].body) as { base?: string; update?: string };
    assert.equal(body.base, "sha-3");
    assert.equal(typeof body.update, "string");
  } finally {
    h.done();
    realTimers();
  }
});

/* Done and Mod-s are an explicit save. In a live room that hands the hub the
   room's state — answered once storage holds it — but only when this tab has
   typing no snapshot carries yet: clean, or a bystander, sends nothing. */
test("saveNow in a live room sends the room's state only when there is typing to hand over", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    await h.file.saveNow();
    h.addPeer();
    h.peerEdit((t) => t.insert(0, "peer\n"));
    await h.file.saveNow();
    same(h.reqs, []);

    h.localEdit((t) => t.insert(t.length, "mine\n"));
    assert.equal(h.states.at(-1), "dirty");
    // Done and Mod-s at once: one request.
    const a = h.file.saveNow();
    const b = h.file.saveNow();
    await Promise.all([a, b]);
    assert.equal(h.reqs.length, 1);
    assert.equal(h.reqs[0].method, "POST");
    assert.ok(h.reqs[0].url.includes("ycollab/sync?path=notes.md"));
    assert.ok(!h.reqs[0].keepalive);
    same(Object.keys(JSON.parse(h.reqs[0].body) as object), ["update"]);
    same(h.states.slice(-2), ["saving", "clean"]);
    assert.equal(h.saved.at(-1), h.file.current());

    // Saved: a second Done sends nothing, and idle typing never writes.
    await h.file.saveNow();
    assert.equal(h.reqs.length, 1);
    h.localEdit((t) => t.insert(t.length, "more\n"));
    await advance(60_000);
    assert.equal(h.reqs.length, 1);

    // Leaving after Done sends nothing more either.
    await h.file.saveNow();
    assert.equal(h.reqs.length, 2);
    h.destroy();
    assert.equal(h.reqs.length, 2);
  } finally {
    h.done();
    realTimers();
  }
});

/* Closing the tab or reloading — no Done, no unmount — must not lose the
   last words: pagehide sends what this tab alone holds, keepalive, and the
   unmount that may follow finds it already sent. Nothing to hand over,
   nothing sent. */
for (const { mode, dirty } of [
  { mode: "live", dirty: true },
  { mode: "live", dirty: false },
  { mode: "offline", dirty: true },
  { mode: "offline", dirty: false },
  { mode: "staging", dirty: true },
  { mode: "staging", dirty: false },
  { mode: "solo", dirty: true },
  { mode: "solo", dirty: false },
] as const) test(`pagehide flushes keepalive exactly once (${mode}, dirty=${dirty})`, async () => {
  fakeTimers();
  const h = await heldRoom(mode === "solo" ? { apiBase: "/api/" } : {});
  try {
    if (mode === "solo") await advance(0); // no room route: solo
    if (mode === "live" || mode === "offline") {
      await h.goLive();
      h.addPeer();
      h.peerEdit((t) => t.insert(0, "peer\n")); // a bystander's view moves too
    }
    if (mode === "offline") h.c.onStatus("offline");
    if (dirty) {
      if (mode === "live" || mode === "offline") h.localEdit((t) => t.insert(t.length, "last words\n"));
      else h.state.buffer += "last words\n";
    }

    window.dispatchEvent(new Event("pagehide"));
    if (!dirty) {
      same(h.reqs, []);
    } else {
      assert.equal(h.reqs.length, 1);
      const r = h.reqs[0];
      assert.equal(r.keepalive, true);
      if (mode === "solo") {
        assert.equal(r.method, "PUT");
        assert.ok(r.url.includes("upload/content?path=notes.md"));
        assert.equal(r.ifMatch, "sha-a");
        assert.equal(r.body, "a\nb\nc\nlast words\n");
      } else {
        assert.equal(r.method, "POST");
        assert.ok(r.url.includes("ycollab/sync?path=notes.md"));
        const body = JSON.parse(r.body) as { update?: string; text?: string; park_on_conflict?: boolean };
        if (mode === "staging") {
          assert.equal(body.text, "a\nb\nc\nlast words\n");
          assert.equal(body.park_on_conflict, true);
        } else assert.equal(typeof body.update, "string");
      }
    }

    // The unmount that follows does not send it again.
    h.destroy();
    await advance(0);
    assert.equal(h.reqs.length, dirty ? 1 : 0);
  } finally {
    h.done();
    realTimers();
  }
});

/* Offline edits are acknowledged by watermark. Two saves while one flush is
   on the wire share it, and the loop sends once more for the typing that
   came after — so `pending` walks 1, 2, 1, 0 and never goes below what is
   really unsent, however the answers interleave. */
test("offline flushes are single-flight and acknowledged by watermark, never subtracted twice", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.c.onStatus("offline");
    const gate = Promise.withResolvers<void>();
    h.net.hold = gate.promise;
    h.localEdit((t) => t.insert(t.length, "x\n"));
    const a = h.file.saveNow();
    await advance(0);
    assert.equal(h.reqs.length, 1);
    h.localEdit((t) => t.insert(t.length, "y\n"));
    const b = h.file.saveNow(); // Done while the first is out: the same loop
    h.net.hold = null;
    await advance(0);
    assert.equal(h.reqs.length, 1);

    gate.resolve();
    await Promise.all([a, b]);
    assert.equal(h.reqs.length, 2);
    same(h.pendings, [1, 2, 1, 0]);

    // Everything acknowledged: the offline timer and the exit send nothing.
    await advance(60_000);
    h.destroy();
    assert.equal(h.reqs.length, 2);
  } finally {
    h.done();
    realTimers();
  }
});

/* The typing counts as handed over only once the hub answers. A pagehide
   whose keepalive failed — the page then restored from the back/forward
   cache — still has it to send on Done; one still on the wire is not sent
   twice in the same unload, but a page shown again may send it on its next
   exit. */
test("a failed or bfcache-restored pagehide flush leaves the typing to be sent again", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.localEdit((t) => t.insert(t.length, "last words\n"));
    h.net.status = 503;
    window.dispatchEvent(new Event("pagehide"));
    await advance(0);
    assert.equal(h.reqs.length, 1);
    assert.equal(h.reqs[0].keepalive, true);

    // The page stayed: Done still sends it.
    h.net.status = 200;
    await h.file.saveNow();
    assert.equal(h.reqs.length, 2);
    assert.ok(!h.reqs[1].keepalive);
    assert.equal(h.states.at(-1), "clean");

    h.localEdit((t) => t.insert(t.length, "more\n"));
    const gate = Promise.withResolvers<void>();
    h.net.hold = gate.promise;
    window.dispatchEvent(new Event("pagehide"));
    window.dispatchEvent(new Event("pagehide")); // the same state is on the wire
    assert.equal(h.reqs.length, 3);
    window.dispatchEvent(new Event("pageshow")); // back from the bfcache
    window.dispatchEvent(new Event("pagehide"));
    assert.equal(h.reqs.length, 4);

    h.net.hold = null;
    gate.resolve();
    await advance(0);
    // Landed: nothing left for Done or the unmount.
    await h.file.saveNow();
    h.destroy();
    assert.equal(h.reqs.length, 4);
  } finally {
    h.done();
    realTimers();
  }
});

/* Done in a live room on a buffer emptied against what the file is known to
   hold: the user is asked at once, before any request (the caller stays on
   the editor), a second Done sends nothing while the question is open, and
   confirming writes it. */
test("an explicit live save of a sharp shrink asks the user first and waits", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.localEdit((t) => t.delete(0, t.length));
    await h.file.saveNow();
    same(h.reqs, []);
    assert.equal(h.asks.length, 1);
    assert.equal(h.file.askingShrink(), true);
    assert.equal(h.states.at(-1), "error");

    await h.file.saveNow();
    await advance(60_000); // nor does the idle check stack a second dialog
    same(h.reqs, []);
    assert.equal(h.asks.length, 1);

    await (h.asks[0] as ShrinkAsk).confirm();
    assert.equal(h.reqs.length, 1);
    const body = JSON.parse(h.reqs[0].body) as { confirm_shrink?: boolean };
    assert.equal(body.confirm_shrink, true);
    assert.equal(h.file.askingShrink(), false);
    assert.equal(h.states.at(-1), "clean");
  } finally {
    h.done();
    realTimers();
  }
});

/* The room's remembered past starts after `saved`. Once a snapshot with "d"
   is the file, a restore to the text from before "d" — made outside the
   room, over unsaved typing — is not this room's history: it is refused. */
test("a restore to a state older than `saved` is an outside write, not the room's past", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.localEdit((t) => t.insert(t.length, "d\n"));
    assert.equal(h.file.merge(h.file.current(), "sha-1"), "same");
    h.localEdit((t) => t.insert(t.length, "e\n"));
    assert.equal(h.file.merge("a\nb\nc\n", "sha-r"), "blocked");
    assert.equal(h.file.current(), "a\nb\nc\nd\ne\n");
  } finally {
    h.done();
    realTimers();
  }
});

/* The same shrink with the socket down: Done asks at once and sends nothing
   (no retry can land an unconfirmed shrink), nothing is sent while the
   question is open, and confirming hands the queued edits over and leaves
   nothing pending. */
test("an offline explicit save of a sharp shrink asks at once and never sends unconfirmed", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.c.onStatus("offline");
    h.localEdit((t) => t.delete(0, t.length));
    await h.file.saveNow();
    same(h.reqs, []);
    assert.equal(h.file.askingShrink(), true);
    assert.equal(h.asks.length, 1);
    assert.equal(h.pendings.at(-1), 1);

    await advance(60_000); // the offline timer and the idle check stay quiet
    same(h.reqs, []);
    assert.equal(h.asks.length, 1);

    await (h.asks[0] as ShrinkAsk).confirm();
    assert.equal(h.reqs.length, 1);
    const body = JSON.parse(h.reqs[0].body) as { confirm_shrink?: boolean; update?: string };
    assert.equal(body.confirm_shrink, true);
    assert.equal(typeof body.update, "string");
    assert.equal(h.file.askingShrink(), false);
    assert.equal(h.pendings.at(-1), 0);
    assert.equal(h.states.at(-1), "clean");

    await advance(60_000);
    h.destroy();
    assert.equal(h.reqs.length, 1);
  } finally {
    h.done();
    realTimers();
  }
});

/* Done on a sharply shrunk buffer while the network never answers, in every
   mode: the question opens at once and nothing is sent, so Done stays on the
   editor instead of timing out past an unanswered save. */
for (const mode of ["live", "offline", "staging", "solo"] as const) {
  test(`Done on a sharp shrink asks at once and sends nothing, the network hung (${mode})`, async () => {
    fakeTimers();
    const h = await heldRoom(mode === "solo" ? { apiBase: "/api/" } : {});
    try {
      if (mode === "solo") await advance(0); // no room route: solo
      if (mode === "live" || mode === "offline") await h.goLive();
      if (mode === "offline") h.c.onStatus("offline");
      h.net.hold = Promise.withResolvers<void>().promise; // never answers
      if (mode === "live" || mode === "offline") h.localEdit((t) => t.delete(0, t.length));
      else h.state.buffer = "";

      let settled = false;
      void h.file.saveNow().then(() => {
        settled = true;
      });
      await settle(); // no timer runs: the answer is local
      assert.equal(settled, true);
      assert.equal(h.file.askingShrink(), true);
      assert.equal(h.asks.length, 1);
      same(h.reqs, []);
    } finally {
      h.done();
      realTimers();
    }
  });
}

/* A shrink only the hub can see: the read seeded an empty buffer (`saved`
   is "", so nothing local is a shrink) and the hub refuses the save 422,
   ten seconds later. Done waits the save out — there is no timer to leave
   on — so the question opens when the answer comes, and Done stays. */
test("a shrink the hub refuses 422 after a slow answer still asks before Done leaves", async () => {
  fakeTimers();
  try {
    const reqs: string[] = [];
    globalThis.fetch = (async (url: RequestInfo | URL, init?: RequestInit) => {
      reqs.push((init?.method ?? "GET") + " " + decodeURIComponent(String(url)));
      // The hub answers late (fake clock: advance() moves it).
      const late = Promise.withResolvers<void>();
      setTimeout(late.resolve, 10_000);
      await late.promise;
      return new Response(JSON.stringify({ error: "would cut most of the file", shrink: true }), { status: 422 });
    }) as typeof fetch;
    let buffer = "";
    let solo = false;
    const asks: ShrinkAsk[] = [];
    const file = openSharedFile({
      apiBase: "/api/",
      path: "notes.md",
      seed: "",
      baseSha: "sha-big",
      me: { name: "t", colour: "oklch(0.7 0.1 200)" },
      onReady: () => {},
      onSolo: () => {
        solo = true;
      },
      onShrinkRefused: (a) => asks.push(a),
      soloText: () => buffer,
      soloApply: (e) => {
        buffer = buffer.slice(0, e.from) + e.insert + buffer.slice(e.to);
      },
    });
    await waitFor(() => assert.equal(solo, true));

    buffer = "x\n";
    let settled = false;
    void file.saveNow().then(() => {
      settled = true;
    });
    await advance(9_999);
    assert.equal(reqs.length, 1);
    assert.equal(settled, false);
    assert.equal(file.askingShrink(), false);
    await advance(1);
    assert.equal(settled, true);
    assert.equal(file.askingShrink(), true);
    assert.equal(asks.length, 1);
    file.destroy();
  } finally {
    realTimers();
  }
});

/* A live room over a non-empty `saved`: nothing local is a shrink, but a
   peer grew the file and the hub snapshotted it before this tab heard. The
   hub refuses the room's state 422, ten seconds later; saveNow waits for that
   answer and leaves the question open, so Done stays on the editor. */
test("a live save the hub refuses 422 for a shrink only it can see asks, however late", async () => {
  fakeTimers();
  const h = await heldRoom();
  try {
    await h.goLive();
    h.localEdit((t) => t.insert(t.length, "d\n")); // grows `saved`: no local shrink
    h.net.status = 422;
    h.net.error = JSON.stringify({ error: "would cut most of the file", shrink: true });
    const late = Promise.withResolvers<void>();
    setTimeout(late.resolve, 10_000); // fake clock: advance() moves it
    h.net.hold = late.promise;

    let settled = false;
    void h.file.saveNow().then(() => {
      settled = true;
    });
    await advance(9_999);
    assert.equal(h.reqs.length, 1);
    assert.equal(settled, false);
    assert.equal(h.file.askingShrink(), false);
    await advance(1);
    assert.equal(settled, true);
    assert.equal(h.file.askingShrink(), true);
    assert.equal(h.asks.length, 1);
  } finally {
    h.done();
    realTimers();
  }
});

/* Typing while Done's save is on the wire is saved — and shrink-checked —
   before saveNow answers: a second pass sends it, or a shrink in it opens the
   question (asked here in solo, refused by the hub in a live room), and
   saveNow answers false so Done stays on the editor. */
for (const mode of ["live", "solo"] as const) {
  for (const followUp of ["typing", "shrink"] as const) {
    test(`typing while saveNow is on the wire is saved before it answers (${mode}, ${followUp})`, async () => {
      fakeTimers();
      const h = await heldRoom(mode === "solo" ? { apiBase: "/api/" } : {});
      try {
        if (mode === "solo") await advance(0); // no room route: solo
        else await h.goLive();
        if (mode === "live") h.localEdit((t) => t.insert(t.length, "d\n"));
        else h.state.buffer = "a\nb\nc\nd\n";
        const gate = Promise.withResolvers<void>();
        h.net.hold = gate.promise;
        let result: boolean | null = null;
        void h.file.saveNow().then((r) => {
          result = r;
        });
        await advance(0);
        assert.equal(h.reqs.length, 1);

        // Typed while the first pass is out.
        if (followUp === "typing") {
          if (mode === "live") h.localEdit((t) => t.insert(t.length, "e\n"));
          else h.state.buffer = "a\nb\nc\nd\ne\n";
        } else if (mode === "live") {
          h.localEdit((t) => t.delete(0, t.length));
          // The room's own answer carries the deletion, so only the hub can
          // tell it is a shrink: it refuses the second pass.
          h.net.status = 422;
          h.net.error = JSON.stringify({ error: "this would empty the file", shrink: true });
        } else h.state.buffer = "";
        h.net.hold = null;
        gate.resolve();
        await advance(0);

        if (followUp === "typing") {
          assert.equal(result, true);
          assert.equal(h.reqs.length, 2);
          if (mode === "solo") assert.equal(h.reqs[1].body, "a\nb\nc\nd\ne\n");
          assert.equal(h.states.at(-1), "clean");
        } else {
          assert.equal(result, false);
          assert.equal(h.file.askingShrink(), true);
          assert.equal(h.asks.length, 1);
          assert.equal(h.reqs.length, mode === "live" ? 2 : 1); // solo asks before sending
        }
      } finally {
        h.done();
        realTimers();
      }
    });
  }
}

/* A save that fails leaves the buffer unsaved: saveNow answers false (its
   retry stays armed, Done stays on the editor), and true once a save lands. */
for (const mode of ["live", "offline", "solo"] as const) {
  test(`saveNow answers false while a save fails and true once one lands (${mode})`, async () => {
    fakeTimers();
    const h = await heldRoom(mode === "solo" ? { apiBase: "/api/" } : {});
    try {
      if (mode === "solo") await advance(0); // no room route: solo
      else await h.goLive();
      if (mode === "offline") h.c.onStatus("offline");
      if (mode === "solo") h.state.buffer = "a\nb\nc\nd\n";
      else h.localEdit((t) => t.insert(t.length, "d\n"));

      h.net.status = 503;
      h.net.error = "unavailable";
      assert.equal(await h.file.saveNow(), false);
      assert.equal(h.reqs.length, 1);

      h.net.status = 200;
      assert.equal(await h.file.saveNow(), true);
      assert.equal(h.reqs.length, 2);
      // Nothing owed now: true at once, nothing sent.
      assert.equal(await h.file.saveNow(), true);
      assert.equal(h.reqs.length, 2);
    } finally {
      h.done();
      realTimers();
    }
  });
}

/* Keep, with the socket down, after Done asked about a sharp shrink: the
   deletion is queued in this tab's document, so putting the file's bytes
   back is an edit the room must get too — over HTTP at once, or, if that
   fails, still queued for the retry, pagehide and the unmount. */
for (const keepAnswer of [200, 503]) {
  test(`offline Keep after a shrink refusal hands the restored text to the room (answer ${keepAnswer})`, async () => {
    const Y = await import("yjs");
    const roomText = (b64: string) => {
      const d = new Y.Doc();
      Y.applyUpdate(d, Uint8Array.from(atob(b64), (c) => c.charCodeAt(0)));
      return d.getText("body").toString();
    };
    fakeTimers();
    const h = await heldRoom();
    const posts = () => h.reqs.filter((r) => r.method === "POST");
    try {
      await h.goLive();
      h.c.onStatus("offline");
      h.localEdit((t) => t.delete(0, t.length));
      await h.file.saveNow();
      same(posts(), []); // asked before anything was sent
      assert.equal(h.file.askingShrink(), true);

      h.net.status = keepAnswer;
      h.net.error = "unavailable";
      await (h.asks[0] as ShrinkAsk).keep();
      assert.equal(h.file.askingShrink(), false);
      assert.equal(h.file.current(), "a\nb\nc\n");
      assert.equal(posts().length, 1);
      const sent = JSON.parse(posts()[0].body) as { update?: string; confirm_shrink?: boolean };
      assert.equal(sent.confirm_shrink, undefined);
      assert.equal(roomText(sent.update ?? ""), "a\nb\nc\n");

      if (keepAnswer === 200) {
        assert.equal(h.pendings.at(-1), 0);
        assert.equal(h.states.at(-1), "clean");
        await advance(60_000);
        h.destroy();
        assert.equal(posts().length, 1);
      } else {
        // Not handed over: still queued, and a closing tab still sends it.
        assert.ok((h.pendings.at(-1) ?? 0) > 0);
        assert.equal(h.states.at(-1), "dirty");
        window.dispatchEvent(new Event("pagehide"));
        assert.equal(posts().length, 2);
        assert.equal(posts()[1].keepalive, true);
        const exit = JSON.parse(posts()[1].body) as { update?: string };
        assert.equal(roomText(exit.update ?? ""), "a\nb\nc\n");
      }
    } finally {
      h.done();
      realTimers();
    }
  });
}

/* VisualEdit's Done: the iframe's last patch may still be in the postMessage
   queue when Done asks the file to save. Saving then sees a clean buffer and
   answers true, and the editor closes before the shrink check or the failure
   that patch would meet. Draining first — flush request, the frame posts what
   it holds, then `bd-edit:flushed` on the same channel — puts the patch in
   the document before the save decides. `frame` stands in for the iframe:
   `deliver` is a message arriving on window "message", later, in order. */
function frameHarness(answer: (body: string) => Response) {
  const puts: string[] = [];
  globalThis.fetch = (async (_url: RequestInfo | URL, init?: RequestInit) => {
    puts.push(String(init?.body));
    return answer(String(init?.body));
  }) as typeof fetch;
  const state = { buffer: "a\nb\nc\n" };
  const asks: ShrinkAsk[] = [];
  let solo = false;
  const file = openSharedFile({
    apiBase: "/api/",
    path: "page.html",
    seed: "a\nb\nc\n",
    baseSha: "sha-a",
    me: { name: "t", colour: "oklch(0.7 0.1 200)" },
    onReady: () => {},
    onSolo: () => {
      solo = true;
    },
    onShrinkRefused: (a) => asks.push(a),
    soloText: () => state.buffer,
    soloApply: (e) => {
      state.buffer = state.buffer.slice(0, e.from) + e.insert + state.buffer.slice(e.to);
    },
  });
  const channel = frameChannel();
  type Msg = { type: "patch"; text: string } | { type: "flushed"; id: number };
  // window "message" delivery: a later task, in posting order.
  const deliver = (msg: Msg) =>
    setImmediate(() =>
      channel.enqueue(async () => {
        await Promise.resolve(); // anchoring is async; order must still hold
        if (msg.type === "patch") state.buffer = msg.text;
        else channel.flushed(msg.id);
      }),
    );
  const frame = { gone: false, answers: true, flushes: 0 };
  const post = (m: { type: "bd-edit:flush"; id: number }) => {
    frame.flushes++;
    if (frame.answers) deliver({ type: "flushed", id: m.id });
  };
  // VisualEdit's exposed saveNow.
  const saveNow = async () => {
    await channel.drain(post, () => frame.gone);
    return file.saveNow();
  };
  const ready = () => waitFor(() => assert.equal(solo, true));
  return { file, state, asks, puts, channel, deliver, frame, saveNow, ready };
}

test("Done drains the frame's queued patch before saving: a sharp shrink in it asks", async () => {
  const h = frameHarness(() => new Response(JSON.stringify({ sha: "sha-b" }), { status: 200 }));
  try {
    await h.ready();
    // The user deleted the page and clicked Done at once: the patch is still
    // on its way when Done runs.
    h.deliver({ type: "patch", text: "" });
    assert.equal(await h.saveNow(), false);
    assert.equal(h.frame.flushes, 1);
    assert.equal(h.state.buffer, "");
    assert.equal(h.file.askingShrink(), true);
    assert.equal(h.asks.length, 1);
    same(h.puts, []); // asked before anything was sent
  } finally {
    h.channel.close();
    h.file.destroy();
  }
});

test("Done drains the frame's queued patch before saving: a failed save keeps the editor", async () => {
  const h = frameHarness(() => new Response("unavailable", { status: 503 }));
  try {
    await h.ready();
    h.deliver({ type: "patch", text: "a\nb\nc\nd\n" });
    assert.equal(await h.saveNow(), false); // the patch met the 503
    same(h.puts, ["a\nb\nc\nd\n"]);
  } finally {
    h.channel.close();
    h.file.destroy();
    await settle();
  }
});

test("Done's drain lands every queued patch in order, then saves once", async () => {
  const h = frameHarness((body) => new Response(JSON.stringify({ sha: "sha-" + body.length }), { status: 200 }));
  try {
    await h.ready();
    h.deliver({ type: "patch", text: "a\nb\nc\nd\n" });
    h.deliver({ type: "patch", text: "a\nb\nc\nd\ne\n" });
    assert.equal(await h.saveNow(), true);
    same(h.puts, ["a\nb\nc\nd\ne\n"]);
    // Nothing held, nothing owed: the next Done answers without a request.
    assert.equal(await h.saveNow(), true);
    assert.equal(h.frame.flushes, 2);
    same(h.puts, ["a\nb\nc\nd\ne\n"]);
  } finally {
    h.channel.close();
    h.file.destroy();
  }
});

/* A frame that never answers — reloaded, replaced, never ready — is not
   waited on: the drain checks it is still there and stops once it is not.
   One that is there is waited on, with no timer to leave on. */
test("Done's drain waits on a live frame and stops once the frame is gone", async () => {
  fakeTimers();
  const h = frameHarness(() => new Response(JSON.stringify({ sha: "sha-b" }), { status: 200 }));
  try {
    await h.ready();
    h.frame.answers = false;
    let result: boolean | null = null;
    void h.saveNow().then((r) => {
      result = r;
    });
    await advance(60_000);
    assert.equal(result, null); // still there: still waited on
    h.frame.gone = true;
    await advance(50);
    assert.equal(result, true); // gone: nothing it held can arrive; clean
    same(h.puts, []);
  } finally {
    h.channel.close();
    h.file.destroy();
    realTimers();
  }
});
