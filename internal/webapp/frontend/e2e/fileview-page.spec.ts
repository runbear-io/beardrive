import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* The file page while it loads, its breadcrumb, and its provenance line. */

test.use({ viewport: { width: 1440, height: 900 } });

test("a skeleton holds the page while the render is in flight", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  let release!: () => void;
  const held = new Promise<void>((r) => (release = r));
  await page.route("**/render?*", async (route) => {
    await held;
    await route.continue();
  });
  await page.goto(`/${pid}/index.md`);
  const skel = page.locator("#content .fskel");
  await expect(skel).toBeVisible();
  await expect(skel.locator(".fskel-title")).toHaveCount(1);
  await expect(skel.locator(".fskel-meta")).toHaveCount(1);
  await expect(skel.locator(".fskel-line")).toHaveCount(6);
  await expect(skel.locator(".fskel-rail")).toBeVisible();
  release();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await expect(skel).toHaveCount(0);
});

test("breadcrumb keeps the first and last segments, cuts a long middle one", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const mid = "a-very-long-folder-name-that-will-not-fit";
  const file = `notes/${mid}/AGENTS.md`;
  const r = await page.request.put(`/api/p/${pid}/upload/content?path=${encodeURIComponent(file)}`, {
    data: "# Agents\n",
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
  expect(r.ok()).toBeTruthy();
  try {
    await page.goto(`/${pid}/${file}`);
    await expect(page.locator("#content h1")).toHaveText("Agents");
    const crumb = page.locator("#crumb");
    await expect(crumb.locator(".crumbs")).toHaveAttribute("title", file);
    await expect(crumb.locator(".crumb-sep").first()).toHaveText("›");
    const cut = (sel: string) =>
      crumb.locator(sel).evaluate((el) => el.scrollWidth > el.clientWidth + 1);
    expect(await cut(`.crumb-seg[title="notes/${mid}"]`)).toBe(true);
    expect(await cut('.crumb-seg[title="notes"]')).toBe(false);
    expect(await cut(".crumb-last")).toBe(false);
    await expect(crumb.locator(".crumb-last")).toHaveText("AGENTS.md");
    await expect(crumb.locator(`.crumb-seg[title="notes/${mid}"]`)).toHaveText(mid); // full text in the DOM
  } finally {
    await page.request.post(`/api/p/${pid}/remove`, { data: { path: file } });
  }
});

test("meta names the editor and gives an absolute local time", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  const who = page.locator(".fv-meta .fv-who");
  await expect(who).toContainText("Alice edited");
  await expect(who).not.toContainText("@");
  await expect(page.locator(".fv-meta .fv-when")).toHaveText(/^\s*[A-Z][a-z]{2} \d{1,2}, \d{4} · \d{1,2}:\d{2}\s?[AP]M\s*$/);
});
