import { expect, test } from "@playwright/test";
import { login, wikiId } from "./helpers";

// The sidebar: Home · Activity · Insights nav with canonical URLs (Settings
// lives in the project menu), tree filter mode, conflict copies folded into a
// badge, the collapsed icon rail, and per-browser pins.

const COPY = "archive/old-runbook.md.bdrive-conflict-mira-laptop-20260814T060945Z";

test("nav routes are canonical and the old view URLs redirect to them", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.click("#nav-activity");
  await page.waitForURL(`/${pid}/activity`);
  await expect(page).toHaveTitle("Activity — wiki");
  await expect(page.locator("#nav-activity")).toHaveClass(/active/);
  await page.click("#nav-insights");
  await page.waitForURL(`/${pid}/insights`);
  await expect(page.locator("#crumb")).toHaveText("Insights — wiki");
  await page.click("#nav-home");
  await page.waitForURL(`/${pid}`);
  await expect(page.locator("#nav-home")).toHaveClass(/active/);
  // Settings is in the project menu; installation is the footer's sync row.
  await expect(page.locator("#projects .nav-menu .row")).toHaveCount(3);
  await page.click("#nav-install");
  await page.waitForURL(`/${pid}/install`);

  await page.goto(`/${pid}/history?q=guide`);
  await page.waitForURL(`/${pid}/activity?q=guide`);
  await page.goto(`/${pid}/dashboard/notes`);
  await page.waitForURL(`/${pid}/insights/notes`);
});

test("clicking the empty search field opens the palette", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.click("#sb-search-input");
  await expect(page.locator("#palette")).toBeVisible();
  await expect(page.locator("#palette input")).toBeFocused();
  await page.keyboard.press("Escape");
  await expect(page.locator("#palette")).toHaveCount(0);
  // A right click is the browser's, not the palette's.
  await page.click("#sb-search-input", { button: "right" });
  await expect(page.locator("#palette")).toHaveCount(0);
});

test("typing in the search field filters the tree; esc restores it", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.waitForSelector('#tree .row[data-path="guide.md"]');
  const input = page.locator("#sb-search-input");
  await input.fill("guide");
  await expect(page.locator("#tree")).toHaveCount(0);
  const hits = page.locator("#tree-filter .fhit[data-path]");
  await expect(hits.first()).toHaveAttribute("data-path", "guide.md");
  await expect(hits.first()).toHaveClass(/sel/);
  await expect(hits.first().locator("mark")).toHaveText("guide");
  await expect(page.locator('#tree-filter .fhit[data-path="archive/moved-guide.md"] .fhit-path')).toHaveText(
    "archive/",
  );
  await expect(page.locator("#tree-filter .fgroup").first()).toContainText(/Files · \d+/);
  await expect(page.locator("#tree-filter .fcontents")).toContainText("Search contents for “guide”");

  // ↓ moves the selection, ↵ opens it.
  await input.press("ArrowDown");
  await expect(hits.nth(1)).toHaveClass(/sel/);
  await input.press("ArrowUp");
  await input.press("Enter");
  await page.waitForURL(`/${pid}/guide.md`);

  // ⌘↵ hands the query to the palette.
  await input.press("ControlOrMeta+Enter");
  await expect(page.locator("#palette")).toBeVisible();
  await expect(page.locator("#palette input")).toHaveValue("guide");
  await page.keyboard.press("Escape");

  await input.press("Escape");
  await expect(input).toHaveValue("");
  await expect(page.locator('#tree .row[data-path="guide.md"]')).toBeVisible();
});

test("a conflict copy is a badge on its original, not a sibling row", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/archive`);
  const original = page.locator('#tree .row[data-path="archive/old-runbook.md"]');
  await expect(original).toBeVisible();
  await expect(original.locator(".tconflicts")).toHaveText("· 1 conflict");
  await expect(page.locator(`#tree .row[data-path="${COPY}"]`)).toHaveCount(0);
  // Still reachable by URL, where the banner explains it.
  await page.goto(`/${pid}/${COPY}`);
  await expect(page.locator(".cbanner, .vbanner").first()).toBeVisible();
});

test("⌘\\ collapses the sidebar to an icon rail and back", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.evaluate(() => localStorage.removeItem("bdrive.sbCollapsed"));
  await page.locator("#content").click();
  await page.keyboard.press("ControlOrMeta+\\");
  await expect(page.locator("body")).toHaveClass(/sb-collapsed/);
  const box = await page.locator("#sidebar").boundingBox();
  expect(box!.width).toBe(48);
  await expect(page.locator("#rail")).toBeVisible();
  await expect(page.locator("#tree")).toBeHidden();
  await page.locator("#rail-activity").hover();
  await expect(page.locator(".tipcard", { hasText: "Activity" })).toBeVisible();
  await page.click("#rail-insights");
  await page.waitForURL(`/${pid}/insights`);
  await page.keyboard.press("ControlOrMeta+\\");
  await expect(page.locator("body")).not.toHaveClass(/sb-collapsed/);
  await expect(page.locator("#tree")).toBeVisible();
  // ⌘B still toggles too.
  await page.keyboard.press("ControlOrMeta+b");
  await expect(page.locator("body")).toHaveClass(/sb-collapsed/);
  await page.keyboard.press("ControlOrMeta+b");
  await expect(page.locator("body")).not.toHaveClass(/sb-collapsed/);
});

test("pins persist per project across reloads and show in the rail", async ({ page }) => {
  await page.setViewportSize({ width: 1280, height: 800 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.evaluate((id) => localStorage.removeItem("bdrive.pins." + id), pid);
  await page.reload();
  const row = page.locator('#tree .row[data-path="archive/old-runbook.md"]');
  await page.locator('#tree .row[data-path="archive"] .chev').click();
  await row.click({ button: "right" });
  await page.getByRole("menuitem", { name: "Pin to sidebar" }).click();
  const pin = page.locator('#pinned .row[data-pin="archive/old-runbook.md"]');
  await expect(pin).toBeVisible();
  await expect(pin.locator(".pin-badge.danger")).toHaveText("1 conflict");

  await page.reload();
  await expect(pin).toBeVisible();
  await pin.click();
  await page.waitForURL(`/${pid}/archive/old-runbook.md`);

  await page.keyboard.press("ControlOrMeta+\\");
  await expect(page.locator('#rail .rail-btn[aria-label="old-runbook.md"] .rail-badge')).toHaveText("1");
  await page.keyboard.press("ControlOrMeta+\\");

  await pin.hover();
  await pin.locator(".pin-x").click();
  await expect(pin).toHaveCount(0);
  await page.reload();
  await expect(page.locator("#pinned")).toHaveCount(0);
});

test("New file is offered only where the hub would accept the write", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // A rule making archive/ read-only for this account; everything else stays
  // writable.
  await page.route(`**/api/p/${pid}/folders`, (route) =>
    route.fulfill({
      json: {
        folders: [{ prefix: "archive/", default: "read", grants: [], me: "read" }],
        scope: "",
      },
    }),
  );
  await page.goto(`/${pid}`);
  const newIn = (dir: string) => page.locator(`#tree .row[data-path="${dir}"] .tact[title="New file"]`);
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  await expect(newIn("notes")).toHaveCount(1);
  await expect(newIn("archive")).toHaveCount(0);

  // A hub with uploads off accepts no new file anywhere.
  await page.route("**/api/config", async (route) => {
    const res = await route.fetch();
    const cfg = await res.json();
    await route.fulfill({ response: res, json: { ...cfg, upload: { ...cfg.upload, enabled: false } } });
  });
  await page.reload();
  await expect(page.locator('#tree .row[data-path="notes"]')).toBeVisible();
  await expect(page.locator('#tree .tact[title="New file"]')).toHaveCount(0);
});

test("Files header: sort toggles name ↔ recent and persists", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await page.evaluate(() => localStorage.removeItem("bdrive.treeOrder"));
  await page.reload();

  const top = () =>
    page.locator("#tree .row[data-path]").evaluateAll((els) =>
      els
        .filter((e) => !e.getAttribute("data-path")!.includes("/"))
        .sort((a, b) => a.getBoundingClientRect().top - b.getBoundingClientRect().top)
        .map((e) => e.getAttribute("data-path")),
    );
  const byName = await top();
  const sort = page.locator("#tree-sort-btn");
  await expect(sort).toHaveAttribute("aria-pressed", "false");
  await sort.click();
  await expect(sort).toHaveAttribute("aria-pressed", "true");
  await expect.poll(top).not.toEqual(byName);
  const byRecent = await top();
  await page.reload();
  await expect(page.locator("#tree-sort-btn")).toHaveAttribute("aria-pressed", "true");
  await expect.poll(top).toEqual(byRecent);
  await page.click("#tree-sort-btn");
  await expect.poll(top).toEqual(byName);
});

test("active nav is neutral; honey marks only the selected tree file's icon", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/guide.md`);
  await page.click("#nav-activity");
  const nav = page.locator("#nav-activity");
  await expect(nav).toHaveClass(/active/);
  await expect(nav).toHaveCSS("color", "rgb(238, 240, 243)"); // --text
  await expect(nav.locator(".label")).toHaveCSS("font-weight", "500");
  await page.goto(`/${pid}/guide.md`);
  const row = page.locator('#tree .row[data-path="guide.md"]');
  await expect(row).toHaveCSS("color", "rgb(238, 240, 243)"); // --text
  await expect(row.locator(".ticon")).toHaveCSS("color", "rgb(245, 166, 35)"); // --accent-ink

  // The brand header carries no controls of its own: collapsing is the
  // topbar's toggle (or ⌘B), and the account lives in the project menu.
  await expect(page.locator("#vault-name")).toHaveText("BearDrive");
  await expect(page.locator("#vault button")).toHaveCount(0);
});

test("dragging the sidebar edge resizes it, persists, and double-click resets", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  const sidebar = page.locator("#sidebar");
  const width = async () => Math.round((await sidebar.boundingBox())!.width);
  expect(await width()).toBe(256);

  const handle = (await page.locator("#sb-resize").boundingBox())!;
  const y = handle.y + handle.height / 2;
  await page.mouse.move(handle.x + 3, y);
  await page.mouse.down();
  await page.mouse.move(340, y, { steps: 5 });
  await page.mouse.up();
  expect(await width()).toBe(340);

  await page.mouse.move(337, y);
  await page.mouse.down();
  await page.mouse.move(900, y, { steps: 5 });
  await page.mouse.up();
  expect(await width()).toBe(480);

  await page.reload();
  await page.waitForSelector("#sidebar");
  expect(await width()).toBe(480);

  await page.locator("#sb-resize").focus();
  await page.keyboard.press("ArrowLeft");
  expect(await width()).toBe(472);

  await page.locator("#sb-resize").dblclick();
  expect(await width()).toBe(256);
});

test("a wide desktop width never covers the mobile drawer's scrim", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.evaluate(() => localStorage.setItem("bdrive.sidebarWidth", "480"));
  await page.setViewportSize({ width: 390, height: 800 });
  await page.goto(`/${pid}`);
  await page.click("#menu-btn");
  await expect(page.locator("body")).toHaveClass(/sb-open/);
  expect(Math.round((await page.locator("#sidebar").boundingBox())!.width)).toBe(256);
  await page.mouse.click(370, 400);
  await expect(page.locator("body")).not.toHaveClass(/sb-open/);

  await page.setViewportSize({ width: 1280, height: 800 });
  expect(Math.round((await page.locator("#sidebar").boundingBox())!.width)).toBe(480);
});

test("a saved wide sidebar never pushes the editing topbar off a narrow desktop window", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const name = `resize-${Date.now()}.html`;
  const put = await page.request.put(`/api/p/${pid}/upload/content?path=${name}`, {
    data: "<!doctype html><html><body><h1>Resize</h1></body></html>",
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
  expect(put.ok()).toBeTruthy();
  try {
    await page.evaluate(() => localStorage.setItem("bdrive.sidebarWidth", "480"));
    await page.setViewportSize({ width: 901, height: 800 });
    await page.goto(`/${pid}/${name}`);
    await page.click("#edit-btn");
    await page.frameLocator("iframe.htmlview").locator("html[data-bd-ready]").waitFor();
    const more = (await page.locator("#more-btn").boundingBox())!;
    expect(more.x + more.width).toBeLessThanOrEqual(901);
  } finally {
    await page.request.post(`/api/p/${pid}/remove`, { data: { path: name } }).catch(() => {});
  }
});
