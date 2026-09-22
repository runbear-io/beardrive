import { test, expect } from "@playwright/test";
import { login, ADMIN, MEMBER, wikiId } from "./helpers";

/* The network budget: what an open tab costs when nothing is happening.

   This is the regression gate for docs/network-efficiency-prd.md. It asserts
   request COUNTS and never byte totals — the seeded project here is a handful
   of files, so bytes measured against it would prove nothing about the
   1.65 MB tree that motivated the work. Counts transfer; bytes do not.

   Why a real minute of waiting: the polls this replaced ran at 15s (tree),
   30s (projects) and 60s (heat), so a window shorter than the slowest one
   would pass against code that still polls. The whole point is to fail then. */

const IDLE_MS = 70_000;

test("an idle tab does not fetch the project down again", async ({ page }) => {
  test.setTimeout(IDLE_MS + 60_000);
  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/`);
  await page.waitForSelector("#sidebar");
  // Everything the first paint asks for, including the SSE stream, lands
  // before the window opens — this measures steady state, not load.
  await page.waitForTimeout(3_000);

  const seen: string[] = [];
  const host = new URL(page.url()).host;
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.host !== host) return; // analytics is somebody else's budget
    seen.push(r.method() + " " + u.pathname + u.search);
  });
  await page.waitForTimeout(IDLE_MS);

  const hits = (s: string) => seen.filter((r) => r.includes(s));
  // The three that used to run on timers. A live change stream already says
  // when any of them is wrong, so an idle tab must ask for none of them.
  expect(hits("/tree")).toHaveLength(0);
  expect(hits("/heat")).toHaveLength(0);
  expect(hits("/api/projects")).toHaveLength(0);

  /* Presence used to be the one thing left ticking in here: a POST every 10s,
     per TAB, from somebody reading a page. In production that was ~19% of
     every request the hub served (docs/hub-load-prd.md Stage 3).

     It is gone. A member holding an open change stream is here by definition,
     so the hub keys presence to that connection and the client announces only
     when the answer changes — arriving, moving to another file, leaving.
     Idling is none of those. */
  expect(
    hits("/presence"),
    "presence announced itself while nothing happened; the heartbeat is back",
  ).toHaveLength(0);

  // Only the SSE stream remains: one request made before the window opened,
  // allowed to reconnect within it.
  const other = seen.filter((r) => !r.includes("/events"));
  expect(other, `unexpected idle traffic: ${other.join(", ")}`).toHaveLength(0);
});

/* What does leave the hub is compressed.

   Asserted on the wire rather than by byte counts, for the same reason as
   above: the seeded project is small, so the ratio here would prove nothing.
   The header is the behaviour — a browser that asked for gzip and got raw
   JSON is the bug, whatever the size happened to be. */
test("responses are compressed, and streams are left alone", async ({ page }) => {
  const enc = new Map<string, string | null>();
  page.on("response", async (r) => {
    const name = new URL(r.url()).pathname.split("/").pop() || "";
    if (["tree", "events", "collab"].includes(name) && !enc.has(name)) {
      enc.set(name, await r.headerValue("content-encoding").catch(() => null));
    }
  });

  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/`);
  await page.waitForSelector("#sidebar");
  await page.waitForTimeout(2_000); // the SSE stream opens just after paint

  expect(enc.get("tree"), "the biggest response on the wire").toBe("gzip");
  // Buffering an event stream to save bytes trades away the whole point of
  // the stream — and once took live updates and co-editing down with it.
  expect(enc.get("events") ?? "").not.toBe("gzip");
});

/* Moving the caret is not a change to anything.

   Every awareness update used to be its own POST, so arrow-keying around a
   file — or holding a key down — spent one request per keypress. Alone in a
   document that is a request per keystroke to draw a caret nobody can see. */
test("moving the cursor alone costs nothing", async ({ page }) => {
  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/edit/guide.md`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.waitForTimeout(1_500); // the room joins and announces once

  const posts: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "POST" && r.url().includes("/collab")) posts.push(r.url());
  });

  await page.locator(".cm-host .cm-content").click();
  for (let i = 0; i < 20; i++) {
    await page.keyboard.press(i % 2 ? "ArrowRight" : "ArrowDown");
    await page.waitForTimeout(60);
  }
  await page.waitForTimeout(1_000); // past the coalescing window

  expect(posts, `cursor moves sent ${posts.length} requests`).toHaveLength(0);
  // ...and the document is untouched: a caret is not an edit.
  await expect(page.locator("#editor-state")).not.toHaveAttribute("data-state", "dirty");
});

/* Typing puts one request on the wire at a time.

   The uplink is HTTP: the stream carries peers' updates DOWN, but everything
   this client produces leaves as a POST. `timer` used to be cleared before
   the await, so a keystroke landing mid-request armed a second flush that
   started while the first was still going — several POSTs in flight at once,
   each with its own headers, cookie and round trip.

   Serialized, never cancelled: Yjs updates are deltas, and dropping one in
   flight would delete those keystrokes from every peer. */
test("fast typing does not stack up requests", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/edit/guide.md`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.waitForTimeout(1_500);

  /* Latency has to be induced, or this proves nothing.

     Against a hub on localhost a POST completes in a few milliseconds —
     inside the coalescing window — so the second flush never starts before
     the first finishes and even the un-serialized code looks fine. Overlap
     needs a request slower than the window, which is every real network and
     no local one. 300ms against a 120ms window guarantees it. */
  await page.route("**/collab*", async (route) => {
    if (route.request().method() !== "POST") return route.continue();
    await new Promise((r) => setTimeout(r, 300));
    await route.continue();
  });

  let inFlight = 0;
  let peak = 0;
  let posts = 0;
  const isUpdate = (u: string) => u.includes("/collab");
  page.on("request", (r) => {
    if (r.method() === "POST" && isUpdate(r.url())) {
      posts++;
      peak = Math.max(peak, ++inFlight);
    }
  });
  const done = (r: { method(): string; url(): string }) => {
    if (r.method() === "POST" && isUpdate(r.url())) inFlight--;
  };
  page.on("requestfinished", done);
  page.on("requestfailed", done);

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("End");
  // Faster than the coalescing window, for longer than one request takes.
  for (let i = 0; i < 60; i++) await page.keyboard.type("x", { delay: 10 });
  await page.waitForTimeout(4_000); // drain, at 300ms a request

  expect(peak, `${peak} collab POSTs were in flight at once`).toBeLessThanOrEqual(1);
  // And the batching still batches: 60 keystrokes must not be 60 requests.
  expect(posts, `${posts} POSTs for 60 keystrokes`).toBeLessThan(20);
  await expect(page.locator("#editor-state")).toHaveAttribute("data-state", "clean");
});

/* What a teammate's write costs everyone else.

   A one-path change used to invalidate the whole tree, the whole heat map,
   and every cached file body in every project open in the tab — twice per
   frame, because the bare ["text"] prefix was invalidated in two places. On a
   real project that is ~1.8 MB to say that one file moved.

   The frame names the path. This asserts the client acts like it. */
test("a peer's write costs the rest of us almost nothing", async ({ page, browser }) => {
  test.setTimeout(90_000);
  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/`);
  await page.waitForSelector("#sidebar");
  await page.waitForTimeout(2_500); // first paint settles

  const seen: string[] = [];
  const host = new URL(page.url()).host;
  page.on("request", (r) => {
    const u = new URL(r.url());
    if (u.host !== host) return;
    if (u.pathname.endsWith("/presence") || u.pathname.endsWith("/events")) return;
    seen.push(u.pathname + u.search);
  });

  // Somebody else writes one file, from their own session.
  const peer = await (await browser.newContext()).newPage();
  await login(peer, MEMBER);
  const r = await peer.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("peer-cost.md")}`,
    { data: "# written by a teammate\n" },
  );
  expect(r.ok()).toBeTruthy();
  await page.waitForTimeout(5_000); // past the 2s coalescing window

  const hits = (s: string) => seen.filter((u) => u.includes(s));
  // Heat is READ telemetry. A write cannot move it, so it must not be asked.
  expect(hits("/heat"), `heat refetched: ${hits("/heat").join(", ")}`).toHaveLength(0);
  // The tree is the expensive one, and one write may refresh it at most once.
  expect(hits("/tree").length).toBeLessThanOrEqual(1);
  expect(seen.length, `a one-file write cost ${seen.length} requests: ${seen.join(", ")}`)
    .toBeLessThanOrEqual(2);
});
