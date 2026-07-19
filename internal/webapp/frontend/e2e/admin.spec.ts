import { test, expect } from "@playwright/test";
import { login, wikiId, ADMIN, MEMBER } from "./helpers";

// Phase 4: org admin (rename, members, projects, invites, share audit) and
// hub settings (policy toggles, pending queue). Panels are not routes —
// navigation closes them. Mutating specs revert their changes: the suite
// shares one hub per run.

// The org panel opens from the account menu (sidebar footer).
async function openOrgSettings(page: import("@playwright/test").Page) {
  await page.click("#account-btn");
  await page.click("#menu-org-settings");
}

test("org admin: members with roles, self marked, rename round-trip", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await expect(page.locator("#org-title")).toHaveText("default");
  await expect(page.locator("#crumb")).toHaveText("default");
  await expect(page.locator(".admin-item", { hasText: ADMIN })).toContainText("(you)");
  const memberRow = page.locator(".admin-item", { hasText: MEMBER });
  await expect(memberRow.locator("select")).toHaveValue("member");

  // Rename and revert
  await page.fill("#org-rename", "renamed-org");
  await page.click("#org-rename-btn");
  await expect(page.locator("#toast")).toContainText("Renamed");
  await page.click("#account-btn");
  await expect(page.locator("#menu-org-settings")).toContainText("renamed-org");
  await page.keyboard.press("Escape");
  await page.fill("#org-rename", "default");
  await page.click("#org-rename-btn");
  await page.click("#account-btn");
  await expect(page.locator("#menu-org-settings")).toContainText("default");
  await page.keyboard.press("Escape");
});

test("org admin: member role change round-trip", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  const sel = page.locator(".admin-item", { hasText: MEMBER }).locator("select");
  await sel.selectOption("owner");
  await expect(page.locator("#toast")).toContainText("Role updated");
  await expect(sel).toHaveValue("owner");
  await sel.selectOption("member");
  await expect(sel).toHaveValue("member");
});

test("org admin: invite create shows in list, revoke removes it", async ({ page }) => {
  await login(page);
  await openOrgSettings(page);
  await page.click(".admin-h .pbtn"); // New invite
  await expect(page.locator("#toast")).toContainText("Invite");
  const row = page.locator(".admin-item", { hasText: "/join/" }).first();
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("unused");
  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn"); // confirm revoke
  await expect(page.locator("#toast")).toContainText("Revoked");
  await expect(page.locator(".admin-item", { hasText: "/join/" })).toHaveCount(0);
});

test("org admin: public share audit lists and revokes", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.request.post(`/api/p/${pid}/shares`, { data: { path: "index.md" } });
  await openOrgSettings(page);
  const row = page.locator(".admin-item", { hasText: "index.md" });
  await expect(row).toBeVisible();
  await expect(row.locator(".ai-tag")).toContainText("wiki");
  await row.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expect(page.locator("#toast")).toContainText("Share revoked");
  await expect(page.locator(".admin-item", { hasText: "index.md" })).toHaveCount(0);
});

test("org admin: project rename and delete", async ({ page }) => {
  await login(page);
  await page.request.post("/api/projects", { data: { name: "doomed" } });
  await page.reload(); // pick up the new project
  await openOrgSettings(page);
  const row = page.locator(".admin-item", { hasText: "doomed" });
  await row.locator(".ai-btn", { hasText: "Rename" }).click();
  await page.fill(".modal-input", "doomed-2");
  await page.click(".modal .pbtn");
  await expect(page.locator("#toast")).toContainText("Renamed");
  const row2 = page.locator(".admin-item", { hasText: "doomed-2" });
  await expect(row2).toBeVisible();
  await row2.locator(".ai-del").click();
  await page.click(".modal .danger-btn");
  await expect(page.locator("#toast")).toContainText("Deleted");
  await expect(page.locator(".admin-item", { hasText: "doomed-2" })).toHaveCount(0);
  await expect(page.locator("#projects .row .label", { hasText: "doomed-2" })).toHaveCount(0);
});

test("member sees the org panel read-only", async ({ page }) => {
  await login(page, MEMBER);
  await openOrgSettings(page);
  await expect(page.locator("#org-title")).toContainText("member");
  await expect(page.locator("#org-rename")).toHaveCount(0);
  await expect(page.locator(".admin-item select")).toHaveCount(0);
  await expect(page.locator(".admin-item .ai-tag").first()).toBeVisible(); // role tags
});

test("hub settings: policy view, save round-trip, pending queue empty", async ({ page }) => {
  await login(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator("#crumb")).toHaveText("Signup & access");
  await expect(page.locator(".admin h1")).toHaveText("Signup & access");
  // Server has no SMTP: verification toggle disabled
  const ver = page.locator(".admin-item.toggle").first().locator("input");
  await expect(ver).toBeDisabled();
  await expect(page.locator(".admin-item", { hasText: "Self-signup" })).toContainText("invite-only");
  await expect(page.locator(".admin-item", { hasText: "Hub admins" })).toContainText(ADMIN);
  // Toggle approval on, save, revert
  const app = page.locator(".admin-item.toggle").nth(1).locator("input");
  await app.check();
  await page.click(".admin > .pbtn");
  await expect(page.locator("#toast")).toContainText("policy saved");
  await app.uncheck();
  await page.click(".admin > .pbtn");
  await expect(page.locator("#toast")).toContainText("policy saved");
  await expect(page.locator(".admin-empty", { hasText: "No one is waiting" })).toBeVisible();
});

test("navigating away closes an open admin panel", async ({ page }) => {
  await login(page);
  await page.click("#account-btn");
  await page.click("#menu-hub-admin");
  await expect(page.locator(".admin h1")).toBeVisible();
  await page.click('#tree .row[data-path="index.md"]');
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(page.locator(".admin")).toHaveCount(0);
});
