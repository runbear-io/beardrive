import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// Phase 2: tree, folder listings (heat dots + change feed), file views
// (markdown/wikilinks/images), breadcrumbs, upload, share, palette.

test("tree lists the seeded folders and files", async ({ page }) => {
  await login(page);
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  await expect(page.locator('#tree .row[data-path="index.md"]')).toBeVisible();
  await expect(page.locator('#tree .row[data-path="guide.md"]')).toBeVisible();
});

test("markdown file: rendered content, crumb, meta, download + share buttons", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(page.locator("#crumb")).toContainText("index.md");
  await expect(page.locator("#meta")).toContainText("alice@x.io");
  // Download lives in the ⋯ menu now; the hidden anchor powers it.
  await expect(page.locator("#download")).toHaveCount(1);
  await expect(page.locator("#more-btn")).toBeVisible();
  await expect(page.locator("#share-btn")).toBeVisible();
});

test("wikilink navigates to the target file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click('#content a:has-text("guide")');
  await page.waitForURL(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
});

test("folder listing: counts, change feed, heat dot on a read file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('#tree .row[data-path="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await expect(page.locator(".dl-title")).toContainText("notes");
  await expect(page.locator(".dl-sub")).toContainText("1 folder");
  await expect(page.locator(".dl-sub")).toContainText("1 file");
  await expect(page.locator(".dl-history .dl-h3")).toHaveText("Recent changes");
  await expect(page.locator(".dl-history .hentry").first()).toBeVisible();
  // notes/readme.md has seeded agent reads → a heat dot on its row
  await expect(page.locator('.dl-row[title="notes/readme.md"] .heatdot')).toBeVisible();
});

test("image file renders an <img>", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/assets/logo.png`);
  await expect(page.locator("#content img")).toBeVisible();
});

test("breadcrumb ancestor opens that folder", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes/deep/topic.md`);
  await expect(page.locator("#content h1")).toHaveText("Topic");
  await page.click('#crumb .crumb-seg[title="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await expect(page.locator(".dl-title")).toContainText("notes");
});

test("deep file link resolves after a hard reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes/readme.md`);
  await expect(page.locator("#content h1")).toHaveText("Notes");
  await page.reload();
  await expect(page.locator("#content h1")).toHaveText("Notes");
  // The tree unfolds the way to the deep-linked file
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
});

test("back/forward walks file → folder → file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click('#tree .row[data-path="notes"]');
  await page.waitForURL(`/${pid}/notes`);
  await page.goBack();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await page.goForward();
  await expect(page.locator(".dl-title")).toContainText("notes");
});

test("header search button opens the palette", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.click("#search-btn");
  await expect(page.locator("#palette")).toBeVisible();
  await page.keyboard.press("Escape");
});

test("palette (⌘K) fuzzy-jumps to a file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  await page.fill("#palette-input", "topic");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}/notes/deep/topic.md`);
  await expect(page.locator("#content h1")).toHaveText("Topic");
});

test("share mints a public link that serves the file, revoke kills it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await page.click("#share-btn");
  const url = await page.locator(".modal-url").textContent();
  expect(url).toContain("/s/");
  const publicRes = await page.request.get(url!);
  expect(publicRes.status()).toBe(200);
  expect(await publicRes.text()).toContain("Second version");
  await page.click(".modal .ai-del"); // revoke
  await expect(page.locator("#toast")).toContainText("revoked");
  const gone = await page.request.get(url!);
  expect(gone.status()).toBe(404);
});

test("no browser upload: content arrives via sync; the tree picks it up", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  // The upload affordance is gone everywhere — content enters via local sync.
  await expect(page.locator("#upload-btn")).toHaveCount(0);
  await expect(page.locator('input[type="file"]')).toHaveCount(0);
  // A file lands through the device/store path (simulated via the API)…
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("notes/dropped.md")}`,
    { data: "# Dropped\n\nArrived through sync.\n" },
  );
  // …and the polling tree shows it; opening renders it.
  await page.goto(`/${pid}/notes/dropped.md`);
  await expect(page.locator("#content h1")).toHaveText("Dropped");
  await expect(page.locator('#tree .row[data-path="notes/dropped.md"]')).toBeVisible();
});

test("html file renders as a page in a sandboxed iframe", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const html = "<h1 id='t'>Hello from HTML</h1><script>document.title='js-ran'</scr" + "ipt>";
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("pages/hello.html")}`,
    { data: html },
  );
  await page.goto(`/${pid}/pages/hello.html`);
  const frame = page.locator("#content iframe.htmlview");
  await expect(frame).toBeVisible();
  await expect(frame).toHaveAttribute("sandbox", "allow-scripts");
  await expect(page.frameLocator("#content iframe.htmlview").locator("#t")).toHaveText(
    "Hello from HTML",
  );
  // Server-side wall: inline HTML carries the sandbox CSP (same as /s/*).
  const res = await page.request.get(`/api/p/${pid}/file?path=${encodeURIComponent("pages/hello.html")}`);
  expect(res.headers()["content-security-policy"]).toBe("sandbox allow-scripts");
});

test("missing path gets the not-found view; Check again finds a late upload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/later.md`);
  await expect(page.locator(".notfound h1")).toHaveText("Couldn't find that");
  await expect(page.locator(".notfound code")).toHaveText("later.md");
  await expect(page.locator(".notfound")).toContainText("still be uploading");
  // The file arrives (a teammate/agent finished syncing it)…
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("later.md")}`,
    { data: "# Finally here\n" },
  );
  await page.click(".notfound .pbtn"); // Check again
  await expect(page.locator("#content h1")).toHaveText("Finally here");
});
