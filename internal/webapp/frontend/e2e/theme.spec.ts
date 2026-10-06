import { test, expect } from "@playwright/test";
import type { Page } from "@playwright/test";
import { login } from "./helpers";

// The appearance preference: System (default) follows the OS, Light and Dark
// override it, and the choice survives a reload without a flash of the other
// theme. The suite runs with an emulated dark OS (playwright.config.ts).

const theme = (page: Page) => page.evaluate(() => document.documentElement.dataset.theme);

test("System is the default and follows the OS live", async ({ page }) => {
  await login(page);
  await page.goto("/");
  expect(await theme(page)).toBe("dark");
  await page.emulateMedia({ colorScheme: "light" });
  await expect.poll(() => theme(page)).toBe("light");
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(255, 255, 255)");
  await page.click("#account-btn");
  await expect(page.locator("#menu-theme-system")).toHaveAttribute("data-state", "checked");
});

test("Light overrides a dark OS, persists across reload, and System hands back", async ({ page }) => {
  await login(page);
  await page.goto("/");
  await page.click("#account-btn");
  await page.click("#menu-theme-light");
  expect(await theme(page)).toBe("light");
  // The menu stays open so the choice is visible before it is committed to.
  await expect(page.locator("#menu-theme-light")).toHaveAttribute("data-state", "checked");

  // Applied before first paint: the attribute is already set when <body> is
  // parsed — before the deferred bundle runs — so it is the inline bootstrap
  // in index.html that did it. Recorded on <html data-first-theme>.
  await page.addInitScript(() => {
    const obs = new MutationObserver(() => {
      if (!document.body) return;
      document.documentElement.dataset.firstTheme = document.documentElement.dataset.theme ?? "unset";
      obs.disconnect();
    });
    obs.observe(document, { childList: true, subtree: true });
  });
  await page.reload();
  expect(await page.evaluate(() => document.documentElement.dataset.firstTheme)).toBe("light");
  await expect(page.locator("body")).toHaveCSS("background-color", "rgb(255, 255, 255)");

  await page.click("#account-btn");
  await page.click("#menu-theme-system");
  expect(await theme(page)).toBe("dark");
  expect(await page.evaluate(() => localStorage.getItem("bdrive-theme"))).toBeNull();

  // Printing a dark-themed page: the inline color-scheme the theme writes on
  // <html> must not keep the UA canvas dark under the print block's black ink.
  await page.emulateMedia({ media: "print" });
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme)).toBe("light");
});

test("the sign-in page honours the same preference", async ({ browser }) => {
  const ctx = await browser.newContext({ colorScheme: "light" });
  const page = await ctx.newPage();
  await page.goto("/auth/login");
  expect(await theme(page)).toBe("light");
  await page.evaluate(() => localStorage.setItem("bdrive-theme", "dark"));
  await page.reload();
  expect(await theme(page)).toBe("dark");
  // Dark chosen on a light OS: the UA-painted parts (autofill, scrollbars,
  // native controls) must go dark with the card, not stay light.
  expect(await page.evaluate(() => getComputedStyle(document.documentElement).colorScheme)).toBe("dark");
  await ctx.close();
});
