import { test, expect } from "@playwright/test";
import { login } from "./helpers";

test("unauthenticated visit redirects to the login page", async ({ page }) => {
  await page.goto("/");
  await page.waitForURL(/auth\/login/);
  await expect(page.locator('input[name="email"]')).toBeVisible();
});

test("login lands in the app shell", async ({ page }) => {
  await login(page);
  await expect(page).toHaveTitle(/BearDrive/); // "<project> — BearDrive" in hub mode
  await expect(page.locator("#sidebar")).toBeVisible();
  await expect(page.locator("#topbar")).toBeVisible();
});

test("hashed assets are served immutable, shell revalidates", async ({ page, request }) => {
  await login(page);
  const src = await page.locator('script[type="module"]').getAttribute("src");
  expect(src).toMatch(/^\/assets\/.+\.js$/);
  const asset = await request.get(src!);
  expect(asset.headers()["cache-control"]).toContain("immutable");
  const shell = await request.get("/");
  expect(shell.headers()["cache-control"]).toBe("no-cache");
});

test("mistyped API path is a real 404, not the shell", async ({ page }) => {
  await login(page); // unauthenticated /api/* is a 401 before routing
  const res = await page.request.get("/api/nope");
  expect(res.status()).toBe(404);
});

test("search button shows the shortcut tooltip on hover", async ({ page }) => {
  const { login } = await import("./helpers");
  await login(page);
  await page.hover("#search-btn");
  const tip = page.locator('[role="tooltip"], .tip').filter({ hasText: "Search" }).first();
  await expect(tip).toBeVisible();
  await expect(tip).toContainText("⌘");
});
