import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* Hovering Edit prefetches the file's text so the editor opens at once. That
   copy must never seed a session once it is more than a moment old: a write in
   between would otherwise open the editor on the previous version. */
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
