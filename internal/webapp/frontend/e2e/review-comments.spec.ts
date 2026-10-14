import { test, expect, Page } from "@playwright/test";
import { login } from "./helpers";

/* Review comments end to end: select text on a rendered file, comment, see the
   thread in the panel and on the page, resolve it — for markdown rendered in
   the app and for HTML rendered in the sandboxed iframe, where the selecting
   happens inside the frame's own bootstrap.

   A project of its own, files removed afterwards: see inline-edit.spec.ts for
   why nothing is added to the shared seed. The file name sorts it after
   home/hub.spec.ts, which assume the seeded project is the only one a fresh
   sign-in lands on — the same place inline-edit.spec.ts already runs. */

const PROJECT = "review-comments-e2e";
let projectId = "";
let n = 0;
const litter: (() => Promise<void>)[] = [];

test.afterEach(async () => {
  while (litter.length) await litter.pop()!().catch(() => {});
});

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  return projectId;
}

async function ownFile(page: Page, id: string, ext: string, body: string): Promise<string> {
  const name = `review-${Date.now()}-${n++}.${ext}`;
  const r = await page.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`, {
    data: body,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
  expect(r.ok()).toBeTruthy();
  litter.push(async () => {
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } });
  });
  return name;
}

async function threads(page: Page, id: string, path: string, status = "all") {
  const r = await page.request.get(`/api/p/${id}/comments?status=${status}&path=${encodeURIComponent(path)}`);
  expect(r.ok()).toBeTruthy();
  return (await r.json()).comments as {
    id: string;
    resolved: boolean;
    anchor: { quote?: string; element?: string; prefix?: string };
    messages: { body: string }[];
  }[];
}

// Selects `text` (which may span inline elements) inside root and lets go of
// the mouse, the way a reader drag-selecting it would end.
const selectText = (text: string) => {
  const root = document.body;
  const walker = document.createTreeWalker(root, NodeFilter.SHOW_TEXT);
  let all = "";
  const nodes: [Text, number][] = [];
  for (let t = walker.nextNode() as Text | null; t; t = walker.nextNode() as Text | null) {
    nodes.push([t, all.length]);
    all += t.data;
  }
  const at = all.indexOf(text);
  if (at < 0) throw new Error("text not on the page: " + text);
  const find = (i: number, end: boolean) => {
    for (let k = nodes.length - 1; k >= 0; k--) {
      const [t, s] = nodes[k];
      if (end ? s < i : s <= i) return [t, i - s] as const;
    }
    throw new Error("no node");
  };
  const [sn, so] = find(at, false);
  const [en, eo] = find(at + text.length, true);
  const r = document.createRange();
  r.setStart(sn, so);
  r.setEnd(en, eo);
  const sel = getSelection()!;
  sel.removeAllRanges();
  sel.addRange(r);
  (sn.parentElement || root).dispatchEvent(new MouseEvent("mouseup", { bubbles: true }));
};

test("comment on selected markdown text, then resolve it", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, "md", "# Q3\n\nRevenue grew by **12%** this quarter.\n\nCosts were flat.\n");
  await page.goto(`/${id}/${file}`);
  await expect(page.locator("#content h1")).toHaveText("Q3");

  await page.evaluate(selectText, "grew by 12%");
  await page.click(".cm-float");
  await page.fill(".cm-compose textarea", "Finance says 14%.");
  await page.click(".cm-compose .pbtn");

  const card = page.locator(".cm-panel .cm-card");
  await expect(card).toHaveCount(1);
  await expect(card).toContainText("Finance says 14%.");
  await expect(card.locator(".cm-q")).toHaveText("grew by 12%");
  // The stored context is on screen too, not only in what an agent reads.
  await expect(card.locator(".cm-quote")).toContainText("Revenue");
  await expect(page.locator("#comments-btn .cm-count")).toHaveText("1");
  // The quoted text carries a numbered pin on the page.
  await expect(page.locator(".cm-pin")).toHaveText("1");

  const stored = await threads(page, id, file);
  expect(stored).toHaveLength(1);
  expect(stored[0].anchor.quote).toBe("grew by 12%");
  expect(stored[0].anchor.prefix).toContain("Revenue");

  // A deep link (what `bdrive comments` prints) opens the panel on the thread.
  await page.goto(`/${id}/${file}?comment=${stored[0].id}`);
  await expect(page.locator(".cm-card.active")).toContainText("Finance says 14%.");

  await page.locator(".cm-card.active textarea.cm-reply").fill("Changed it.");
  await page.click(".cm-card.active >> text=Reply & resolve");
  await expect(page.locator(".cm-panel .cm-tabs [aria-selected=true]")).toContainText("Open");
  await expect(page.locator(".cm-pin")).toHaveCount(0);
  const after = await threads(page, id, file);
  expect(after[0].resolved).toBe(true);
  expect(after[0].messages.map((m) => m.body)).toEqual(["Finance says 14%.", "Changed it."]);
});

const PAGE = `<!DOCTYPE html>
<html><head><title>Deck</title></head>
<body>
  <h1>Launch plan</h1>
  <p>We ship the beta on <b>March 3</b> to every design partner.</p>
  <div id="chart" style="width:200px;height:80px;background:#ddd"></div>
  <script>document.addEventListener("click", () => { document.title = "page saw a click"; });</script>
</body></html>
`;

test("comment on text and on an element inside an HTML page", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, "html", PAGE);
  await page.goto(`/${id}/${file}`);
  const frame = page.frameLocator("iframe.htmlview");
  await expect(frame.locator("h1")).toHaveText("Launch plan");
  const inner = page.frames().find((f) => f.url().includes("comment=1"));
  expect(inner, "the HTML renders its commentable variant").toBeTruthy();

  // Text: selected inside the frame, composed in the app.
  await expect.poll(() => inner!.evaluate(() => !!document.querySelector("bd-comment-layer"))).toBe(true);
  await inner!.evaluate(selectText, "beta on March 3");
  await page.click(".cm-float");
  await page.fill(".cm-compose textarea", "Is the date final?");
  await page.click(".cm-compose .pbtn");
  await expect(page.locator(".cm-panel .cm-card")).toHaveCount(1);

  // An element with no text to select: pick it.
  await page.click(".cm-pickbar button");
  await frame.locator("#chart").click();
  await expect(page.locator(".cm-compose .cm-el")).toContainText("div#chart");
  // The page's own click handler never saw the pick.
  expect(await inner!.evaluate(() => document.title)).toBe("Deck");
  await page.fill(".cm-compose textarea", "Chart is empty.");
  await page.click(".cm-compose .pbtn");
  await expect(page.locator(".cm-panel .cm-card")).toHaveCount(2);

  const stored = await threads(page, id, file);
  const quotes = stored.map((t) => t.anchor.quote || t.anchor.element).sort();
  expect(quotes).toEqual(["beta on March 3", "div#chart"]);
});
