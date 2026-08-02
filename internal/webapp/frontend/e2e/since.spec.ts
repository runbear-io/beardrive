import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// "What's new" is the history feed anchored at a localStorage marker. Every
// Playwright test gets a fresh context, so storage starts empty and "first
// visit" needs no setup — which is also why each test that wants a stored
// marker has to make one by visiting the page first.

test("first visit falls back to the last 7 days and says so", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/since`);
  await expect(page.locator(".since-sub")).toContainText("showing the last 7 days");
  // The seeded ops are 90min–72h old, so they are all inside the window.
  await expect(page.locator(".hrun-note")).toContainText("claude-code session 8f21e4");
});

test("a change lands, then the revisit is empty", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/since`); // stamps the marker
  await expect(page.locator(".since-sub")).toContainText("since your last visit");

  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("since-probe.md")}`,
    { data: "# Probe\n" },
  );
  await page.goto(`/${pid}/since`);
  await expect(page.locator(".since-sub")).toContainText("1 change since your last visit");
  await expect(page.locator(".hpath")).toHaveText(["since-probe.md"]);

  await page.goto(`/${pid}/since`);
  await expect(page.locator(".since-sub")).toContainText("Nothing new since your last visit");
  await expect(page.locator(".hpath")).toHaveCount(0);
});

test("only this view stamps the marker", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/since`); // marker: now
  await expect(page.locator(".since-sub")).toContainText("since your last visit");

  // A change lands, then the user wanders through the other project pages.
  // If any of them stamped, the change below would be swallowed.
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("since-stamp-probe.md")}`,
    { data: "# Stamp probe\n" },
  );
  await page.goto(`/${pid}/history`);
  await expect(page.locator(".history")).toBeVisible();
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-treemap")).toBeVisible();

  await page.goto(`/${pid}/since`);
  await expect(page.locator(".hpath")).toHaveText(["since-stamp-probe.md"]);
});

test("the baseline stays put while the page is open", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/since`);
  await expect(page.locator(".since-sub")).toContainText("showing the last 7 days");
  const shown = (await page.locator(".since-sub").textContent()) || "";
  // The marker was written under us; the page must not notice.
  await page.waitForTimeout(1200);
  await expect(page.locator(".since-sub")).toHaveText(shown);
});

test("the nav item navigates there and is the only active row", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.click("#nav-since");
  await expect(page).toHaveURL(new RegExp(`/${pid}/since$`));
  await expect(page.locator(".nav-menu .row.active")).toHaveCount(1);
  await expect(page.locator("#nav-since")).toHaveClass(/active/);
  // A deep link survives a hard reload (SPA fallback), not just client nav.
  await page.reload();
  await expect(page.locator(".since-head")).toHaveText("What's new");
});
