import { expect, test } from "@playwright/test";
import { login, wikiId } from "./helpers";

// The sidebar: Home · Activity · Insights · Settings nav with canonical URLs,
// tree filter mode, conflict copies folded into a badge, the collapsed icon
// rail, and per-browser pins.

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
  // Installation left the nav for the footer action.
  await expect(page.locator("#projects .nav-menu .row")).toHaveCount(4);
  await page.click("#nav-install");
  await page.waitForURL(`/${pid}/install`);

  await page.goto(`/${pid}/history?q=guide`);
  await page.waitForURL(`/${pid}/activity?q=guide`);
  await page.goto(`/${pid}/dashboard/notes`);
  await page.waitForURL(`/${pid}/insights/notes`);
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
  expect(box!.width).toBe(56);
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
