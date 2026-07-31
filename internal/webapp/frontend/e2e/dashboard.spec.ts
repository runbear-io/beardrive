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
