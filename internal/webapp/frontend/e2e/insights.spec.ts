import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

// The seeded wiki: archive/retired-spec.md has 14 agent reads and is 210 days
// old — hot and stale under the default thresholds (≥ 10 reads, 30 days).

test.beforeEach(async ({ page }) => {
  await page.addInitScript(() => localStorage.removeItem("bd.insights.thresholds.v2"));
});

test("the KPI row splits reads by reader and flags with icon + label", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.locator(".in-kpi")).toHaveCount(4);
  const reads = page.locator('.in-kpi[data-kpi="reads"]');
  for (const k of ["people", "agents", "share"]) await expect(reads.locator(".in-kpi-n")).toContainText(k);
  await expect(page.locator('.in-kpi[data-kpi="coverage"] .in-kpi-v')).toContainText("%");
  const stale = page.locator('.in-kpi[data-kpi="stale"]');
  expect(Number((await stale.locator(".in-kpi-v").textContent())!.match(/\d+/)![0])).toBeGreaterThan(0);
  await expect(stale.locator(".in-status")).toHaveText("⚠ Review");
  await expect(page.locator('.in-kpi[data-kpi="empty"]')).toBeVisible();
});

test("the threshold control drives the fix-first table and the scatter together", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  const row = page.locator(".in-fix-row", { hasText: "retired-spec.md" });
  await expect(row).toHaveCount(1);
  await expect(row.locator(".in-fx-why")).toHaveText("Only agents read it");
  await expect(row.getByRole("button", { name: "Review" })).toBeVisible();

  const line = page.locator('svg.in-scatter line[data-th="stale"]');
  const x0 = Number(await line.getAttribute("x1"));
  await expect(page.locator("svg.in-scatter .in-pt.danger")).not.toHaveCount(0);

  await page.getByLabel("Stale after days").fill("365");
  await expect(row).toHaveCount(0);
  await expect(page.locator("svg.in-scatter .in-pt.danger")).toHaveCount(0);
  await expect(page.locator("svg.in-scatter .in-quad-count")).toHaveText("· 0");
  expect(Number(await line.getAttribute("x1"))).toBeGreaterThan(x0);
  await expect(page.locator('.in-kpi[data-kpi="stale"] .in-kpi-n')).toContainText("365+ days");

  await page.getByLabel("Stale after days").fill("30");
  await page.getByLabel("Hot at reads").fill("100");
  await expect(row).toHaveCount(0);
  await page.getByLabel("Hot at reads").fill("3");
  await expect(row).toHaveCount(1);
  await row.getByRole("button", { name: "Review" }).click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/archive/retired-spec\\.md$`));
});

test("an emptied file that is still read is a Restore row", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "emptied" } })).json();
  const pid = made.project.id;
  try {
    await page.request.put(`/api/p/${pid}/upload/content?path=notes/full.md`, { data: "# Full\n" });
    // Emptied: had content, now 0 B. Always-empty: 0 B from the start.
    await page.request.put(`/api/p/${pid}/upload/content?path=notes/daily.md`, { data: "standup notes\n" });
    await page.request.put(`/api/p/${pid}/upload/content?path=notes/daily.md`, { data: "" });
    await page.request.put(`/api/p/${pid}/upload/content?path=notes/robots.txt`, { data: "" });
    await page.request.post(`/api/p/${pid}/reads`, {
      headers: { "X-Bdrive-Device": "insights-e2e-agent" },
      data: { reads: [{ path: "notes/daily.md" }, { path: "notes/robots.txt" }] },
    });
    await page.goto(`/${pid}/insights`);
    const row = page.locator('.in-fix-row[data-kind="empty"]', { hasText: "daily.md" });
    await expect(row).toHaveCount(1);
    await expect(row.locator(".in-flag-empty")).toHaveAttribute("aria-label", "emptied");
    await expect(row.locator(".in-fx-when")).toContainText("→ 0 B");
    await expect(page.locator('.in-kpi[data-kpi="empty"] .in-status')).toHaveText("⛔ Restore");
    await expect(page.locator(".in-tm-cell.flag-empty")).toHaveCount(1);
    // A file that was never anything but empty is not "now empty".
    await expect(page.locator('.in-fix-row[data-kind="empty"]', { hasText: "robots.txt" })).toHaveCount(0);
    await expect(page.locator('.in-kpi[data-kpi="empty"] .in-kpi-v, .in-kpi[data-kpi="empty"]').first()).toContainText("1");
    await row.getByRole("button", { name: "Restore" }).click();
    await expect(page).toHaveURL(new RegExp(`/${pid}/activity/notes/daily\\.md$`));
  } finally {
    await page.request.delete("/api/projects/" + pid);
  }
});

test("the treemap starts at folder level and drills into a folder's files", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  const svg = page.locator("svg.in-treemap");
  await expect(svg).toHaveAttribute("data-level", "folder");
  await expect(svg.locator(".in-tm-cell[data-path]")).toHaveCount(0);
  const archive = svg.locator('.in-tm-cell[data-dir="archive"]');
  await expect(archive).toHaveClass(/flag-stale/);
  await archive.click();
  await expect(svg).toHaveAttribute("data-level", "file");
  const paths = await svg.locator(".in-tm-cell[data-path]").evaluateAll((els) =>
    els.map((e) => e.getAttribute("data-path")!),
  );
  expect(paths.length).toBeGreaterThan(0);
  for (const p of paths) expect(p.startsWith("archive/")).toBe(true);
  await expect(svg.locator('.in-tm-cell[data-path="archive/retired-spec.md"]')).toHaveClass(/flag-stale/);
  await page.locator(".in-tm-crumbs button", { hasText: "All folders" }).click();
  await expect(svg).toHaveAttribute("data-level", "folder");
});

test("every chart has a View as table toggle", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  for (const sec of ["treemap", "hot-agent", "hot-human", "scatter", "coverage"]) {
    const s = page.locator(`[data-sec="${sec}"]`);
    await expect(s.locator(".in-data-table")).toHaveCount(0);
    await s.getByRole("button", { name: "View as table" }).click();
    await expect(s.locator(".in-data-table")).toBeVisible();
    await expect(s.locator("svg, .in-hotpath, .in-matrix")).toHaveCount(0);
    await s.getByRole("button", { name: "View as chart" }).click();
    await expect(s.locator(".in-data-table")).toHaveCount(0);
  }
  const scatter = page.locator('[data-sec="scatter"]');
  await scatter.getByRole("button", { name: "View as table" }).click();
  await expect(scatter.locator("tr", { hasText: "archive/retired-spec.md" })).toContainText("⚠ hot + stale");
});

test("the lens applies to every section", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.locator('[data-sec="hot-agent"]')).toHaveCount(1);
  await expect(page.locator('[data-sec="hot-human"]')).toHaveCount(1);
  const sub = page.locator('[data-sec="scatter"] .in-sub');
  const allSub = await sub.textContent();

  await page.getByRole("button", { name: "People", exact: true }).click();
  const reads = page.locator('.in-kpi[data-kpi="reads"]');
  await expect(reads.locator('[data-kind="agent"]')).toContainText("0 agents");
  await expect(page.locator('[data-sec="hot-agent"]')).toHaveCount(0);
  await expect(page.locator('[data-sec="hot-human"]')).toHaveCount(1);
  await expect(page.locator(".in-fix-row", { hasText: "retired-spec.md" })).toHaveCount(0); // agent-only reads
  await expect(sub).not.toHaveText(allSub!);
  await expect(page.locator(".in-cov-off")).toBeVisible();
  // The table view is held to the same lens: no agent matrix behind it.
  const cov = page.locator('[data-sec="coverage"]');
  await cov.locator(".in-astable").click();
  await expect(cov.locator(".in-table-wrap .in-cov-off")).toBeVisible();
  await expect(cov.locator(".in-table-wrap table")).toHaveCount(0);
  await cov.locator(".in-astable").click();

  await page.getByRole("button", { name: "Agents", exact: true }).click();
  await expect(reads.locator('[data-kind="human"]')).toContainText("0 people");
  await expect(page.locator('[data-sec="hot-human"]')).toHaveCount(0);
  await expect(page.locator(".in-fix-row", { hasText: "retired-spec.md" })).toHaveCount(1);
  await expect(page.locator(".in-matrix")).toBeVisible();
});

async function vizTokens(page: Page) {
  return page.evaluate(() => {
    const cs = getComputedStyle(document.documentElement);
    const v = (n: string) => cs.getPropertyValue(n).trim().toLowerCase();
    return {
      agent: v("--viz-agent"),
      human: v("--viz-human"),
      share: v("--viz-share"),
      fresh: [1, 2, 3, 4].map((i) => v("--viz-fresh-" + i)),
      cov: [1, 2, 3, 4].map((i) => v("--viz-cov-" + i)),
      kpiAgent: getComputedStyle(document.querySelector(".in-sw.agent")!).backgroundColor,
    };
  });
}

test("both themes carry the validated chart palette", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.locator(".in-kpis")).toBeVisible();
  expect(await vizTokens(page)).toEqual({
    agent: "#c98500",
    human: "#5b8def",
    share: "#d55181",
    fresh: ["#4cc38a", "#3d9a6c", "#327656", "#285a44"],
    cov: ["#5e420a", "#7d5507", "#a06d05", "#c98500"],
    kpiAgent: "rgb(201, 133, 0)",
  });
  await page.emulateMedia({ colorScheme: "light" });
  await expect(page.locator("html")).toHaveAttribute("data-theme", "light");
  expect(await vizTokens(page)).toEqual({
    agent: "#c98500",
    human: "#2a78d6",
    share: "#d55181",
    fresh: ["#3daa78", "#2e8a5f", "#23704c", "#1b573b"],
    cov: ["#f0ddb0", "#e3bf6e", "#d5a132", "#c98500"],
    kpiAgent: "rgb(201, 133, 0)",
  });
  // No chart colour is a literal outside the tokens.
  const share = await page.locator(".in-sw.share").first().evaluate((e) => getComputedStyle(e).backgroundColor);
  expect(share).toBe("rgb(213, 81, 129)");
});

test("a fresh browser starts at hot ≥ 10, and an old default save does not pin it", async ({ page }) => {
  // Re-seeded on every navigation: beforeEach clears the v2 key each time.
  await page.addInitScript(() =>
    localStorage.setItem("bd.insights.thresholds", JSON.stringify({ hot: 3, stale: 30 })),
  );
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.getByLabel("Hot at reads")).toHaveValue("10");
  await expect(page.getByLabel("Stale after days")).toHaveValue("30");
  expect(await page.evaluate(() => localStorage.getItem("bd.insights.thresholds"))).toBeNull();
});

test("a customised pre-v2 threshold save carries over", async ({ page }) => {
  // Re-seeded on every navigation: beforeEach clears the v2 key each time.
  await page.addInitScript(() =>
    localStorage.setItem("bd.insights.thresholds", JSON.stringify({ hot: 5, stale: 60 })),
  );
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.getByLabel("Hot at reads")).toHaveValue("5");
  await expect(page.getByLabel("Stale after days")).toHaveValue("60");
});

test("the treemap outlines at most 8 hot + stale cells, even when everything qualifies", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await page.getByLabel("Stale after days").fill("1");
  await page.getByLabel("Hot at reads").fill("1");
  const cells = page.locator("svg.in-treemap .in-tm-cell");
  await expect(cells.first()).toBeVisible();
  await expect(page.locator("svg.in-treemap .in-tm-cell.flag-stale").first()).toBeVisible();
  expect(await page.locator("svg.in-treemap .in-tm-cell.flag-stale").count()).toBeLessThanOrEqual(8);
  // At file level too.
  await page.locator("svg.in-treemap [data-dir]").first().click();
  await expect(page.locator("svg.in-treemap")).toHaveAttribute("data-level", "file");
  expect(await page.locator("svg.in-treemap .in-tm-cell.flag-stale").count()).toBeLessThanOrEqual(8);
});

for (const width of [1440, 1200]) {
  test(`the Reads KPI legend stays on one line at ${width}`, async ({ page }) => {
    await page.setViewportSize({ width, height: 900 });
    await login(page);
    const pid = await wikiId(page);
    await page.goto(`/${pid}/insights`);
    const items = page.locator('.in-kpi[data-kpi="reads"] .in-kpi-n > span');
    await expect(items).toHaveCount(3);
    const boxes = await items.evaluateAll((els) => els.map((e) => e.getBoundingClientRect()));
    for (const b of boxes) expect(Math.abs(b.top - boxes[0].top)).toBeLessThan(2);
    const kpi = (await page.locator('.in-kpi[data-kpi="reads"]').boundingBox())!;
    expect(boxes[2].right).toBeLessThanOrEqual(kpi.x + kpi.width);
  });
}

test("on a phone the Reads legend wraps inside its card", async ({ page }) => {
  await page.setViewportSize({ width: 390, height: 844 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  const kpi = (await page.locator('.in-kpi[data-kpi="reads"]').boundingBox())!;
  const boxes = await page
    .locator('.in-kpi[data-kpi="reads"] .in-kpi-n > span')
    .evaluateAll((els) => els.map((e) => e.getBoundingClientRect()));
  for (const b of boxes) expect(b.right).toBeLessThanOrEqual(kpi.x + kpi.width + 0.5);
});
