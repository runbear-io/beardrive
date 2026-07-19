import { test, expect } from "@playwright/test";
import { login, wikiId, MEMBER } from "./helpers";

// Phase 3: project home (connect guide + embedded insights), the dedicated
// insights route, and the history views. Ports the original parity checks
// from the pre-migration smoke suite.

test("landing is the project home (guide), not an insights redirect", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await expect(page.locator(".guide")).toBeVisible();
  await expect(page.locator("#crumb")).toHaveText("wiki");
});

test("guide: three agent tabs, one active, choice persisted", async ({ page }) => {
  await login(page);
  await page.waitForSelector(".guide");
  await expect(page.locator(".gd-tab")).toHaveText(["Claude Code & Cowork", "Hermes", "Codex"]);
  await expect(page.locator(".gd-tab.active")).toHaveCount(1);
  await page.click('.gd-tab[data-key="codex"]');
  expect(await page.evaluate(() => localStorage.getItem("bdrive-guide-agent"))).toBe("codex");
  await page.click('.gd-tab[data-key="claude"]');
});

test("claude tab: plugin flow with real hub origin and project id, no raw CLI", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('.gd-tab[data-key="claude"]');
  const codes = await page.$$eval(".gd-code code", (els) => els.map((e) => e.textContent).join("\n"));
  expect(codes).toContain("/plugin marketplace add runbear-io/beardrive");
  expect(codes).toContain("/plugin install beardrive@beardrive");
  expect(codes).toContain(`/beardrive:install connect to http://localhost:8993, project ${pid}`);
  expect(codes).not.toContain("brew install");
  expect(codes).not.toContain("hooks install");
  await expect(page.locator(".gd-body")).toContainText("Cowork");
});

test("codex tab keeps the full CLI flow", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click('.gd-tab[data-key="codex"]');
  const codes = await page.$$eval(".gd-code code", (els) => els.map((e) => e.textContent).join("\n"));
  expect(codes).toContain("brew install runbear-io/tap/beardrive");
  expect(codes).toContain("bdrive login http://localhost:8993");
  expect(codes).toContain(`bdrive init --project ${pid}`);
  expect(codes).toContain("bdrive hooks install --agent codex");
  await page.click('.gd-tab[data-key="claude"]');
});

test("a stale saved tab choice falls back to the first tab", async ({ page }) => {
  await login(page);
  await page.waitForSelector(".guide");
  await page.evaluate(() => localStorage.setItem("bdrive-guide-agent", "cowork"));
  await page.reload();
  await page.waitForSelector(".guide");
  await expect(page.locator(".gd-tab.active")).toHaveText("Claude Code & Cowork");
  await page.evaluate(() => localStorage.setItem("bdrive-guide-agent", "claude"));
});

test("admin home embeds insights below the guide; member home does not", async ({ page, browser }) => {
  await login(page);
  await page.waitForSelector(".guide");
  await expect(page.locator(".home-insights .insights")).toBeVisible();
  // Guide renders above the embedded insights
  const order = await page.evaluate(() => {
    const g = document.querySelector(".guide");
    const i = document.querySelector(".home-insights");
    return g && i ? (g.compareDocumentPosition(i) & Node.DOCUMENT_POSITION_FOLLOWING) !== 0 : false;
  });
  expect(order).toBe(true);
  await expect(page.locator(".in-treemap")).toBeVisible();
  await expect(page.locator(".in-hotpath .in-hp-row").first()).toBeVisible();

  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await login(p2, MEMBER);
  await p2.waitForSelector(".guide");
  await expect(p2.locator(".home-insights")).toHaveCount(0);
  await ctx.close();
});

test("dedicated insights route still works and survives reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await expect(page.locator("#crumb")).toHaveText("Insights — wiki");
  await expect(page.locator(".in-treemap")).toBeVisible();
  await page.reload();
  await expect(page.locator(".in-treemap")).toBeVisible();
});

test("hot path row opens the file", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/insights`);
  await page.click(".in-hp-row:first-child");
  await page.waitForURL(/\/(index|guide)\.md$/);
  await expect(page.locator("#content h1")).toBeVisible();
});

test("vault name returns to the project home", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/index.md`);
  await page.click("#vault-name");
  await page.waitForURL("/" + pid);
  await expect(page.locator(".guide")).toBeVisible();
});

test("back/forward walks home → file → insights", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await page.click('#tree .row[data-path="index.md"]');
  await page.waitForURL(`/${pid}/index.md`);
  await page.goto(`/${pid}/insights`);
  await page.goBack();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await page.goBack();
  await expect(page.locator(".guide")).toBeVisible();
  await page.goForward();
  await expect(page.locator("#content h1")).toHaveText("Wiki");
});

test("history: whole project, newest first, and per-file versions", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click("#history-btn"); // from home: whole project
  await page.waitForURL(`/${pid}/history`);
  await expect(page.locator("#crumb")).toContainText("History — all changes");
  await expect(page.locator(".history .hentry").first()).toBeVisible();
  expect(await page.locator(".history .hentry").count()).toBeGreaterThanOrEqual(6); // all seeded ops
  // guide.md has two versions
  await page.goto(`/${pid}/history/guide.md`);
  await expect(page.locator("#crumb")).toContainText("History — guide.md");
  await expect(page.locator(".history .hentry")).toHaveCount(2);
  await expect(page.locator(".history .hentry").first()).toContainText("edited");
  // clicking an entry opens the file
  await page.click(".history .hentry.clickable >> nth=0");
  await page.waitForURL(`/${pid}/guide.md`);
});

test("folder listing's Full history goes to the subtree feed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.click(".dl-more");
  await page.waitForURL(`/${pid}/history/notes`);
  await expect(page.locator("#crumb")).toContainText("History — notes/ (folder)");
  const paths = await page.$$eval(".history .hpath", (els) => els.map((e) => e.textContent));
  for (const p of paths) expect(p).toContain("notes/");
});

test("insights scopes to the selected folder via the ⋯ menu", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/notes`);
  await page.click("#more-btn");
  await page.click("#more-menu .more-item:has-text('Insights')");
  await page.waitForURL(`/${pid}/insights/notes`);
  await expect(page.locator(".in-title .in-scope")).toContainText("notes");
  // Scope note in the subtitle is the stable assertion.
  await expect(page.locator(".insights .dl-sub")).toContainText("notes and everything in it");
});

test("project menu pages each own a URL: Dashboard, Installation, Settings", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.click("#nav-dashboard");
  await page.waitForURL(`/${pid}/insights`);
  await expect(page.locator(".insights .in-title")).toContainText("Knowledge insights");
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  await page.click("#nav-install");
  await page.waitForURL(`/${pid}/install`);
  await expect(page.locator("#crumb")).toHaveText("Installation");
  await expect(page.locator("#nav-install")).toHaveClass(/active/);
  await page.click("#nav-history");
  await page.waitForURL(`/${pid}/history`);
  await expect(page.locator("#nav-history")).toHaveClass(/active/);
  await page.click("#nav-settings");
  await page.waitForURL(`/${pid}/settings`);
  await expect(page.locator("#crumb")).toHaveText("Project settings");
  await expect(page.locator(".project-settings h2")).toHaveText("wiki");
  await page.click("#nav-dashboard");
  await page.waitForURL(`/${pid}/insights`);
  await expect(page.locator("#nav-dashboard")).toHaveClass(/active/);
  // Deep link + reload land on the page, like any URL.
  await page.goto(`/${pid}/settings`);
  await expect(page.locator(".project-settings h2")).toHaveText("wiki");
  await expect(page.locator("#nav-settings")).toHaveClass(/active/);
});
