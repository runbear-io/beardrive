import { Page } from "@playwright/test";

export const ADMIN = "e2e@example.com";
export const MEMBER = "member@example.com";
export const PASSWORD = "e2e-pass-1";

// One real form login per identity per run, then the session cookie is
// reused — the server rate-limits credential POSTs to 10/min per IP, which
// a fresh login in every spec would trip.
type Cookies = Awaited<ReturnType<ReturnType<Page["context"]>["cookies"]>>;
const sessions = new Map<string, Cookies>();

// Signs in (through the server-rendered /auth pages on first use) and waits
// for the SPA shell to render.
export async function login(page: Page, email: string = ADMIN) {
  const cached = sessions.get(email);
  if (cached) {
    await page.context().addCookies(cached);
    await page.goto("/");
    await page.waitForSelector("#sidebar");
    return;
  }
  await page.goto("/");
  await page.waitForURL(/auth\/login/);
  await page.fill('input[name="email"]', email);
  await page.fill('input[name="password"]', PASSWORD);
  await page.click("form button");
  await page.waitForSelector("#sidebar");
  sessions.set(email, await page.context().cookies());
}

export async function wikiId(page: Page): Promise<string> {
  const out = await (await page.request.get("/api/projects")).json();
  return out.projects.find((p: { name: string }) => p.name === "wiki").id;
}
