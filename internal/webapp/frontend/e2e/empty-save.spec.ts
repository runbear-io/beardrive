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

  // The guard blocks the editor until it is answered; keeping the saved
  // version is the default and puts the file's bytes back in the buffer.
  await expect(page.locator("#shrink-dialog")).toBeVisible({ timeout: 10_000 });
  await page.unrouteAll({ behavior: "ignoreErrors" });
  const after = await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`);
  expect(await after.text()).toBe(body);
  await page.locator("#shrink-keep").click();
  await expect(page.locator("#shrink-dialog")).toBeHidden();
  await expect(page.locator(".cm-host .cm-content")).toContainText("shipped the thing");
  await page.waitForTimeout(1_500);
  const kept = await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`);
  expect(await kept.text()).toBe(body);
});

/* The question belongs to the editor that asked it. Leaving that editor —
   another file, or out of edit mode — while the dialog is up must close it
   without answering: neither file is written, and nothing from the old
   editor can run later. */
for (const leave of ["another file", "read mode"] as const) {
  test(`leaving for ${leave} with the shrink question open writes nothing`, async ({ page }) => {
    test.setTimeout(60_000);
    await login(page, ADMIN);
    if (!projectId) {
      const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
      expect(r.ok()).toBeTruthy();
      projectId = (await r.json()).project.id;
    }
    const id = projectId;
    const file = `leave-${Date.now()}.md`;
    const other = `other-${Date.now()}.md`;
    const body = "## Snow\n" + "- shipped the thing\n".repeat(60);
    const otherBody = "## Other\nuntouched\n";
    const at = (f: string) => `/api/p/${id}/upload/content?path=${encodeURIComponent(f)}`;
    expect((await page.request.put(at(file), { data: body })).ok()).toBeTruthy();
    expect((await page.request.put(at(other), { data: otherBody })).ok()).toBeTruthy();

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
    await expect(page.locator("#shrink-dialog")).toBeVisible({ timeout: 10_000 });

    const writes: string[] = [];
    page.on("request", (r) => {
      if (r.method() !== "GET" && r.url().includes("/upload/")) writes.push(r.method() + " " + r.url());
    });
    const to = leave === "another file" ? `/${id}/edit/${other}` : `/${id}/${file}`;
    await page.evaluate((u) => {
      history.pushState(null, "", u);
      dispatchEvent(new PopStateEvent("popstate"));
    }, to);

    await expect(page.locator("#shrink-dialog")).toBeHidden({ timeout: 5_000 });
    await page.waitForTimeout(2_000);
    expect(writes).toEqual([]);
    await page.unrouteAll({ behavior: "ignoreErrors" });
    expect(await (await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)).text()).toBe(body);
    expect(await (await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(other)}`)).text()).toBe(otherBody);
  });
}
