import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* Fullscreen (?full=1): the same file page with the app chrome hidden, so a
   wide table, a diagram, a rendered HTML file or a PDF gets the window.

   The two things worth pinning here are the ones the code made non-obvious:
   the chrome is HIDDEN rather than unmounted (so #content keeps its
   scrollTop and the reader does not move), and the Exit control is painted
   over the HTML/PDF iframes — both swallow every key event, so Esc alone
   would strand a reader inside one. */

const LONG = "long/scrolly.md";
const HTML = "pages/fs-hello.html";
const PDF = "pages/fs-doc.pdf";

// A minimal but real PDF, so the browser mounts its own viewer in .pdfview.
const MINI_PDF = [
  "%PDF-1.4",
  "1 0 obj<</Type/Catalog/Pages 2 0 R>>endobj",
  "2 0 obj<</Type/Pages/Kids[3 0 R]/Count 1>>endobj",
  "3 0 obj<</Type/Page/Parent 2 0 R/MediaBox[0 0 200 200]>>endobj",
  "trailer<</Root 1 0 R>>",
  "%%EOF",
].join("\n");

/* The seeded project, seeded ONCE per run. Two constraints pull against each
   other here and both are load-bearing: every upload journals an op, and
   enough of them push the seeded agent-run card out of the window
   session-run.spec reads — while a project of our own would sort before
   "wiki" and steal projects[0] from every landing spec. So: the shared
   project, three files, once. Playwright keeps one worker process per file,
   so the flag holds for the whole spec. */
let seeded = false;
async function seed(page: Page, pid: string) {
  if (seeded) return;
  seeded = true;
  const put = (path: string, data: string) =>
    page.request.put(`/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`, { data });
  await put(LONG, "# Scrolly\n\n" + Array.from({ length: 220 }, (_, i) => `Paragraph ${i}.`).join("\n\n"));
  await put(HTML, "<h1 id='t'>Hello fullscreen</h1>");
  await put(PDF, MINI_PDF);
}

const chromeHidden = async (page: Page) => {
  await expect(page.locator("#sidebar")).toBeHidden();
  await expect(page.locator("#topbar")).toBeHidden();
  await expect(page.locator("#exit-full")).toBeVisible();
};

test("the fullscreen control hides the chrome and puts it in the URL", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}`);
  await page.waitForSelector("#content > .page");
  await expect(page.locator("#sidebar")).toBeVisible();

  await page.click("#full-btn");
  await expect(page).toHaveURL(new RegExp(`\\?full=1$`));
  await chromeHidden(page);

  // The URL is the state: a teammate pasting it lands where you are.
  await page.click("#exit-full");
  await expect(page).toHaveURL(`http://localhost:8993/${pid}/${LONG}`);
  await expect(page.locator("#topbar")).toBeVisible();
});

test("a pasted ?full=1 link renders fullscreen with no flash of the normal layout", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}?full=1`);
  // Deliberately the FIRST thing asserted after the page element exists: the
  // flag is derived from the URL before the tree loads, so there is never a
  // frame with the chrome up.
  await page.waitForSelector("#content > .page");
  expect(await page.locator("#topbar").isVisible()).toBe(false);
  expect(await page.locator("#sidebar").isVisible()).toBe(false);
  await expect(page.locator("#exit-full")).toBeVisible();

  // Nothing was pushed, so there is nothing to pop: Exit replaces in place.
  await page.click("#exit-full");
  await expect(page).toHaveURL(`http://localhost:8993/${pid}/${LONG}`);
});

test("a past version reads fullscreen, banner and all", async ({ page }) => {
  await login(page);
  // The seeded project, read-only here: guide.md is the one path with two
  // versions, and this test uploads nothing.
  const pid = await wikiId(page);
  // guide.md is seeded with two versions; take the older one's sha.
  const hist = await (await page.request.get(`/api/p/${pid}/history?path=guide.md`)).json();
  const older = hist.entries[hist.entries.length - 1].sha as string;

  await page.goto(`/${pid}/guide.md?v=${older}&full=1`);
  await page.waitForSelector("#content > .page");
  await chromeHidden(page);
  await expect(page.locator(".vbanner")).toBeVisible();

  // Exiting keeps every param except full.
  await page.click("#exit-full");
  await expect(page).toHaveURL(`http://localhost:8993/${pid}/guide.md?v=${older}`);
  await expect(page.locator(".vbanner")).toBeVisible();
});

test("Esc and browser Back both leave fullscreen", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}`);
  await page.click("#full-btn");
  await chromeHidden(page);
  await page.keyboard.press("Escape");
  await expect(page.locator("#topbar")).toBeVisible();
  await expect(page).toHaveURL(`http://localhost:8993/${pid}/${LONG}`);
  // Exit collapsed the entry it pushed rather than stacking a third, so one
  // more Back does not walk straight back into fullscreen.
  await page.goBack();
  await expect(page).not.toHaveURL(/full=1/);

  await page.goto(`/${pid}/${LONG}`);
  await page.click("#full-btn");
  await chromeHidden(page);
  await page.goBack();
  // Back lands on the SAME file, not on the previously visited one.
  await expect(page).toHaveURL(`http://localhost:8993/${pid}/${LONG}`);
  await expect(page.locator("#topbar")).toBeVisible();
});

test("the reader keeps their place entering and leaving fullscreen", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}`);
  await page.waitForSelector("#content .markdown");
  // #content is the one scroll container in the app — the document never
  // scrolls (see helpers.ts).
  await page.locator("#content").evaluate((c) => c.scrollTo({ top: 1400, behavior: "instant" }));
  await page.waitForTimeout(150);
  const before = await page.locator("#content").evaluate((c) => c.scrollTop);
  expect(before).toBeGreaterThan(1000);

  await page.click("#full-btn");
  await chromeHidden(page);
  await page.waitForTimeout(250);
  const during = await page.locator("#content").evaluate((c) => c.scrollTop);
  expect(Math.abs(during - before)).toBeLessThan(20);

  await page.click("#exit-full");
  await expect(page.locator("#topbar")).toBeVisible();
  await page.waitForTimeout(250);
  const after = await page.locator("#content").evaluate((c) => c.scrollTop);
  expect(Math.abs(after - before)).toBeLessThan(20);
});

test("toggling fullscreen refetches nothing", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}`);
  await page.waitForSelector("#content .markdown");
  // `full` never reaches a query key, so a toggle costs no request — which is
  // also what keeps read counts honest and a large PDF from reloading.
  let renders = 0;
  page.on("request", (r) => {
    if (r.url().includes("/render?path=") || r.url().includes("/file?path=")) renders++;
  });
  await page.click("#full-btn");
  await chromeHidden(page);
  await page.click("#exit-full");
  await expect(page.locator("#topbar")).toBeVisible();
  await page.waitForTimeout(400);
  expect(renders).toBe(0);
});

test("the Exit control is reachable over the HTML and PDF iframes", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  for (const [path, frame] of [[HTML, "iframe.htmlview"], [PDF, "iframe.pdfview"]] as const) {
    await page.goto(`/${pid}/${path}?full=1`);
    await expect(page.locator(`#content ${frame}`)).toBeVisible();
    // Painted OVER the frame, not beside it: hit-testing the button's own
    // centre has to land on the button.
    const hit = await page.locator("#exit-full").evaluate((el) => {
      const r = el.getBoundingClientRect();
      const top = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
      return el.contains(top);
    });
    expect(hit, `${path}: Exit is on top`).toBe(true);
    await page.click("#exit-full");
    await expect(page.locator("#topbar")).toBeVisible();
  }
});

test("hidden chrome is out of the tab order, and focus follows the exit", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  await page.goto(`/${pid}/${LONG}`);
  await page.click("#full-btn");
  await chromeHidden(page);
  // Entering moves focus to Exit…
  await expect(page.locator("#exit-full")).toBeFocused();
  // …and nothing in the hidden chrome can be tabbed to (display:none takes it
  // out of the tab order and the accessibility tree together).
  for (let i = 0; i < 12; i++) {
    await page.keyboard.press("Tab");
    const id = await page.evaluate(() => document.activeElement?.id || "");
    expect(["menu-btn", "search-btn", "full-btn", "share-btn", "more-btn"]).not.toContain(id);
    const inChrome = await page.evaluate(
      () => !!document.activeElement?.closest("#sidebar, #topbar"),
    );
    expect(inChrome).toBe(false);
  }
  // Leaving returns focus to the control that opened it.
  await page.locator("#exit-full").focus();
  await page.click("#exit-full");
  await expect(page.locator("#full-btn")).toBeFocused();
});

test("markdown keeps its measure; the HTML frame takes the window", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);

  const columnOf = (p: Page) =>
    p.evaluate(() => {
      const el = document.querySelector("#content > .page") as HTMLElement;
      return Math.round(el.getBoundingClientRect().width);
    });
  const frameHeight = (p: Page) =>
    p.evaluate(() => {
      const el = document.querySelector("#content iframe.htmlview") as HTMLElement;
      return Math.round(el.getBoundingClientRect().height);
    });

  // Prose: a 2000px line is unreadable, so markdown keeps --page-read and
  // gains only the removed chrome.
  await page.goto(`/${pid}/${LONG}`);
  await page.waitForSelector("#content .markdown");
  expect(await columnOf(page)).toBe(768);
  await page.click("#full-btn");
  await chromeHidden(page);
  expect(await columnOf(page)).toBe(768);

  // Everything else fills the viewport.
  await page.goto(`/${pid}/${HTML}`);
  await expect(page.locator("#content iframe.htmlview")).toBeVisible();
  const normal = await frameHeight(page);
  await page.goto(`/${pid}/${HTML}?full=1`);
  await expect(page.locator("#content iframe.htmlview")).toBeVisible();
  expect(await frameHeight(page)).toBeGreaterThan(normal);
  expect(await columnOf(page)).toBeGreaterThan(1200);
});

test("fullscreen works below the sidebar breakpoint", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await seed(page, pid);
  await page.setViewportSize({ width: 700, height: 800 });

  await page.goto(`/${pid}/${LONG}`);
  await page.waitForSelector("#content > .page");
  await page.click("#full-btn");
  // The sidebar is already off-canvas here; the topbar is what goes.
  await expect(page.locator("#topbar")).toBeHidden();
  await expect(page.locator("#exit-full")).toBeVisible();
  await page.click("#exit-full");
  await expect(page.locator("#topbar")).toBeVisible();
});

test("folders and view routes ignore ?full=1", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);

  // A folder listing has no content the chrome is in the way of, and a view
  // route is not a file page at all.
  for (const path of ["notes", "history", "dashboard"]) {
    await page.goto(`/${pid}/${path}?full=1`);
    await page.waitForSelector("#content > .page");
    await expect(page.locator("#topbar"), path).toBeVisible();
    await expect(page.locator("#exit-full"), path).toHaveCount(0);
  }
});
