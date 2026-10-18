import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* Hovering Edit prefetches the file's text so the page has something to show
   at once. That copy must never seed the session: editing always reads the
   file again first, so a write in between is what the editor opens on — and a
   copy older than a moment is not even shown. */
test("an edit session never starts from text prefetched before a newer write", async ({ page }) => {
  await login(page);
  const id = await wikiId(page);
  const name = `prefetch-${Date.now()}.md`;
  const put = (body: string) =>
    page.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`, {
      data: body,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  expect((await put("# Doc\n\nfirst version\n")).ok()).toBeTruthy();
  try {
    await page.goto(`/${id}/${name}`);
    await expect(page.locator("#content h1")).toHaveText("Doc");
    await page.hover("#edit-btn"); // prefetches the text
    await page.waitForTimeout(500);
    expect((await put("# Doc\n\nsecond version\n")).ok()).toBeTruthy();
    await page.waitForTimeout(2_500);

    await page.evaluate(() => {
      const w = window as unknown as { sawOld: boolean };
      w.sawOld = false;
      const check = () => {
        for (const el of document.querySelectorAll(".cm-preview, .cm-host .cm-content")) {
          if ((el.textContent || "").includes("first version")) w.sawOld = true;
        }
      };
      new MutationObserver(check).observe(document.body, { subtree: true, childList: true, characterData: true });
    });
    await page.click("#edit-btn");
    await expect(page.locator(".cm-host .cm-content")).toContainText("second version");
    expect(await page.evaluate(() => (window as unknown as { sawOld: boolean }).sawOld)).toBe(false);
  } finally {
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } }).catch(() => {});
  }
});

// Even a copy fetched a moment before the click is only shown: the editor is
// built from a read made after the click, so a write in between still wins.
test("a write just after the hover prefetch is what the editor opens on", async ({ page }) => {
  await login(page);
  const id = await wikiId(page);
  const name = `prefetch-${Date.now()}-b.md`;
  const put = (body: string) =>
    page.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`, {
      data: body,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  expect((await put("# Doc\n\nfirst version\n")).ok()).toBeTruthy();
  try {
    await page.goto(`/${id}/${name}`);
    await expect(page.locator("#content h1")).toHaveText("Doc");
    await page.hover("#edit-btn");
    await page.waitForTimeout(300);
    expect((await put("# Doc\n\nsecond version\n")).ok()).toBeTruthy();
    await page.click("#edit-btn");
    await expect(page.locator(".cm-host .cm-content")).toContainText("second version");
    await expect(page.locator(".cm-host .cm-content")).not.toContainText("first version");
  } finally {
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } }).catch(() => {});
  }
});
