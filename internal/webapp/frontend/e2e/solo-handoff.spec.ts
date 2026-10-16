import { test, expect, Page } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* An editor that gave up on the co-editing room stays solo, even when the
   room becomes reachable a moment later.

   Solo means this tab holds its own copy of the file and saves it itself. A
   provider still retrying underneath would, on its first good handshake,
   merge the hub's document (built from the same file) into that copy — the
   whole file twice — or leave the solo surface unbound from a room that
   now does the saving, so nothing typed after it reaches the file. Either
   way the check is the stored bytes: the edit is there, and the file's
   text appears exactly once. */

const PROJECT = "zz-solo-handoff";
let projectId = "";

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  return projectId;
}

async function put(page: Page, id: string, file: string, body: string) {
  const r = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: body, headers: { "Content-Type": "text/plain; charset=utf-8" } },
  );
  expect(r.ok()).toBeTruthy();
}

async function source(page: Page, id: string, file: string): Promise<string> {
  const r = await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`);
  expect(r.ok()).toBeTruthy();
  return r.text();
}

const count = (hay: string, needle: string) => hay.split(needle).length - 1;

/* Refuse the room until the editor has gone solo, then let it through. */
async function refuseThenAllow(page: Page): Promise<() => void> {
  let refuse = true;
  await page.routeWebSocket(/ycollab/, (ws) => {
    if (refuse) ws.close();
    else ws.connectToServer();
  });
  return () => {
    refuse = false;
  };
}

async function waitSoloThenOpen(page: Page, allow: () => void) {
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "solo", {
    timeout: 15_000,
  });
  allow();
  // Longer than y-websocket's reconnect backoff: a provider left running
  // would have completed its handshake by now.
  await page.waitForTimeout(4_000);
}

test("a solo source editor keeps saving after the room becomes reachable", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `solo-${Date.now()}.md`;
  const body = "# Solo note\n\nThe one and only paragraph.\n";
  await put(page, id, file, body);

  const allow = await refuseThenAllow(page);
  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await waitSoloThenOpen(page, allow);

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("Typed after the room came back.");

  await expect
    .poll(() => source(page, id, file), { timeout: 10_000, intervals: [250] })
    .toContain("Typed after the room came back.");
  const after = await source(page, id, file);
  expect(count(after, "The one and only paragraph.")).toBe(1);
  expect(count(after, "# Solo note")).toBe(1);
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "solo");
});

const HTML = `<!DOCTYPE html>
<html>
  <body>
    <h1>Solo report</h1>
    <p class="lead">Revenue grew steadily.</p>
  </body>
</html>
`;

test("a solo visual editor neither duplicates nor drops edits once the room is reachable", async ({
  page,
}) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `solo-${Date.now()}.html`;
  await put(page, id, file, HTML);

  const allow = await refuseThenAllow(page);
  await page.goto(`/${id}/${file}`);
  await page.click("#edit-btn");
  const frame = page.frameLocator("iframe.htmlview");
  await frame.locator("html[data-bd-ready]").waitFor();
  await waitSoloThenOpen(page, allow);

  const lead = frame.locator("p.lead");
  await lead.click();
  await page.keyboard.press("End");
  await page.keyboard.type(" Especially EMEA.");

  await expect
    .poll(() => source(page, id, file), { timeout: 10_000, intervals: [250] })
    .toContain("Revenue grew steadily. Especially EMEA.");
  const after = await source(page, id, file);
  expect(count(after, "<h1>Solo report</h1>")).toBe(1);
  expect(count(after, "<!DOCTYPE html>")).toBe(1);
  expect(count(after, "Revenue grew steadily.")).toBe(1);
});
