import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* Hovering Edit prefetches the file's text so the editor can open on it in
   the first frame. A copy the change stream has since invalidated — a newer
   write landed after it was read — is never painted: editing waits for the
   fresh read instead. */
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
        for (const el of document.querySelectorAll(".cm-host .cm-content")) {
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

// A write the change stream has not reported yet when Edit is clicked: the
// editor may open on the prefetched copy, and the read it always makes on the
// way in folds the newer write into the open document.
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

// The room is opened while editing reads the file, and the hub seeds a room
// when it creates it — so a write landing between the two leaves a room
// seeded with the older text, while the reading page had already warmed that
// older text too. Whatever the editor opens on, it must settle on the newer
// write, with the older text gone from the document.
test("a room seeded before the edit's own read is not the one the editor opens on", async ({ page }) => {
  await login(page);
  const id = await wikiId(page);
  const name = `prefetch-${Date.now()}-room.md`;
  const put = (body: string) =>
    page.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`, {
      data: body,
      headers: { "Content-Type": "text/plain; charset=utf-8" },
    });
  expect((await put("# Doc\n\nfirst version\n")).ok()).toBeTruthy();
  try {
    await page.goto(`/${id}/${name}`);
    await expect(page.locator("#content h1")).toHaveText("Doc");
    // Hold the edit page's read until the room is open, then change the file.
    let held = false;
    await page.route(`**/file?path=${encodeURIComponent(name)}*`, async (route) => {
      if (!held) {
        held = true;
        await page.waitForTimeout(700);
        expect((await put("# Doc\n\nsecond version\n")).ok()).toBeTruthy();
      }
      await route.continue();
    });
    await page.click("#edit-btn");
    await expect(page.locator(".cm-host .cm-content")).toContainText("second version");
    // Settled, not just passing through: nothing puts the older text back.
    await page.waitForTimeout(500);
    await expect(page.locator(".cm-host .cm-content")).toContainText("second version");
    await expect(page.locator(".cm-host .cm-content")).not.toContainText("first version");
  } finally {
    await page.unrouteAll({ behavior: "ignoreErrors" });
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } }).catch(() => {});
  }
});
