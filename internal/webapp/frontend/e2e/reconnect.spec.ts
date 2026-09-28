import { test, expect } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* A browser whose co-editing socket drops and comes back.

   The hub evicts a room the moment its last peer leaves, and the browser
   that left still holds the document — a laptop lid, a proxy dropping an
   idle socket. y-websocket reconnects it into whatever room exists then. If
   that room were a fresh insert of the file's bytes, the browser's copy and
   the hub's copy would be two documents spelling the same text, and the
   merge keeps both: the whole file, twice, once more per reconnect. A real
   user's daily-meeting notes came back five times over.

   So the hub rebuilds a room from the state the last one stored, and this
   drives the actual reconnect through the real client to prove the merge is
   a no-op: type, kill the socket, let it come back, type again, and the file
   says everything exactly once. */

const PROJECT = "zz-reconnect";
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
  }
}

test("a reconnected editor does not duplicate the file", async ({ page }) => {
  test.setTimeout(90_000);
  // Keep a handle on every socket the page opens, so the collab one can be
  // killed from outside the way a network would.
  await page.addInitScript(() => {
    window.__sockets = [];
    const Orig = window.WebSocket;
    window.WebSocket = new Proxy(Orig, {
      construct(target, args: ConstructorParameters<typeof WebSocket>) {
        const s = new target(...args);
        window.__sockets.push(s);
        return s;
      },
    });
  });
  await login(page, ADMIN);
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  const id = projectId;
  const file = `reconnect-${Date.now()}.md`;
  await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");

  const sockets = () => page.evaluate(() => window.__sockets.length);
  const live = () =>
    page.evaluate(() => window.__sockets.filter((s) => s.readyState === WebSocket.OPEN).length);
  await expect.poll(live).toBe(1);

  for (let round = 1; round <= 3; round++) {
    await page.keyboard.type(` r${round}`);
    // Let the hub's idle snapshot land before the socket goes: that is the
    // state the rebuilt room must be reproduced from.
    await page.waitForTimeout(3_000);

    const before = await sockets();
    await page.evaluate(() => window.__sockets.at(-1)!.close());
    await expect.poll(sockets, { timeout: 15_000 }).toBeGreaterThan(before);
    await expect.poll(live, { timeout: 15_000 }).toBe(1);
    await page.waitForTimeout(1_000);

    const shown = await page.locator(".cm-host .cm-content").innerText();
    expect(shown.split("BASE").length - 1, `round ${round}: editor shows BASE ${shown.split("BASE").length - 1} times`).toBe(1);
  }

  await page.keyboard.type(" end");
  await page.waitForTimeout(3_000);
  const text = await (
    await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)
  ).text();
  expect(text).toBe("BASE r1 r2 r3 end\n");
});
