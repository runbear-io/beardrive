import { test, expect } from "@playwright/test";
import { login, ADMIN, wikiId } from "./helpers";

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

  // What is left is the presence heartbeat (10s) and nothing else. The SSE
  // stream is one request made before the window and is allowed to reconnect.
  const other = seen.filter((r) => !r.includes("/presence") && !r.includes("/events"));
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
