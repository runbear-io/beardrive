import { test, expect } from "@playwright/test";
import { login, wikiId, MEMBER, PASSWORD } from "./helpers";

// Phase 1: shell, session flags, project list/selection, routing, empty
// state, invite accept. Mutating specs (project creation) run last —
// specs share one seeded hub per run.

test("landing selects the first project and rewrites the URL", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await expect(page.locator("#vault-name")).toHaveText("wiki");
  await expect(page).toHaveTitle("wiki — BearDrive");
  await expect(page.locator("#projects .row.active .label")).toHaveText("wiki");
});

test("deep link to a project resolves after reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto("/" + pid);
  await expect(page.locator("#vault-name")).toHaveText("wiki");
  await expect(page).toHaveURL("/" + pid);
});

test("unknown project id falls back to a real project", async ({ page }) => {
  await login(page);
  await page.goto("/p-00000000");
  await page.waitForURL(/\/p-[0-9a-f]{8}$/);
  await expect(page.locator("#vault-name")).not.toHaveText("…");
});

test("admin sees admin bar and org Manage; member does not", async ({ page, browser }) => {
  await login(page); // admin, owner of "default"
  await expect(page.locator("#adminbar")).toBeVisible();
  await expect(page.locator("#orgbar #org-name")).toHaveText("default");
  await expect(page.locator("#org-settings-btn")).toBeVisible();
  await expect(page.locator("#signout")).toBeVisible();

  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await login(p2, MEMBER);
  await expect(p2.locator("#orgbar #org-name")).toHaveText("default");
  await expect(p2.locator("#adminbar")).toHaveCount(0);
  await expect(p2.locator("#org-settings-btn")).toHaveCount(0);
  await ctx.close();
});

test("join link accepts an invite after sign-in", async ({ page, browser }) => {
  await login(page); // admin mints the invite
  const orgs = await (await page.request.get("/api/orgs")).json();
  const org = orgs.orgs.find((o: { name: string }) => o.name === "default");
  const inv = await (
    await page.request.post(`/api/orgs/${org.id}/invites`, { data: {} })
  ).json();
  expect(inv.url).toContain("/join/");
  const token = inv.url.split("/join/")[1];

  // A signed-out visitor keeps the token through the login redirect.
  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await p2.goto("/join/" + token);
  await p2.waitForURL(/auth\/login/);
  await p2.fill('input[name="email"]', MEMBER);
  await p2.fill('input[name="password"]', PASSWORD);
  await p2.click("form button");
  await p2.waitForSelector("#toast.show");
  await expect(p2.locator("#toast")).toContainText("you joined");
  await p2.waitForURL(/\/p-[0-9a-f]{8}$/); // lands on the org's project
  await ctx.close();
});

test("no-org account gets the onboarding empty state and can create a project", async ({
  page,
}) => {
  await login(page, "solo@example.com");
  await expect(page.locator(".onboard h1")).toHaveText("Welcome to BearDrive");
  await page.fill("#ob-name", "solo-notes");
  await page.click("#ob-create");
  await page.waitForURL(/\/p-[0-9a-f]{8}$/);
  await expect(page.locator("#vault-name")).toHaveText("solo-notes");
  await expect(page.locator("#orgbar")).toBeVisible(); // fresh org, owner
});

test("new project via the sidebar + modal", async ({ page }) => {
  await login(page);
  await page.click("#projects .nav-add");
  await page.fill(".modal-input", "scratch");
  await page.click(".modal .pbtn");
  await page.waitForURL(/\/p-[0-9a-f]{8}$/);
  await expect(page.locator("#vault-name")).toHaveText("scratch");
  await expect(page.locator("#projects .row .label")).toContainText(["scratch", "wiki"]);
  await expect(page.locator("#toast")).toContainText("Created");
});
