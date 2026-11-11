import { test, expect, Page } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* An editor whose co-editing room is slow to answer lets the user type at
   once, and joins the room cleanly when it does answer.

   A hub that refuses the socket for a while (a deploy rolling, a proxy that
   will not upgrade yet) is not an older hub: past STALLED_MS the editor
   mounts a staging buffer, checkpoints its typing into the room over
   POST ycollab/sync, and moves onto the shared document once the socket
   syncs. The danger in that hand-off is the stored bytes: the room's
   document is built from the same file, so a careless merge writes the
   whole file twice, and a careless swap drops what was typed before it.
   Either way the check is the file: every edit is there, and every line of
   it appears exactly once.

   A hub with no room at all — no socket and no ycollab/sync route — never
   goes live: the buffer saves the file itself, with If-Match, as solo. */

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

/* Refuse the room's socket until allowed, then let it through. */
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

/* The room stays refused well past STALLED_MS while the user already types,
   then opens; the editor must reach the live room. */
async function stallThenOpen(page: Page, allow: () => void) {
  await page.waitForTimeout(4_000);
  await expect(page.locator("#editor-state")).not.toHaveAttribute("data-collab", "live");
  allow();
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "live", {
    timeout: 20_000,
  });
}

test("a source editor typed into while the room stalled joins it without duplicating or dropping", async ({
  page,
}) => {
  test.setTimeout(90_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `stall-${Date.now()}.md`;
  const body = "# Stalled note\n\nThe one and only paragraph.\n";
  await put(page, id, file, body);

  const allow = await refuseThenAllow(page);
  await page.goto(`/${id}/edit/${file}`);
  // The staging buffer: mounted once the room has stalled, editable at once.
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("Typed while the room was down.");
  // Checkpointed into the room over HTTP: on the hub before any socket.
  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("Typed while the room was down.");

  await stallThenOpen(page, allow);

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.press("Enter");
  await page.keyboard.type("Typed after the room came back.");

  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("Typed after the room came back.");
  const after = await source(page, id, file);
  expect(count(after, "# Stalled note")).toBe(1);
  expect(count(after, "The one and only paragraph.")).toBe(1);
  expect(count(after, "Typed while the room was down.")).toBe(1);
  expect(count(after, "Typed after the room came back.")).toBe(1);
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "live");
});

test("an older hub with no room route never goes live and saves without duplicating", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `solo-${Date.now()}.md`;
  const body = "# Solo note\n\nThe one and only paragraph.\n";
  await put(page, id, file, body);

  // Neither the socket nor the HTTP room route exists on an older hub.
  await page.routeWebSocket(/ycollab/, (ws) => ws.close());
  await page.route("**/ycollab/sync*", (route) => route.fulfill({ status: 404, body: "not found" }));
  const filePuts: string[] = [];
  page.on("request", (r) => {
    if (r.method() === "PUT" && r.url().includes("/upload/content?")) filePuts.push(r.url());
  });

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.type("Typed with no room at all.");

  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("Typed with no room at all.");
  // Saved by the tab itself — and it keeps saving: nothing ever goes live.
  expect(filePuts.length).toBeGreaterThan(0);
  await page.keyboard.type(" And more.");
  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("Typed with no room at all. And more.");
  const after = await source(page, id, file);
  expect(count(after, "# Solo note")).toBe(1);
  expect(count(after, "The one and only paragraph.")).toBe(1);
  expect(count(after, "Typed with no room at all. And more.")).toBe(1);
  await expect(page.locator("#editor-state")).not.toHaveAttribute("data-collab", "live");
});

const HTML = `<!DOCTYPE html>
<html>
  <body>
    <h1>Solo report</h1>
    <p class="lead">Revenue grew steadily.</p>
  </body>
</html>
`;

test("a visual editor typed into while the room stalled joins it without duplicating or dropping", async ({
  page,
}) => {
  test.setTimeout(90_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `stall-${Date.now()}.html`;
  await put(page, id, file, HTML);

  const allow = await refuseThenAllow(page);
  await page.goto(`/${id}/${file}`);
  await page.click("#edit-btn");
  const frame = page.frameLocator("iframe.htmlview");
  await frame.locator("html[data-bd-ready]").waitFor();
  await page.waitForTimeout(3_000); // past STALLED_MS: the staging document is up

  // Typed into the staging document while the socket is still refused.
  await frame.locator("p.lead").click();
  await page.keyboard.press("End");
  await page.keyboard.type(" Especially EMEA.");
  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("Revenue grew steadily. Especially EMEA.");

  await stallThenOpen(page, allow);
  await frame.locator("html[data-bd-ready]").waitFor();

  await frame.locator("h1").click();
  await page.keyboard.press("End");
  await page.keyboard.type(" Q3");

  await expect
    .poll(() => source(page, id, file), { timeout: 15_000, intervals: [250] })
    .toContain("<h1>Solo report Q3</h1>");
  const after = await source(page, id, file);
  expect(count(after, "<!DOCTYPE html>")).toBe(1);
  expect(count(after, "<h1>Solo report Q3</h1>")).toBe(1);
  expect(count(after, "Solo report")).toBe(1);
  expect(count(after, "Revenue grew steadily. Especially EMEA.")).toBe(1);
  expect(count(after, "Revenue grew steadily.")).toBe(1);
});
