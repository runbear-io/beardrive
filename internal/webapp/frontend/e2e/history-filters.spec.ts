import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// BEA-67: History was a flat scroll with no way to narrow it. The filter bar
// drives the API (not the loaded page) and lives in the URL, so a narrowed
// feed is linkable, survives reload, and Back undoes it.

// Every row currently on screen, run cards included.
const rows = ".history .hentry";

test("the path filter narrows the feed and lands in the URL", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  await expect(page.locator(rows).first()).toBeVisible();
  const before = await page.locator(rows).count();
  expect(before).toBeGreaterThan(3);

  await page.fill(".hfilters input[type=search]", "runbook");
  await page.waitForURL(`/${pid}/history?q=runbook`);
  // Every row matches, and there are strictly fewer of them. (Exact counts
  // would be a lie: the suite shares one mutable hub, and earlier specs
  // upload and restore into this feed.)
  await expect(page.locator(`${rows} .hpath`).first()).toBeVisible();
  const paths = await page.locator(`${rows} .hpath`).allTextContents();
  expect(paths.length).toBeLessThan(before);
  for (const p of paths) expect(p.toLowerCase()).toContain("runbook");

  // A reload gets the same narrowed feed — the filter is not component state.
  await page.reload();
  await expect(page.locator(".hfilters input[type=search]")).toHaveValue("runbook");
  await expect(page.locator(rows)).toHaveCount(paths.length);

  // Back undoes the filter like any other navigation.
  await page.goBack();
  await expect(page).toHaveURL(`/${pid}/history`);
  await expect(page.locator(rows)).toHaveCount(before);
});

test("the author filter offers the accounts in the feed and narrows to one", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  const sel = page.locator(".hfilters select.hf-user");
  await expect(sel.locator("option")).toContainText(["Anyone", "alice@x.io", "bob@x.io"]);

  await sel.selectOption("bob@x.io");
  await page.waitForURL(`/${pid}/history?user=bob%40x.io`);
  // the seed gives bob exactly one change, and nothing else in the suite
  // ever writes as him
  await expect(page.locator(rows)).toHaveCount(1);
  await expect(page.locator(`${rows} .hpath`)).toHaveText("notes/deep/topic.md");
  // and the other author is still selectable — filtering by one must not
  // strand the reader with a list rebuilt from their rows alone
  await expect(sel.locator("option")).toContainText(["Anyone", "alice@x.io", "bob@x.io"]);
});

test("the date range filters server-side, and filters compose", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // Everything the seed writes is within the last few days, so a window that
  // ends long ago must empty the feed rather than quietly return all of it.
  await page.goto(`/${pid}/history?since=2020-01-01&until=2020-01-02`);
  await expect(page.locator(rows)).toHaveCount(0);
  await expect(page.locator(".history .empty")).toContainText("No changes match these filters.");

  // Compose: an author who did write, plus a window that excludes everyone.
  await page.goto(`/${pid}/history?user=alice%40x.io&until=2020-01-02`);
  await expect(page.locator(rows)).toHaveCount(0);
  await expect(page.locator(".hfilters select.hf-user")).toHaveValue("alice@x.io");
  await expect(page.locator(".hf-date").nth(1)).toHaveValue("2020-01-02");
});

test("no match offers a way out, and Clear empties the query string", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history?q=no-such-file`);
  await expect(page.locator(rows)).toHaveCount(0);
  const empty = page.locator(".history .empty");
  await expect(empty).toContainText("No changes match these filters.");
  await empty.getByRole("button", { name: "Clear filters" }).click();
  await page.waitForURL(`/${pid}/history`);
  await expect(page.locator(rows).first()).toBeVisible();

  // The bar's own Clear does the same, and only shows while something is set.
  await expect(page.locator(".hf-clear")).toHaveCount(0);
  await page.goto(`/${pid}/history?q=runbook`);
  await page.locator(".hf-clear").click();
  await page.waitForURL(`/${pid}/history`);
});

test("the folder feed and the per-file version list filter too", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // folder subtree: the filter composes with the prefix scoping
  await page.goto(`/${pid}/history/notes?q=readme`);
  await expect(page.locator(".hfilters")).toBeVisible();
  for (const p of await page.locator(`${rows} .hpath`).allTextContents()) {
    expect(p).toContain("notes/readme.md");
  }
  // per-file version list: guide.md has two versions, one of them Alice's
  await page.goto(`/${pid}/history/guide.md`);
  await expect(page.locator(rows).first()).toBeVisible();
  expect(await page.locator(rows).count()).toBeGreaterThanOrEqual(2);
  await page.goto(`/${pid}/history/guide.md?user=bob%40x.io`);
  await expect(page.locator(rows)).toHaveCount(0);
});
