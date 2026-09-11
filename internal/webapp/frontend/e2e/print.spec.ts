import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// BEA-219. Two printers, because there are two kinds of document on screen:
// what the app renders into its own DOM prints from the page, and synced HTML
// — which lives in a cross-origin frame that prints clipped to its box —
// opens a top-level print view that presses Print itself.

test("print media drops the chrome and keeps the document", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toBeVisible();

  await page.emulateMedia({ media: "print" });
  // "The printing part should be the content only" — the app's frame is not
  // part of the document, and on paper it is a wasted first page.
  await expect(page.locator("#sidebar")).toBeHidden();
  await expect(page.locator("#topbar")).toBeHidden();
  await expect(page.locator("#content h1")).toBeVisible();

  // #content is the app's only scroller; if it keeps its overflow on paper
  // the print stops at one viewport.
  await expect(page.locator("#content")).toHaveCSS("overflow-y", "visible");
  // The app is dark-only, so without the token flip this prints near-white
  // text onto white paper. Heading and prose are separate tokens and were
  // separate hardcoded literals, so both are checked.
  for (const sel of ["#content h1", "#content .markdown p"]) {
    const ink = await page.locator(sel).first().evaluate((el) => getComputedStyle(el).color);
    const [r, g, b] = ink.match(/\d+/g)!.map(Number);
    expect(r + g + b, `${sel} ink ${ink} is too light for paper`).toBeLessThan(300);
  }
});

test("markdown prints in place, from this page", async ({ page }) => {
  await page.addInitScript(() => {
    (window as unknown as { __printed: number }).__printed = 0;
    window.print = () => ((window as unknown as { __printed: number }).__printed += 1);
  });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#more-btn");
  await page.click("#print-item");
  await expect
    .poll(() => page.evaluate(() => (window as unknown as { __printed: number }).__printed))
    .toBe(1);
  // Printing closes the menu — otherwise it is still open, over the page,
  // when the print dialog returns.
  await expect(page.locator("#more-menu")).toHaveCount(0);
  // Nothing to open: the document is already on this page.
  await expect(page.locator("#print")).toHaveCount(0);
});

test("no print button while editing — the editor only renders what you can see", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#edit-btn");
  await expect(page.locator(".cm-host")).toBeVisible();
  await page.click("#more-btn");
  await expect(page.locator("#more-menu")).toBeVisible();
  // CodeMirror puts only the visible lines in the DOM, so printing here would
  // silently drop most of a long file. Reading is the mode you print from.
  await expect(page.locator("#print-item")).toHaveCount(0);
  await page.keyboard.press("Escape");
  await page.click("#edit-btn"); // Done
  await page.click("#more-btn");
  await expect(page.locator("#print-item")).toBeVisible();
});

test("html opens a top-level print view that prints itself", async ({ page }) => {
  await page.addInitScript(() => {
    (window as unknown as { __printed: number }).__printed = 0;
    window.print = () => ((window as unknown as { __printed: number }).__printed += 1);
  });
  await login(page);
  const pid = await wikiId(page);
  const path = "pages/printable.html";
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`,
    { data: "<h1 id='t'>Printable</h1>" },
  );
  await page.goto(`/${pid}/${path}`);
  await expect(page.locator("#content iframe.htmlview")).toBeVisible();

  // rel=noopener matters: the print view is opaque-origin but still gets a
  // WindowProxy, and `opener.location` is a tab this page owns.
  const anchor = page.locator("#print");
  await expect(anchor).toHaveAttribute("target", "_blank");
  await expect(anchor).toHaveAttribute("rel", "noopener");
  const href = await anchor.getAttribute("href");
  expect(href).toContain(encodeURIComponent(path));
  expect(href).toContain("print=1");

  // Printing THIS page would print the frame clipped to its box, so the menu
  // item must not do that.
  await page.click("#more-btn");
  await page.click("#print-item");
  await expect(page.locator("#more-menu")).toHaveCount(0);
  expect(await page.evaluate(() => (window as unknown as { __printed: number }).__printed)).toBe(0);

  // The URL the button builds is the one the server honours: the document's
  // own bytes, then the script that opens the dialog, inside a sandbox that
  // gained allow-modals and nothing else.
  const res = await page.request.get(href!);
  expect(res.headers()["content-security-policy"]).toBe("sandbox allow-scripts allow-modals");
  const body = await res.text();
  expect(body).toContain("<h1 id='t'>Printable</h1>");
  expect(body).toMatch(/print\(\)\}\)<\/script>$/);
});

test("pdf keeps its viewer's own print button instead of a second one", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("pages/sheet.pdf")}`,
    { data: "%PDF-1.4\n%%EOF\n" },
  );
  await page.goto(`/${pid}/pages/sheet.pdf`);
  await page.click("#more-btn");
  await expect(page.locator("#more-menu")).toBeVisible();
  await expect(page.locator("#print-item")).toHaveCount(0);
});
