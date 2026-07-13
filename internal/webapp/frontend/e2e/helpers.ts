import { Page } from "@playwright/test";

export const ADMIN = "e2e@example.com";
export const MEMBER = "member@example.com";
export const PASSWORD = "e2e-pass-1";

// Signs in through the server-rendered /auth pages and waits for the SPA
// shell to render.
export async function login(page: Page, email: string = ADMIN) {
  await page.goto("/");
  await page.waitForURL(/auth\/login/);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', PASSWORD);
  await page.click("form button");
  await page.waitForSelector("#sidebar");
}
