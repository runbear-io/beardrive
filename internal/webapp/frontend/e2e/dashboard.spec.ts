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

/* Three defects on one chart (BEA-60): the busiest dot sat half outside the
   frame, no dot said which file it was, and the size caption was drawn on top
   of the "hot + stale" label. The frame numbers come off the viewBox rather
   than being repeated here. */
test("every dot sits inside the frame, and the hot+stale ones say which file they are", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/dashboard`);

  const svg = page.locator("svg.in-chart:not(.in-treemap):not(.in-matrix)");
  const [, , W, H] = (await svg.getAttribute("viewBox"))!.split(" ").map(Number);
  const M = { l: 44, r: 16, t: 20, b: 34 }; // Scatter's margins

  const dots = await svg.locator(".in-pt").evaluateAll((els) =>
    els.map((e) => ({
      cx: +e.getAttribute("cx")!,
      cy: +e.getAttribute("cy")!,
      r: +e.getAttribute("r")!,
    })),
  );
  expect(dots.length).toBeGreaterThan(0);
  // The repro: the hottest file landed at cy === M.t with r up to 7.
  expect(Math.min(...dots.map((d) => d.cy - d.r))).toBeGreaterThanOrEqual(M.t);
  expect(Math.max(...dots.map((d) => d.cx + d.r))).toBeLessThanOrEqual(W - M.r);
  expect(Math.max(...dots.map((d) => d.cy + d.r))).toBeLessThanOrEqual(H - M.b);
  expect(Math.min(...dots.map((d) => d.cx - d.r))).toBeGreaterThanOrEqual(M.l);

  // The seeded archive/ files are hot and months old — the danger quadrant.
  const labels = svg.locator(".in-pt-label");
  const names = await labels.allTextContents();
  expect(names.length).toBeGreaterThan(0);
  expect(names.length).toBeLessThanOrEqual(6);
  expect(names).toContain("retired-spec.md"); // basename, not the full path
  for (const n of names) {
    await expect(page.locator(".in-hp-row", { hasText: n })).not.toHaveCount(0);
  }
  // Inside the frame, and no two labels on the same baseline.
  const boxes = await labels.evaluateAll((els) =>
    els.map((e) => e.getBoundingClientRect()),
  );
  const plot = (await svg.boundingBox())!;
  const s = plot.width / W; // viewBox units → screen px
  for (const b of boxes) {
    expect(b.left).toBeGreaterThanOrEqual(plot.x + M.l * s - 1);
    expect(b.right).toBeLessThanOrEqual(plot.x + (W - M.r) * s + 1);
    expect(b.top).toBeGreaterThanOrEqual(plot.y + M.t * s - 1);
    expect(b.bottom).toBeLessThanOrEqual(plot.y + (H - M.b) * s + 1);
  }
  for (let i = 0; i < boxes.length; i++)
    for (let j = i + 1; j < boxes.length; j++)
      expect(
        boxes[i].right < boxes[j].left ||
          boxes[j].right < boxes[i].left ||
          boxes[i].bottom < boxes[j].top ||
          boxes[j].bottom < boxes[i].top,
      ).toBe(true);

  // A dot keeps its tooltip and its click even where a label now sits.
  await expect(svg.locator(".in-pt").first().locator("title")).toHaveCount(1);
  await expect(svg.locator(".in-pt.danger").first()).toBeVisible();

  // The caption moved out of the plot, where it overprinted "hot + stale".
  await expect(svg).not.toContainText("dot size");
  await expect(page.locator(".in-cap")).toHaveText("dot size = agent share of reads");
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
