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

// A brand-new project used to draw ~840px of empty frames with the quadrant
// labels floating over nothing — the first screen every project shows.
// Created at runtime and deleted: a permanent fixture sorting before "wiki"
// would move where the app lands and break home.spec.ts.
test("a project with no files says so instead of drawing empty charts", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "blank" } })).json();
  try {
    await page.goto(`/${made.project.id}/dashboard`);
    await expect(page.locator(".in-blank")).toContainText("no files");
    await expect(page.locator(".in-chart")).toHaveCount(0);
    await expect(page.locator("body")).not.toContainText("hot + stale");
    await expect(page.locator(".in-blank a")).toHaveAttribute(
      "href",
      `/${made.project.id}/install`,
    );
  } finally {
    await page.request.delete("/api/projects/" + made.project.id);
  }
});

// The other zero state, which was never broken: files that nobody has read
// still get a map, a scatter and a self-explaining hot path.
test("files with no reads still chart", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);
  await expect(page.locator(".in-treemap")).toBeVisible();
  await expect(page.locator(".in-blank")).toHaveCount(0);
});
