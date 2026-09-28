import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* The reading column's drag handles (components/WidthHandle.tsx). The width
   is tab memory only: it follows the reader across files and in and out of
   fullscreen, and a reload is the reset. Seeded files only — an upload here
   would journal ops other specs count. */

test.use({ viewport: { width: 1600, height: 900 } });

const colWidth = async (page: Page) => (await page.locator("#content > .page").boundingBox())!.width;

async function dragRight(page: Page, dx: number) {
  const box = (await page.locator(".rw-right").boundingBox())!;
  const x = box.x + box.width / 2;
  const y = box.y + 40;
  await page.mouse.move(x, y);
  await page.mouse.down();
  await page.mouse.move(x + dx / 2, y, { steps: 3 });
  await page.mouse.move(x + dx, y, { steps: 3 });
  await page.mouse.up();
}

test("dragging the edge widens the column and it follows the reader until a reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.waitForSelector("#content > .page.read .rw-right");
  expect(await colWidth(page)).toBe(768);

  // Invisible at rest, drawn on hover.
  const lineOpacity = () =>
    page.locator(".rw-right").evaluate((el) => getComputedStyle(el, "::after").opacity);
  expect(await lineOpacity()).toBe("0");
  await page.hover(".rw-right");
  await expect.poll(lineOpacity).toBe("1");

  // Centered column: an edge moved 100px widens it by 200.
  await dragRight(page, 100);
  expect(await colWidth(page)).toBe(968);

  // Another file, reached in-app: same width.
  await page.locator(".markdown a", { hasText: "notes" }).click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/notes/readme\\.md$`));
  await page.waitForSelector("#content > .page.read");
  expect(await colWidth(page)).toBe(968);

  // In and out of fullscreen: same width.
  await page.click("#full-btn");
  await expect(page.locator("#exit-full")).toBeVisible();
  expect(await colWidth(page)).toBe(968);
  await page.click("#exit-full");
  await expect(page.locator("#topbar")).toBeVisible();
  expect(await colWidth(page)).toBe(968);

  // A reload is the reset.
  await page.reload();
  await page.waitForSelector("#content > .page.read");
  expect(await colWidth(page)).toBe(768);
});

test("the column stops at the floor, and a double-click puts the default back", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.waitForSelector("#content > .page.read .rw-right");

  await dragRight(page, -300);
  expect(await colWidth(page)).toBe(600);

  await page.dblclick(".rw-right");
  expect(await colWidth(page)).toBe(768);
  expect(await page.locator("#content > .page").getAttribute("style")).toBeFalsy();
});

test("the arrow keys resize from a focused handle", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.waitForSelector("#content > .page.read .rw-left");

  await page.focus(".rw-left");
  await page.keyboard.press("ArrowLeft"); // the left edge moving left is wider
  expect(await colWidth(page)).toBe(800);
  await page.keyboard.press("ArrowRight");
  await page.keyboard.press("ArrowRight");
  expect(await colWidth(page)).toBe(736);
});

test("there is no handle on a phone-width window", async ({ page }) => {
  await page.setViewportSize({ width: 800, height: 900 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.waitForSelector("#content > .page.read");
  await expect(page.locator(".rw-right")).toBeHidden();
  await expect(page.locator(".rw-left")).toBeHidden();
});
