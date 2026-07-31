import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// A missing quadrant label is invisible to every check except this one.
test("reads × freshness names all four quadrants", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-chart .in-quad")).toHaveText([
    "hot + stale",
    "hot + fresh",
    "cold + stale",
    "cold + fresh",
  ]);
});

/* A heat row outlives its file. Before this, the file panels joined heat onto
   the tree and dropped whatever didn't match — so the page could say "no reads"
   while the agent-coverage panel beside it rendered those same reads. */
test("reads for a deleted file are ranked and labelled, not dropped", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);

  const row = page.locator(".in-hp-row", { hasText: "scratch.md" });
  await expect(row).toHaveCount(1); // seeded with reads, deleted by the seed
  await expect(row.locator(".in-hp-gone")).toHaveText("· no longer in the project");
  await expect(page.locator(".insights")).not.toContainText("No reads in the window yet");
  await expect(page.locator(".in-orphan-note")).toHaveText([
    "1 file with reads is no longer in the project — see Hot path.",
    "1 file with reads is no longer in the project — see Hot path.",
  ]);

  // The file view would 404 on it; history still has the content.
  await row.click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/history/scratch\\.md$`));
});

// The footnote counts what Hot path will actually list, per lens — scratch.md
// has human reads only, so the agent lens has no orphan to report.
test("the orphan footnote follows the lens", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-orphan-note").first()).toBeVisible();
  await page.getByRole("button", { name: "Agent reads" }).click();
  await expect(page.locator(".in-orphan-note")).toHaveCount(0);
  await expect(page.locator(".in-hp-row", { hasText: "scratch.md" })).toHaveCount(0);
  await page.getByRole("button", { name: "Human reads" }).click();
  await expect(page.locator(".in-hp-row", { hasText: "scratch.md" })).toHaveCount(1);
});
