import { test, expect, type Page } from "@playwright/test";
import { login, ADMIN } from "./helpers";

/* The meeting-note editor: the safety states (blocking empty-save dialog,
   pending edits while the socket is down, one-click restore), the modes on
   one room, the rail's action items, the / menu and section folding. */

const PROJECT = "zz-meeting-editor";
let projectId = "";

declare global {
  interface Window {
    __sockets: WebSocket[];
  }
}

test.beforeAll(async ({ browser }) => {
  const page = await browser.newPage();
  await login(page, ADMIN);
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  await page.close();
});

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

const put = (page: Page, file: string, body: string) =>
  page.request.put(`/api/p/${projectId}/upload/content?path=${encodeURIComponent(file)}`, { data: body });
const read = async (page: Page, file: string) =>
  (await page.request.get(`/api/p/${projectId}/file?path=${encodeURIComponent(file)}`)).text();

async function open(page: Page, file: string) {
  // Wide enough for the rail to sit beside the text rather than fold up.
  await page.setViewportSize({ width: 1600, height: 1000 });
  await page.goto(`/${projectId}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "live", { timeout: 15_000 });
}

const editorText = (page: Page) =>
  page.evaluate(() => {
    const lines = [...document.querySelectorAll(".cm-host .cm-line")].map((l) => (l as HTMLElement).innerText);
    return lines.join("\n");
  });

test("emptying a live room asks first, and Keep puts the saved text back", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `shrink-${Date.now()}.md`;
  const body = "## Snow\n" + "- shipped the thing\n".repeat(40);
  expect((await put(page, file, body)).ok()).toBeTruthy();
  await open(page, file);

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Delete");

  const dialog = page.locator("#shrink-dialog");
  await expect(dialog).toBeVisible({ timeout: 10_000 });
  await expect(dialog).toContainText("This save would erase the note");
  // The default answer is focused.
  await expect(page.locator("#shrink-keep")).toBeFocused();
  await page.locator("#shrink-keep").click();
  await expect(dialog).toBeHidden();
  await expect.poll(() => editorText(page)).toContain("shipped the thing");
  await page.waitForTimeout(2_500);
  expect(await read(page, file)).toBe(body);

  // Save anyway writes what the editor holds when it is clicked.
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type("short");
  await expect(dialog).toBeVisible({ timeout: 10_000 });
  await page.locator("#shrink-confirm").click();
  await expect(dialog).toBeHidden();
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("short");
});

test("Keep in a live room leaves the editor saved, and later typing still saves", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `keep-state-${Date.now()}.md`;
  const body = "## Snow\n" + "- shipped the thing\n".repeat(40);
  expect((await put(page, file, body)).ok()).toBeTruthy();
  await open(page, file);
  const state = page.locator("#editor-state");

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.press("Delete");
  await expect(page.locator("#shrink-dialog")).toBeVisible({ timeout: 10_000 });
  await page.locator("#shrink-keep").click();
  await expect(page.locator("#shrink-dialog")).toBeHidden();
  await expect.poll(() => editorText(page)).toContain("shipped the thing");
  await expect(state).toHaveAttribute("data-state", "clean");
  await page.waitForTimeout(3_000);
  await expect(state).toHaveAttribute("data-state", "clean");
  expect(await read(page, file)).toBe(body);

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" today");
  await expect(state).toHaveAttribute("data-state", "dirty");
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe(body.replace("## Snow", "## Snow today"));
  await expect(state).toHaveAttribute("data-state", "clean", { timeout: 10_000 });
});

test("Save anyway in a live room, then typing, writes both — no conflict copy", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `confirm-then-type-${Date.now()}.md`;
  const body = "## Snow\n" + "- shipped the thing\n".repeat(40);
  expect((await put(page, file, body)).ok()).toBeTruthy();
  await open(page, file);

  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+a");
  await page.keyboard.type("short");
  await expect(page.locator("#shrink-dialog")).toBeVisible({ timeout: 10_000 });
  await page.locator("#shrink-confirm").click();
  await expect(page.locator("#shrink-dialog")).toBeHidden();
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("short");

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" and more");
  await expect.poll(() => read(page, file), { timeout: 15_000 }).toBe("short and more");
  const tree = await (await page.request.get(`/api/p/${projectId}/tree?slim=1`)).text();
  expect(tree).not.toContain(file + ".bdrive-conflict-");
});

test("edits made while the socket is down are marked pending, then land", async ({ page }) => {
  test.setTimeout(60_000);
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
  const file = `pending-${Date.now()}.md`;
  await put(page, file, "BASE\n");
  await open(page, file);
  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");

  // Refuse every reconnect, then drop the live socket.
  await page.routeWebSocket(/ycollab/, (ws) => ws.close());
  await page.evaluate(() => window.__sockets.filter((s) => s.url.includes("ycollab")).forEach((s) => s.close()));
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "offline", { timeout: 10_000 });

  await page.keyboard.type(" queued");
  await expect(page.locator("#editor-state")).not.toHaveAttribute("data-pending", "0");
  await expect(page.locator("#editor-state")).toContainText("pending");
  await expect(page.locator("#ed-offline")).toContainText("kept in this tab");
  expect(await read(page, file)).toBe("BASE\n");

  await page.unrouteAll({ behavior: "ignoreErrors" });
  await expect(page.locator("#editor-state")).toHaveAttribute("data-collab", "live", { timeout: 20_000 });
  await expect(page.locator("#editor-state")).toHaveAttribute("data-pending", "0");
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("BASE queued\n");
});

test("a past version is restored in one click, as a new version", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `restore-${Date.now()}.md`;
  await put(page, file, "first version\n");
  await page.waitForTimeout(1_100);
  await put(page, file, "second version\n");
  await open(page, file);

  const rows = page.locator(".er-vlist li[data-sha]");
  await expect(rows).toHaveCount(2);
  await rows.nth(1).locator(".er-restore").click();
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("first version\n");
  await expect.poll(() => editorText(page)).toContain("first version");
  const h = await (await page.request.get(`/api/p/${projectId}/history?path=${encodeURIComponent(file)}`)).json();
  expect(h.entries.length).toBe(3);
});

test("a restore reaches the open document without the change stream", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  // The editor must not depend on hearing about its own restore.
  await page.route(/\/events(\?|$)/, (route) => route.abort());
  const file = `restore-quiet-${Date.now()}.md`;
  await put(page, file, "first version\n");
  await page.waitForTimeout(1_100);
  await put(page, file, "second version\n");
  await open(page, file);

  const rows = page.locator(".er-vlist li[data-sha]");
  await expect(rows).toHaveCount(2);
  await rows.nth(1).locator(".er-restore").click();
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("first version\n");
  await expect.poll(async () => (await editorText(page)).trim()).toBe("first version");

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" and more");
  await expect.poll(() => read(page, file), { timeout: 15_000 }).toBe("first version and more\n");
  expect((await editorText(page)).trim()).toBe("first version and more");
});

test("switching modes keeps one room and never duplicates the text", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `modes-${Date.now()}.md`;
  await put(page, file, "BASE\n");
  await open(page, file);
  await expect(page.locator("#mode-write")).toHaveAttribute("aria-selected", "true");
  await expect(page.locator(".cm-host .cm-gutters")).toHaveCount(0);

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" one");
  await page.locator("#mode-markdown").click();
  await expect(page.locator(".cm-host .cm-gutters")).toBeVisible();
  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" two");
  await page.waitForTimeout(2_500);
  await page.locator("#mode-preview").click();
  await expect(page.locator("#ed-preview")).toContainText("BASE one two");
  await page.locator("#mode-write").click();
  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" three");
  await expect(page).toHaveURL(new RegExp(`/${projectId}/edit/${file}$`));
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toBe("BASE one two three\n");
});

test("the rail lists action items with their owners", async ({ page }) => {
  await login(page, ADMIN);
  const file = `items-${Date.now()}.md`;
  await put(page, file, "## Snow\n- [ ] Review triage @Mino\n- [x] Cost dashboard @Isac\n- plain\n");
  await open(page, file);
  const items = page.locator(".er-list li");
  await expect(items).toHaveCount(2);
  await expect(items.nth(0)).toHaveAttribute("data-owner", "Mino");
  await expect(items.nth(0)).toContainText("Review triage");
  await expect(items.nth(1)).toHaveClass(/done/);
  await expect(page.locator(".er-items h4")).toContainText("Action items · 1");
});

test("the / menu inserts an action item and carries over open items", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const dir = `team/daily-meetings/2026-10/`;
  await put(page, dir + "2026-10-14.md", "## Snow\n- [ ] older @x\n");
  await put(page, dir + "2026-10-15.md", "## Snow\n- [ ] follow up @Mino\n- [x] finished @Bobb\n");
  const file = dir + "2026-10-16.md";
  await put(page, file, "## e2e\n\n");
  await open(page, file);

  await page.locator(".cm-host .cm-line").nth(1).click();
  await page.keyboard.type("/");
  const menu = page.locator(".cm-tooltip-autocomplete");
  await expect(menu).toContainText("Action item");
  await expect(menu).toContainText("Carry over from last meeting");
  await page.keyboard.type("action");
  await expect(menu.locator("li[aria-selected]")).toContainText("Action item");
  // CodeMirror ignores Enter for 75ms after the list changes (interactionDelay).
  await page.waitForTimeout(200);
  await page.keyboard.press("Enter");
  await page.keyboard.type("write notes");
  await expect.poll(() => editorText(page)).toMatch(/- \[ \] write notes @E2E/);

  await page.keyboard.press("End");
  await page.keyboard.press("Enter");
  await page.keyboard.type("/carry");
  await expect(menu.locator("li[aria-selected]")).toContainText("Carry over");
  await page.waitForTimeout(200);
  await page.keyboard.press("Enter");
  await expect.poll(() => editorText(page)).toContain("- [ ] follow up @Mino");
  const text = await editorText(page);
  expect(text).not.toContain("finished");
  expect(text).not.toContain("older");
  await expect.poll(() => read(page, file), { timeout: 10_000 }).toContain("- [ ] follow up @Mino");
});

test("collapsing a teammate's section never edits the text", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const file = `team/daily-meetings/2026-10/fold-${Date.now()}.md`;
  const body = "## e2e\n- my update\n\n## Snow\n- Konny - Briana\n- Outreach report\n\n## Bale\n";
  await put(page, file, body);
  await open(page, file);

  // Someone else's section starts collapsed to a summary; yours is open.
  const summary = page.locator(".ms-summary");
  await expect(summary).toHaveCount(1);
  await expect(summary).toContainText("Konny - Briana · Outreach report");
  await expect(page.locator(".cm-host")).toContainText("my update");
  await expect(page.locator('.ms-status[data-status="empty"]')).toContainText("Not started");

  await page.locator('.ms-toggle[data-section="Snow"]').click();
  await expect(summary).toHaveCount(0);
  await expect(page.locator(".cm-host")).toContainText("Outreach report");
  await page.locator('.ms-toggle[data-section="Snow"]').click();
  await expect(summary).toHaveCount(1);
  await page.locator('.ms-toggle[data-section="e2e"]').click();
  await page.waitForTimeout(2_500);
  expect(await read(page, file)).toBe(body);
  await expect(page.locator("#editor-state")).toHaveAttribute("data-state", "clean");
});
