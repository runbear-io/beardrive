import { test, expect } from "@playwright/test";
import { login, wikiId, expectToast, READER } from "./helpers";

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
  // The full whoChanged() string, not just the address: the seed's Author
  // equals its User, so "alice@x.io" alone passed even when the viewer was
  // rendering the git/OS identity instead of the signed-in account.
  await expect(page.locator("#meta")).toContainText("Alice <alice@x.io>");
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

// BEA-28: copying a folder URL hands you a trailing slash, and that URL used
// to 404 while the sidebar showed the folder populated right next to it.
test("folder URL with a trailing slash renders the listing and drops the slash", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.goto(`/${pid}/notes/`);
  await expect(page.locator(".dl-title")).toContainText("notes");
  await expect(page.locator(".dl-history .dl-h3")).toHaveText("Recent changes");
  await expect(page).toHaveURL(`/${pid}/notes`);
  // Replaced, not pushed: Back leaves the folder instead of bouncing off the
  // slashed URL and landing right back here.
  await page.goBack();
  await expect(page).toHaveURL(`/${pid}`);
});

// BEA-17: the kind glyph read as a disclosure toggle. It is now a text
// badge, the row's only real expander is the note, and clicking the badge
// navigates like the rest of the row — no dead zone, no second behavior.
test("history row: kind is a badge, not a disclosure control", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const row = page.locator(".history .hentry").first();
  await expect(row).toBeVisible();
  // kind is conveyed as text, not by an icon shape
  await expect(row.locator(".hkind")).toHaveText("edited");
  await expect(row.locator(".hkind .ico")).toHaveCount(0);
  // a row announces kind, path and author without the icon
  await expect(page.getByRole("button", { name: /edited\s+guide\.md.*alice@x\.io/s })).toHaveCount(1);
  // only genuine expanders claim to expand: the note and the diff disclosure
  await expect(page.locator(".history .hnote[aria-expanded]")).toHaveCount(1);
  await expect(page.locator(".history [aria-expanded]:not(.hnote):not(.hdiff-btn)")).toHaveCount(0);
  // ...and it still expands in place, without navigating
  await row.locator(".hnote").click({ position: { x: 6, y: 6 } }); // off the note's link
  await expect(row.locator(".hnote")).toHaveClass(/open/);
  await expect(page).toHaveURL(`/${pid}/history/guide.md`);
  // clicking the badge does exactly what clicking the row does: it opens
  // the version the row describes (BEA-7), not a dead zone
  await row.locator(".hkind").click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
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
  await page.fill("#palette input", "topic");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}/notes/deep/topic.md`);
  await expect(page.locator("#content h1")).toHaveText("Topic");
});

// BEA-52: on a path that doesn't resolve the tree entries are gone and the
// switcher lists only other projects, so the palette used to offer no way
// back. cmdk owns the list's id (it overwrites ours), hence [cmdk-list].
test("palette on a dead route still offers the way back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/does-not-exist.md`);
  await expect(page.locator(".notfound")).toBeVisible();
  await page.keyboard.press("ControlOrMeta+k");
  await expect(page.locator("#palette")).toBeVisible();
  for (const label of ["Go to project root", "Dashboard", "Installation", "Settings"]) {
    await expect(page.locator("#palette [cmdk-list]")).toContainText(label);
  }
  // exactly one whole-project history entry, no duplicate
  expect(
    await page.locator("#palette [cmdk-item]", { hasText: "History: whole project" }).count(),
  ).toBe(1);
  await page.fill("#palette input", "Dashboard");
  await page.keyboard.press("Enter");
  await page.waitForURL(`/${pid}/dashboard`);
  await page.reload(); // the entries are real URLs, not panel state
  await expect(page.locator(".in-treemap")).toBeVisible();
});

// BEA-54: cmdk overwrites the `id` we pass its primitives, so every palette
// rule anchored on one was dead — the input lost its only author `color` and
// fell back to the UA's black. Anchors here are ours (#palette) or cmdk's own
// attributes, which is exactly what the fix relies on.
test("palette renders in the dark palette, not UA black (BEA-54)", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.keyboard.press("ControlOrMeta+k");
  const input = page.locator("#palette input");
  await input.fill("topic");
  await expect(input).toHaveCSS("color", "rgb(238, 240, 243)"); // --text, was rgb(0, 0, 0)
  const sel = page.locator("#palette [cmdk-item][data-selected='true']");
  await expect(sel.locator(".plabel")).toHaveCSS("color", "rgb(255, 207, 133)"); // --accent-bright
  await expect(sel.locator(".pkind")).toHaveCSS("text-transform", "uppercase");
  await expect(sel).toHaveCSS("background-color", "rgba(245, 166, 35, 0.13)"); // --glow
  await page.keyboard.press("Escape");
});

test("share mints a public link that serves the file, revoke kills it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await page.click("#share-btn");
  // BEA-32: the modal hands over the URL and nothing destructive — Revoke is
  // the banner's, and two of them for one link is what this asserts away.
  await expect(page.locator(".modal .ai-del")).toHaveCount(0);
  const url = await page.locator(".modal-url").textContent();
  expect(url).toContain("/s/");
  const publicRes = await page.request.get(url!);
  expect(publicRes.status()).toBe(200);
  expect(await publicRes.text()).toContain("Second version");

  // Revoke where the control actually lives, from the file page.
  await page.click(".modal button:has-text('Done')");
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await banner.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  const gone = await page.request.get(url!);
  expect(gone.status()).toBe(404);
});

// BEA-29: the CLI has had --expires all along; the dialog now offers it on
// the link you just minted, without changing that link's URL.
test("share dialog sets an expiry on the link it just minted", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#share-btn");
  const url = (await page.locator(".modal-url").textContent())!;
  await expect(page.locator(".modal-expiry-note")).toHaveText("no expiry");

  await page.selectOption("#share-expiry", "168h");
  await expect(page.locator(".modal-expiry-note")).toContainText("expires");
  // Same link: the URL already on the clipboard keeps working.
  expect(await page.locator(".modal-url").textContent()).toBe(url);
  expect((await page.request.get(url)).status()).toBe(200);
  await page.click(".modal button:has-text('Done')");

  // …and Settings stops calling it permanent.
  await page.goto(`/${pid}/settings`);
  const row = page.locator(".admin-item", { hasText: "index.md" });
  await expect(row.locator(".ai-tag")).toContainText("expires");
  await expect(row.locator(".ai-tag")).not.toContainText("no expiry");

  await page.request.delete(`/api/shares/${url.split("/s/")[1]}`);
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

test("tree chevron folds and unfolds a folder", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  // Unfold via row click (opens listing + expands), then fold via chevron.
  await page.click('#tree .row[data-path="notes"]');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
  await page.click('#tree .row[data-path="notes"] .chev');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).not.toBeVisible();
  await page.click('#tree .row[data-path="notes"] .chev');
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toBeVisible();
});

// BEA-16: the undo for "I made this public" lives on the file, not three
// clicks away in the org panel.
test("public link: the file page says it is shared, and revokes without a reload", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator(".share-banner")).toHaveCount(0);

  await page.click("#share-btn");
  const url = (await page.locator(".modal-url").textContent())!;
  await page.click(".modal button:has-text('Done')");

  // The indicator is on the file itself, and it is still there after a reload
  // (the dialog used to be the only place the link — and its Revoke — existed).
  await page.reload();
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Publicly shared");
  await expect(banner).toContainText("1 active link");
  await expect(banner).toContainText("no expiry");
  expect((await page.request.get(url)).status()).toBe(200);

  // Revoking from the file page kills the link and updates in place.
  await banner.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  await expect(banner).toHaveCount(0);
  expect((await page.request.get(url)).status()).toBe(404);
});

test("project settings lists this project's public links and revokes them", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.post(`/api/p/${pid}/shares`, { data: { path: "notes/readme.md" } });

  await page.goto(`/${pid}/settings`);
  const row = page.locator(".admin-item", { hasText: "notes/readme.md" });
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("by e2e@example.com");
  await expect(row.locator(".ai-tag")).toContainText("no expiry");

  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Share revoked");
  await expect(page.locator(".admin-item", { hasText: "notes/readme.md" })).toHaveCount(0);
  await expect(page.locator(".admin-empty", { hasText: "No public links." })).toBeVisible();
});

test("a read-only member sees the public-link banner but cannot revoke", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "index.md" } })
  ).json();

  // A real second identity in this page: drop the admin session first, or
  // the helper's first-time form login never sees /auth/login.
  await page.context().clearCookies();
  await login(page, READER);
  await page.goto(`/${pid}/index.md`);
  const banner = page.locator(".share-banner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("Publicly shared");
  await expect(banner.locator("button:has-text('Copy link')")).toBeVisible();
  await expect(banner.locator(".ai-del")).toHaveCount(0);
  await expect(page.locator("#share-btn")).toHaveCount(0);

  await page.context().clearCookies();
  await login(page); // clean up as someone who may
  await page.request.delete(`/api/shares/${made.token}`);
});

test("public links: banner and settings table fit a 390px viewport", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "guide.md" } })
  ).json();
  await page.setViewportSize({ width: 390, height: 780 });

  const sideways = () =>
    page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1);

  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator(".share-banner")).toBeVisible();
  expect(await sideways()).toBe(false);

  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".admin-item", { hasText: "guide.md" })).toBeVisible();
  expect(await sideways()).toBe(false);
  // The table takes its own horizontal scroll rather than widening the page.
  const box = page.locator(".project-settings .admin-card-table").last();
  expect(await box.evaluate((el) => getComputedStyle(el).overflowX)).toBe("auto");

  await page.request.delete(`/api/shares/${made.token}`);
});

// BEA-7: a history row is an address for the version it describes.

test("history row opens THAT version, banner says so, View current returns", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  // Oldest row = the first version; click it rather than the latest.
  const added = page.locator(".hentry.add");
  await expect(added).toBeVisible();
  await added.click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content")).toContainText("First version");
  await expect(page.locator("#content")).not.toContainText("Second version");
  // Rendered markdown, not raw source.
  await expect(page.locator("#content h1")).toHaveText("Guide");
  // The banner is what stops the page misleading.
  const banner = page.locator(".vbanner");
  await expect(banner).toBeVisible();
  await expect(banner).toContainText("This is not the current file");
  await expect(banner).toContainText("alice@x.io");
  await expect(banner.locator("a[download]")).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}.*download=1/);
  // Downloading while pinned gives that version, not the current bytes.
  await expect(page.locator("#download")).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}/);
  await banner.getByText("View current").click();
  await page.waitForURL(`/${pid}/guide.md`);
  await expect(page.locator("#content")).toContainText("Second version");
  await expect(page.locator(".vbanner")).toHaveCount(0);
});

test("a version URL survives a hard reload, and Back returns to history", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  await page.locator(".hentry.add").click();
  const url = page.url();
  await page.reload();
  await expect(page.locator("#content")).toContainText("First version");
  await expect(page.locator(".vbanner")).toBeVisible();
  await page.goto(url); // fresh navigation to the deep link
  await expect(page.locator("#content")).toContainText("First version");
  await page.goBack();
  await expect(page.locator(".history .hentry").first()).toBeVisible();
});

test("an unknown version says so instead of showing current content", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md?v=${"a".repeat(64)}`);
  await expect(page.locator("#content")).toContainText("That version isn't available");
  await expect(page.locator("#content")).not.toContainText("Second version");
  await expect(page.locator(".vbanner")).toBeVisible(); // still offers a way back
});

test("delete rows have no version to open, so they stay unclickable", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/scratch.md`);
  const del = page.locator(".hentry.delete");
  await expect(del).toBeVisible();
  await expect(del).not.toHaveClass(/clickable/);
  await del.click();
  await expect(page).toHaveURL(`/${pid}/history/scratch.md`);
});

// BEA-6: one agent run is one card, and any version can be put back.

test("history groups one agent run into a single card", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const run = page.locator(".hrun");
  await expect(run).toHaveCount(1);
  await expect(run.locator(".hrun-note")).toHaveText("claude-code session 8f21e4");
  await expect(run.locator(".hrun-meta")).toContainText("2 files");
  await expect(run.locator(".hrun-meta")).toContainText("seed-agent");
  // Both of the run's changes live inside the card...
  await expect(run.locator(".hentry")).toHaveCount(2);
  await expect(run.locator('.hentry:has-text("runbook.md")')).toBeVisible();
  // ...and note-less changes are still bare rows, exactly as before.
  await expect(page.locator(".history > .hentry").first()).toBeVisible();
  await expect(page.locator(".history > .hentry .hrun-note")).toHaveCount(0);
  // The note is not repeated on every row inside the card.
  await expect(run.locator(".hnote")).toHaveCount(0);
  // The card collapses without navigating.
  await run.locator(".hrun-toggle").click();
  await expect(run.locator(".hentry")).toHaveCount(0);
  await expect(page).toHaveURL(`/${pid}/history`);
});

// BEA-35: the one thing history couldn't reverse was a file a run CREATED.
// Undo removes it (via a delete op the hub journals), and the DELETED row it
// leaves behind restores it — so the round trip is what the test asserts, and
// the seeded run is left exactly as it was found.
test("a file the run created can be undone, and comes back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const created = page.locator('.hrun .hentry.add:has-text("runbook.md")');
  await expect(created).toBeVisible();
  // An add has no old bytes to put back — its undo is a removal.
  await expect(created.locator(".hrestore-btn")).toHaveCount(0);
  await expect(created.locator(".hremove-btn")).toBeVisible();
  // The file it edited still offers a restore.
  await expect(
    page.locator('.hrun .hentry.edit:has-text("notes/readme.md") .hrestore-btn'),
  ).toBeVisible();

  // It reaches every device, so it always asks first — and Cancel means no.
  await created.locator(".hremove-btn").click();
  const modal = page.locator(".modal");
  await expect(modal).toContainText("Remove runbook.md?");
  await expect(modal).toContainText("every synced device");
  await modal.getByRole("button", { name: "Cancel" }).click();
  await expect(page.locator(`.hentry.delete:has-text("runbook.md")`)).toHaveCount(0);

  await created.locator(".hremove-btn").click();
  await page.locator(".modal .danger-btn").click();
  await expectToast(page, /Removed runbook\.md/);
  const gone = page.locator('.history > .hentry.delete:has-text("runbook.md")').first();
  await expect(gone).toBeVisible();

  // ...and the delete row puts it back, bytes and all.
  await gone.locator(".hrestore-btn").click();
  await expectToast(page, /Restored runbook\.md/);
  await page.goto(`/${pid}/runbook.md`);
  await expect(page.locator("#content")).toContainText("Created during the agent run");
});

test("restoring an old version brings its content back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Its own file, with its own two versions — restoring is a real write, so
  // it must not disturb what the rest of the suite reads.
  const path = "restore-me.md";
  const url = `/api/p/${pid}/upload/content?path=${path}`;
  await page.request.put(url, { data: "# Restore me\n\nThe good version.\n" });
  await page.request.put(url, { data: "# Restore me\n\nClobbered by an agent.\n" });

  await page.goto(`/${pid}/history/${path}`);
  const older = page.locator(".hentry.add"); // the first version
  await expect(older).toBeVisible();
  await older.locator(".hrestore-btn").click();
  await expectToast(page, /Restored restore-me\.md/);
  // The restore is itself a change, and the file serves the old bytes again.
  await expect(page.locator(".history .hentry")).toHaveCount(3);
  await expect(page.locator(".history .hentry").first()).toContainText("restore restore-me.md@");
  await page.goto(`/${pid}/${path}`);
  await expect(page.locator("#content")).toContainText("The good version");
});

test("a read-only member gets no restore or remove buttons", async ({ page }) => {
  await login(page, READER);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  await expect(page.locator(".history .hentry").first()).toBeVisible();
  await expect(page.locator(".hrestore-btn")).toHaveCount(0);
  await expect(page.locator(".hremove-btn")).toHaveCount(0);
});

// BEA-26: the row was already an address for its version — but a bare
// role="button" div announces that to nobody, so a persona whose whole fear
// is "an agent quietly rewrote my doc" concludes recovery is impossible.
// The version now carries visible handles.

test("history rows carry visible Open/Download controls for that version", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const added = page.locator(".hentry.add"); // the first version of guide.md
  await expect(added).toBeVisible();

  const open = added.getByRole("button", { name: /Open guide\.md as of/ });
  const dl = added.getByRole("link", { name: /Download guide\.md as of/ });
  await expect(open).toBeVisible();
  await expect(dl).toBeVisible();
  // Neither claims to expand anything (BEA-17's invariant).
  await expect(page.locator(".history [aria-expanded]:not(.hnote):not(.hdiff-btn)")).toHaveCount(0);

  // The download is that version's bytes, not the current file's.
  const href = await dl.getAttribute("href");
  expect(href).toMatch(/blob\?sha=[0-9a-f]{64}&name=guide\.md&download=1$/);
  const body = await (await page.request.get(href!)).text();
  expect(body).toContain("First version");
  expect(body).not.toContain("Second version");

  // Open lands on the file pinned to that version, and fires once.
  await open.click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator(".vbanner")).toBeVisible();
  await expect(page.locator("#content")).toContainText("First version");
});

test("version controls are keyboard-reachable and don't double-fire the row", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/guide.md`);
  const open = page.locator(".hentry.add").getByRole("button", { name: /Open guide\.md as of/ });
  await open.focus();
  await expect(open).toBeFocused();
  await open.press("Enter");
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content")).toContainText("First version");
  // One entry in history, not two: the row's own handler never also ran.
  await page.goBack();
  await expect(page).toHaveURL(`/${pid}/history/guide.md`);
});

test("a delete row offers no version to open or download", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history/scratch.md`);
  const del = page.locator(".hentry.delete");
  await expect(del).toBeVisible();
  await expect(del.locator(".hver-btn")).toHaveCount(0);
});

test("the folder change feed carries the version controls too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  // This feed passes no `diff` prop at all — the controls must not be
  // gated behind it.
  const row = page.locator(".dl-history .hentry").first();
  await expect(row).toBeVisible();
  await expect(row.locator(".hdiff-btn")).toHaveCount(0);
  const dl = row.getByRole("link", { name: /^Download .* as of/ });
  await expect(dl).toBeVisible();
  await expect(dl).toHaveAttribute("href", /blob\?sha=[0-9a-f]{64}&name=[^&]+&download=1$/);
  await row.getByRole("button", { name: /^Open .* as of/ }).click();
  await page.waitForURL(new RegExp(`/${pid}/notes/.*\\?v=[0-9a-f]{64}$`));
  await expect(page.locator(".vbanner")).toBeVisible();
});

// BEA-44: the viewer used to decide on the extension, so every extensionless
// file an agent wrote — Dockerfile, LICENSE, .bdriveignore — hit a dead
// "No preview" card. It decides on the bytes now, and renders PDFs.

test("an extensionless UTF-8 file previews as text", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/Dockerfile`, {
    data: "FROM alpine\nRUN apk add --no-cache curl\n",
  });
  await page.goto(`/${pid}/sniff/Dockerfile`);
  await expect(page.locator("#content pre.plain")).toContainText("RUN apk add --no-cache curl");
});

test("an unlisted extension previews as text too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/main.tf`, {
    data: 'resource "aws_s3_bucket" "b" {}\n',
  });
  await page.goto(`/${pid}/sniff/main.tf`);
  await expect(page.locator("#content pre.plain")).toContainText("aws_s3_bucket");
});

test("binary bytes get the no-preview card, never dumped into the page", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/model.bin`, {
    data: Buffer.from([0x89, 0x50, 0x00, 0x01, 0x02, 0xff, 0xfe]),
  });
  await page.goto(`/${pid}/sniff/model.bin`);
  const card = page.locator("#content .filecard");
  await expect(card).toContainText("No preview for this file type.");
  await expect(page.locator("#content pre.plain")).toHaveCount(0);
  await expect(card.getByRole("link", { name: "Download" })).toHaveAttribute(
    "href",
    /download\?path=sniff%2Fmodel\.bin$/,
  );
});

test("a text file past the 1 MB cap says so instead of loading it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/huge.dat`, {
    data: "x".repeat((1 << 20) + 1024),
  });
  await page.goto(`/${pid}/sniff/huge.dat`);
  await expect(page.locator("#content .filecard")).toContainText(/Too large to preview \(1\.0 MB\)/);
});

test("a pdf renders in the browser's viewer, in the wide column", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Smallest thing Chromium's viewer accepts; the assertion is the frame,
  // not the glyphs.
  const pdf =
    "%PDF-1.4\n1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj\n" +
    "2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj\n" +
    "3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj\n" +
    "trailer<</Root 1 0 R>>\n%%EOF\n";
  await page.request.put(`/api/p/${pid}/upload/content?path=sniff/report.pdf`, { data: pdf });
  await page.goto(`/${pid}/sniff/report.pdf`);
  const frame = page.locator("#content iframe.pdfview");
  await expect(frame).toBeVisible();
  await expect(frame).toHaveAttribute("src", /file\?path=sniff%2Freport\.pdf$/);
  // No sandbox attribute: the PDF viewer is not this page's JS realm, and
  // sandboxing without allow-same-origin breaks Firefox's pdf.js.
  await expect(frame).not.toHaveAttribute("sandbox", /./);
  await expect(page.locator(".page.wide")).toBeVisible();
});

test("an old version of an extensionless file previews the same way", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const url = `/api/p/${pid}/upload/content?path=sniff/LICENSE`;
  await page.request.put(url, { data: "MIT License — the first draft.\n" });
  await page.request.put(url, { data: "Apache 2.0 — the second draft.\n" });
  await page.goto(`/${pid}/history/sniff/LICENSE`);
  const older = page.locator(".hentry.add");
  await expect(older).toBeVisible();
  await older.getByRole("button", { name: /^Open .* as of/ }).click();
  await page.waitForURL(new RegExp(`/${pid}/sniff/LICENSE\\?v=[0-9a-f]{64}$`));
  await expect(page.locator("#content pre.plain")).toContainText("the first draft");
  // A bad sha still explains itself rather than previewing nothing.
  await page.goto(`/${pid}/sniff/LICENSE?v=${"0".repeat(64)}`);
  await expect(page.locator("#content .empty")).toContainText("That version isn't available.");
});
