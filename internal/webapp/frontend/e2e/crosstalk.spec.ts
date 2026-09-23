import { test, expect, Page } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* Two different files open in two tabs of ONE browser must not mix.

   They did. y-websocket keys its cross-tab BroadcastChannel on
   serverUrl + "/" + roomname and nothing else, and the client passed one
   fixed room name for every file with the path tucked into the query params.
   So every editor in the same browser shared one channel and applied each
   other's Y.Doc updates: type in one file, watch it appear in the other.

   SAME CONTEXT on purpose. BroadcastChannel is per origin per browser
   profile; two Playwright contexts are two profiles and cannot hear each
   other, so a test built on newContext() passes against the bug. */

const PROJECT = "zz-crosstalk";
let projectId = "";

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  const perm = await page.request.put(`/api/p/${projectId}/permissions`, {
    data: { default: "write" },
  });
  expect(perm.ok()).toBeTruthy();
  return projectId;
}

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

test("two files in two tabs of one browser stay two files", async ({ context }) => {
  test.setTimeout(90_000);
  const a = await context.newPage();
  await login(a, ADMIN);
  const id = await project(a);
  const stamp = Date.now();
  const fileA = `alpha-${stamp}.md`;
  const fileB = `beta-${stamp}.md`;
  for (const [f, body] of [
    [fileA, "ALPHA-BASE\n"],
    [fileB, "BETA-BASE\n"],
  ]) {
    const r = await a.request.put(
      `/api/p/${id}/upload/content?path=${encodeURIComponent(f)}`,
      { data: body },
    );
    expect(r.ok()).toBeTruthy();
  }

  // Second TAB, same context: shares cookies, shares BroadcastChannel.
  const b = await context.newPage();
  await a.goto(`/${id}/edit/${fileA}`);
  await b.goto(`/${id}/edit/${fileB}`);
  for (const pg of [a, b]) await pg.waitForSelector(".cm-host .cm-content");
  await a.waitForTimeout(2_500); // both rooms joined

  await a.locator(".cm-host .cm-line").first().click();
  await a.keyboard.press("End");
  await a.keyboard.type(" TYPED-IN-ALPHA");
  await a.waitForTimeout(3_000);

  // Control: A really did take the keystrokes.
  await expect(a.locator(".cm-host .cm-content")).toContainText("TYPED-IN-ALPHA");

  // The point: B is a different document and must not have them.
  const bText = await b.locator(".cm-host .cm-content").innerText();
  expect(
    bText,
    `text typed into ${fileA} appeared in ${fileB}, which is open in another tab ` +
      `of the same browser — two documents are sharing one channel`,
  ).not.toContain("TYPED-IN-ALPHA");
  expect(bText, "B lost its own content").toContain("BETA-BASE");

  // And on the hub, each file has only its own text.
  await a.waitForTimeout(3_000);
  const read = async (p: string) =>
    (await a.request.get(`/api/p/${id}/file?path=${encodeURIComponent(p)}`)).text();
  expect(await read(fileB)).not.toContain("TYPED-IN-ALPHA");
  expect(await read(fileA)).toContain("TYPED-IN-ALPHA");
});
