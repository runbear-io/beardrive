import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

test("insights via ⋯ scopes to the open file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes/readme.md`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Insights')");
  await expect(page).toHaveURL(`/${pid}/insights/notes/readme.md`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes/readme.md");
  // The subject stays selected in the tree; Dashboard does NOT light up.
  await expect(page.locator('#tree .row[data-path="notes/readme.md"]')).toHaveClass(/active/);
  await expect(page.locator("#nav-dashboard")).not.toHaveClass(/active/);
});

test("insights via ⋯ scopes to the selected folder", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Insights')");
  await expect(page).toHaveURL(`/${pid}/insights/notes`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes");
  await expect(page.locator('#tree .row[data-path="notes"]')).toHaveClass(/active/);
  await expect(page.locator("#nav-dashboard")).not.toHaveClass(/active/);
});

test("root Dashboard still lights the menu, not the tree", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.click("#nav-dashboard");
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  await expect(page.locator("#tree .row.active")).toHaveCount(0);
});
