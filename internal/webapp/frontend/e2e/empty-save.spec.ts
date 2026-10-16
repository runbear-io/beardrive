import { test, expect } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* An editor that joins before the file's content has loaded must never save
   an empty document over the file.

   Four daily-meeting notes were overwritten with 0 bytes this way: each save
   carried the right If-Match, so the conflict check passed, and only the
   text was missing. Here the read arrives empty and the co-editing socket
   never opens, so the editor holds a blank buffer built on the real version.
   Its save must be refused, and the file must keep its bytes. */

const PROJECT = "zz-empty-save";
let projectId = "";

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

test("an editor that opened before its content never empties the file", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  const id = projectId;
  const file = `daily-${Date.now()}.md`;
  const body = "## Snow\n" + "- shipped the thing\n".repeat(60);
  const put = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: body },
  );
  expect(put.ok()).toBeTruthy();

  await page.routeWebSocket(/ycollab/, (ws) => ws.close());
  await page.route(
    (u) => u.pathname === `/api/p/${id}/file` && u.searchParams.get("path") === file,
    async (route) => {
      const real = await route.fetch();
      await route.fulfill({ response: real, body: "" });
    },
  );

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.type("x");

  await expect(page.getByText(/Not saved: this would empty or cut most of/)).toBeVisible({
    timeout: 10_000,
  });
  await page.unroute(() => true);
  const after = await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`);
  expect(await after.text()).toBe(body);
});
