import { test, expect } from "@playwright/test";
import { login } from "./helpers";

test("unauthenticated visit redirects to the login page", async ({ page }) => {
  await page.goto("/");
  await page.waitForURL(/auth\/login/);
  await expect(page.locator('input[name="email"]')).toBeVisible();
});

/* The title asserted against what the app actually landed on.

   It used to be `toHaveTitle(/BearDrive/)`, with a comment claiming hub mode
   renders "<project> — BearDrive". It does not: titleForRoute returns the
   SCOPE ALONE when a route has no page, so a project's root is titled with
   the project's name and nothing else. The brand only appears when there is
   no project to name — so that assertion passed exactly while this hub had no
   projects yet, and broke the moment any earlier spec created one (hub.spec
   makes "brought-my-own", and sorts first). It was the suite's most frequent
   red, and never once about this page.

   Asserting the landed project's own name tests the wiring that matters and
   cannot be perturbed by what another spec left behind. */
test("login lands in the app shell", async ({ page }) => {
  await login(page);
  await expect(page.locator("#sidebar")).toBeVisible();
  await expect(page.locator("#topbar")).toBeVisible();

  const projects = (await (await page.request.get("/api/projects")).json())
    .projects as { id: string; name: string }[];
  const landed = projects.find((p) => page.url().includes(p.id));
  // A hub with no projects at all titles itself with the brand instead.
  await expect(page).toHaveTitle(landed ? landed.name : /BearDrive/);
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
