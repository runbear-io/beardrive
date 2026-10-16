import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// The Activity view: filters that speak the reader's calendar, a summary,
// day groups, session cards, the diff side panel and the three non-feed
// states. The seed's agent run (session 8f21e4) is the only agent activity.

const rows = ".history .hentry";

test("date presets resolve to the reader's local midnight", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const sent: string[] = [];
  page.on("request", (r) => {
    if (/\/history\?/.test(r.url())) sent.push(r.url());
  });
  await page.goto(`/${pid}/activity`);
  await expect(page.locator(rows).first()).toBeVisible();
  await page.selectOption(".hf-range", "7d");
  await page.waitForURL(`/${pid}/activity?range=7d`);
  await expect(page.locator(".hsum-sub")).toContainText("Last 7 days");
  // The API got an instant, and it is the reader's midnight six days ago.
  const url = new URL(sent.findLast((u) => u.includes("since="))!);
  const since = new Date(url.searchParams.get("since")!);
  const want = await page.evaluate(() => {
    const n = new Date();
    return new Date(n.getFullYear(), n.getMonth(), n.getDate() - 6).getTime();
  });
  expect(since.getTime()).toBe(want);
  // The 95-day-old archive files fall outside it.
  await expect(page.locator(`${rows} .hpath`, { hasText: "archive/legacy-notes.md" })).toHaveCount(0);
  await expect(page.locator(rows).first()).toBeVisible();

  // Custom days are local days, and the control names the zone.
  await page.selectOption(".hf-range", "custom");
  await expect(page.locator(".hf-date")).toHaveCount(2);
  const zone = await page.evaluate(() => Intl.DateTimeFormat().resolvedOptions().timeZone);
  await expect(page.locator(".hf-dates .hf-lbl")).toHaveText(zone);
});

test("the agent/people segment narrows to who made the change", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/activity`);
  await page.locator(".hf-seg button", { hasText: "Agents" }).click();
  await page.waitForURL(`/${pid}/activity?actor=agent`);
  await expect(page.locator(".hf-seg button", { hasText: "Agents" })).toHaveAttribute("aria-pressed", "true");
  // Only the seeded run's ops carry a session.
  await expect(page.locator(".hrun")).toHaveCount(1);
  for (const r of await page.locator(rows).all()) await expect(r).toHaveAttribute("data-agent", "");

  await page.locator(".hf-seg button", { hasText: "People" }).click();
  await page.waitForURL(`/${pid}/activity?actor=human`);
  await expect(page.locator(".hrun")).toHaveCount(0);
  await expect(page.locator(`${rows}[data-agent]`)).toHaveCount(0);
  await expect(page.locator(rows).first()).toBeVisible();
});

test("the summary splits changes into agents and people", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/activity`);
  await expect(page.locator(".hsum-total")).toContainText(/\d+ changes?/);
  await expect(page.locator(".hsum-col")).toHaveCount(7);
  await expect(page.locator(".hsum-sub")).toContainText("% by agents");
  await expect(page.locator(".hday").first()).toBeVisible();
});

test("a session card collapses and expands to per-file rows with counts", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/activity`);
  const run = page.locator(".hrun");
  await expect(run.locator(".hrun-what")).toContainText("Claude Code edited 2 files");
  await run.locator(".hrun-toggle").click();
  await expect(run.locator(".hentry")).toHaveCount(0);
  await run.locator(".hrun-toggle").click();
  await expect(run.locator(".hentry")).toHaveCount(2);
  // The rewritten file shows its line counts on the row.
  const readme = run.locator(".hentry", { hasText: "notes/readme.md" });
  await expect(readme.locator(".hstat .dv-add")).toHaveText("+1");
  await expect(readme.locator(".hstat .dv-del")).toHaveText("−1");
});

test("the diff panel follows J/K and closes on Escape", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/activity`);
  await expect(page.locator(rows).first()).toBeVisible();
  await expect(page.locator(".apanel")).toHaveCount(0);
  await page.locator("body").press("j");
  const panel = page.locator(".apanel");
  await expect(panel).toBeVisible();
  const first = await panel.locator(".ap-path").textContent();
  await expect(page.locator(`${rows}.sel`)).toHaveCount(1);
  await page.locator("body").press("j");
  await expect(panel.locator(".ap-path")).not.toHaveText(first!);
  await page.locator("body").press("k");
  await expect(panel.locator(".ap-path")).toHaveText(first!);
  // Tabs: the same change three ways.
  await panel.getByRole("tab", { name: "Full file" }).click();
  await expect(panel.locator(".dv-line").first()).toBeVisible();
  await panel.getByRole("tab", { name: "Changes" }).click();
  await expect(panel.getByRole("button", { name: "Copy link" })).toBeVisible();
  // Selecting is not navigating.
  await expect(page).toHaveURL(`/${pid}/activity`);
  await page.locator("body").press("Escape");
  await expect(panel).toHaveCount(0);

  // Details opens it on a chosen row; guide.md's edit diffs against its first version.
  const guide = page.locator(".history > .hentry", { hasText: "guide.md" }).first();
  await guide.locator(".hsel-btn").click();
  await expect(panel.locator(".dv-ins")).toContainText("Second version of the guide");
  await panel.locator(".ap-open").click();
  await page.waitForURL(new RegExp(`/${pid}/guide\\.md\\?v=[0-9a-f]{64}$`));
});

test("no match shows removable filter chips", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/activity?q=no-such-file&actor=human&range=7d`);
  const empty = page.locator(".hempty");
  await expect(empty).toContainText("No changes match these filters.");
  await expect(empty.locator(".hf-chip")).toHaveText(["path: no-such-file", "People", "Last 7 days"]);
  await expect(empty.locator(".hf-widen")).toHaveText("Show last 30 days");
  // A chip removes exactly its own filter.
  await empty.locator(".hf-chip", { hasText: "path:" }).click();
  await page.waitForURL(`/${pid}/activity?actor=human&range=7d`);
  await expect(page.locator(rows).first()).toBeVisible();
});

test("a history request that hangs turns into an error with Try again", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.clock.install();
  let hang = true;
  await page.route(/\/history\?/, async (route) => {
    if (hang) return; // never answered
    await route.continue();
  });
  await page.goto(`/${pid}/activity`);
  await expect(page.locator(".hskel")).toBeVisible();
  await page.clock.runFor(10_500);
  const err = page.locator(".herror");
  await expect(err).toContainText("Couldn't load activity");
  await expect(err).toContainText("within 10 seconds");
  hang = false;
  await page.clock.resume();
  await err.locator(".herror-retry").click();
  await expect(page.locator(rows).first()).toBeVisible();
  await expect(err).toHaveCount(0);
});
