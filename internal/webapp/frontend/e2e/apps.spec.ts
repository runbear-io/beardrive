import { test, expect } from "@playwright/test";
import { login, wikiId, expectToast } from "./helpers";

// Apps (org settings) and the Data setting (project settings): a server's
// own key, and the switch that lets it query a project's data. Mutating specs
// clean up after themselves; the suite shares one hub per run.

async function openOrgSettings(page: import("@playwright/test").Page) {
  await page.click("#project-select");
  await page.click("#menu-org-settings");
}

test("org admin: an app's key is shown once, then listed by its last four", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await page.click("#org-app-new");
  await page.fill("#modal-input", "Dashboard");
  await page.click(".modal .pbtn");
  await expectToast(page, "App created");
  const app = page.locator(".admin-item", { hasText: "Dashboard" });
  await expect(app).toContainText("app:");

  await app.getByRole("button", { name: "New key" }).click();
  const shown = await page.locator("#modal-input").inputValue();
  expect(shown).toMatch(/^bda_[0-9a-f]+$/);
  await page.click(".modal .pbtn");
  const key = page.locator(".admin-item", { hasText: "bda_…" + shown.slice(-4) });
  await expect(key).toContainText("never used");
  // The key itself is never on the page again.
  await expect(page.locator("body")).not.toContainText(shown);

  await key.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Revoked");
  await app.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expectToast(page, "Deleted");
  await expect(page.locator(".admin-item", { hasText: "Dashboard" })).toHaveCount(0);
});

test("project settings: data queries turn on and off", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/settings`);
  await page.click("#ps-data-toggle");
  await expectToast(page, "Data queries are on");
  await expect(page.locator("#ps-data-toggle")).toHaveText("Turn off data queries");
  const r = await page.request.post(`/api/p/${pid}/data/query`, { data: { pattern: "**/*.md", count: true } });
  expect(r.status()).toBe(200);
  await page.click("#ps-data-toggle");
  await expectToast(page, "Data queries are off");
  expect((await page.request.post(`/api/p/${pid}/data/query`, { data: { pattern: "**/*.md" } })).status()).toBe(404);
});
