import { test, expect, Page } from "@playwright/test";
import { login, ADMIN, MEMBER } from "./helpers";

/* A co-editor whose socket is down while the others keep typing.

   The change stream is a separate connection from the co-editing socket, so
   it routinely outlives it: the hub's snapshot of everyone else's typing
   reaches this browser as a changed file while its document cannot hear the
   room. y-websocket has also dropped every other participant from awareness
   by then, so the browser looks alone. If it splices that file into its own
   document, the splice is new text of its own — and when the socket comes
   back the room already holds the same words as the other person's items,
   so the merge keeps both. That is how a meeting note doubled itself. */

const PROJECT = "zz-offline-merge";
let projectId = "";

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

declare global {
  interface Window {
    __sockets: WebSocket[];
    __blockWs: boolean;
  }
}

// Every socket the page opens is kept, and while __blockWs is set a new one
// is pointed somewhere nothing listens — a network that refuses only the
// co-editing socket, leaving the change stream (SSE) up.
async function holdSockets(page: Page) {
  await page.addInitScript(() => {
    window.__sockets = [];
    window.__blockWs = false;
    const Orig = window.WebSocket;
    window.WebSocket = new Proxy(Orig, {
      construct(target, args: ConstructorParameters<typeof WebSocket>) {
        const s = window.__blockWs ? new target("ws://127.0.0.1:9/") : new target(...args);
        window.__sockets.push(s);
        return s;
      },
    });
  });
}

const open = (page: Page) =>
  page.evaluate(() => window.__sockets.filter((s) => s.readyState === WebSocket.OPEN).length);

test("a co-editor that was offline does not duplicate what others typed", async ({ browser }) => {
  test.setTimeout(90_000);
  const a = await (await browser.newContext()).newPage();
  await holdSockets(a);
  await login(a, ADMIN);
  const r = await a.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  const id = projectId;
  expect((await a.request.put(`/api/p/${id}/permissions`, { data: { default: "write" } })).ok()).toBeTruthy();
  const file = `offline-${Date.now()}.md`;
  await a.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`, { data: "BASE\n" });

  const b = await (await browser.newContext()).newPage();
  await login(b, MEMBER);

  for (const pg of [a, b]) {
    await pg.goto(`/${id}/edit/${file}`);
    await pg.waitForSelector(".cm-host .cm-content");
  }
  await expect.poll(() => open(a)).toBe(1);

  // A's co-editing socket goes, and stays gone.
  await a.evaluate(() => {
    window.__blockWs = true;
    window.__sockets.at(-1)!.close();
  });
  await expect(a.locator("#ed-offline")).toBeVisible({ timeout: 10_000 });

  // B types; the hub snapshots it and A hears about the file changing.
  await b.locator(".cm-host .cm-line").first().click();
  await b.keyboard.press("End");
  await b.keyboard.type(" hello world");
  await b.waitForTimeout(5_000);

  // A comes back.
  await a.evaluate(() => {
    window.__blockWs = false;
  });
  await expect.poll(() => open(a), { timeout: 15_000 }).toBe(1);
  await a.waitForTimeout(4_000);

  const text = await (await a.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)).text();
  expect(text).toBe("BASE hello world\n");
  for (const pg of [a, b]) {
    const shown = await pg.locator(".cm-host .cm-content").innerText();
    expect(shown.split("hello world").length - 1).toBe(1);
  }
});
