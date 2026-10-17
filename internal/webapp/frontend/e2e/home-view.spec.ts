import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

// The project Home: what changed since your last visit, what needs you, and
// the first-run guide for a project with nothing in it yet.

test("greeting and the since-your-last-visit summary", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // A baseline far in the past: every seeded change is new. Set before the
  // page loads, since leaving the previous page moves the stored marker.
  await page.addInitScript((id) => localStorage.setItem("bdrive.lastVisit." + id, "1000"), pid);
  await page.goto(`/${pid}`);
  await expect(page.locator(".home-greet")).toHaveText(/^Good (morning|afternoon|evening)/);
  await expect(page.locator(".home-since")).toContainText("Since your last visit");
  await expect(page.locator(".home-since")).toContainText(/\d+ changes? across \d+ files?/);
  await expect(page.locator(".home-since")).toContainText("by agents");
});

test("jump back in lists the files this browser opened, newest first", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.evaluate((id) => localStorage.removeItem("bdrive.recent." + id), pid);
  await page.goto(`/${pid}/index.md`);
  await expect(page.locator("#content h1")).toHaveText("Wiki");
  await page.goto(`/${pid}/guide.md`);
  await expect(page.locator("#content h1")).toBeVisible();
  await page.click("#nav-home");
  await page.waitForURL("/" + pid);
  const cards = page.locator(".home-jump .home-card");
  await expect(cards).toHaveCount(2);
  await expect(cards.nth(0)).toContainText("guide.md");
  await expect(cards.nth(1)).toContainText("index.md");
  await cards.nth(1).click();
  await page.waitForURL(`/${pid}/index.md`);
});

test("needs your attention: conflict, comments on your files, hot-but-stale", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const made = await (
    await page.request.post(`/api/p/${pid}/comments`, {
      data: { path: "index.md", body: "home attention check", anchor: { quote: "Wiki" } },
    })
  ).json();
  try {
    await page.goto(`/${pid}`);
    const attn = page.locator(".home-attn");
    const conflict = attn.locator(".home-attn-row.conflict");
    await expect(conflict).toContainText("archive/old-runbook.md");
    await expect(attn.locator(".home-attn-row.comments", { hasText: "index.md" })).toContainText(
      "1 open comment",
    );
    await expect(attn.locator(".home-attn-row.stale", { hasText: "retired-spec.md" })).toContainText(
      "reads in 30d",
    );
    // Resolve lands on the conflict copy, where the banner explains it.
    await conflict.locator(".pbtn", { hasText: "Resolve" }).click();
    await page.waitForURL(/old-runbook\.md\.bdrive-conflict-/);
    await expect(page.locator(".vbanner")).toBeVisible();
  } finally {
    await page.request.delete(`/api/p/${pid}/comments/${made.comment.id}`);
  }
});

test("recent activity filters by actor and links to Activity", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  const feed = page.locator(".home-activity");
  await expect(feed.locator(".home-fe").first()).toBeVisible();
  await feed.getByRole("button", { name: "Agents" }).click();
  await expect(feed.locator(".fe-avatar.agent").first()).toBeVisible();
  await expect(feed.locator(".fe-avatar.human")).toHaveCount(0);
  await expect(feed.locator(".home-more")).toHaveAttribute("href", `/${pid}/activity?actor=agent`);
  await feed.getByRole("button", { name: "People" }).click();
  await expect(feed.locator(".fe-avatar.agent")).toHaveCount(0);
  await feed.locator(".home-more").click();
  await page.waitForURL(`/${pid}/activity?actor=human`);
});

test("right column: devices, connect card, where reads go", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  const dev = page.locator(".home-devices li", { hasText: "seed-agent" });
  await expect(dev).toBeVisible();
  await expect(dev.locator(".dev-agent")).toHaveText("Claude Code");
  await expect(page.locator(".home-connect .gd-code")).toContainText(pid);
  await expect(page.locator(".home-connect a", { hasText: "Full setup guide" })).toHaveAttribute(
    "href",
    `/${pid}/install`,
  );
  const archive = page.locator(".home-reads li", { hasText: "archive" });
  await expect(archive).toBeVisible();
  await expect(archive).toHaveClass(/fresh-(fresh|aging|stale)/);
  // The root listing stays reachable below the summary.
  await expect(page.locator("#home-files")).toContainText("notes");
});

test("an empty project's home is the connect guide", async ({ page }) => {
  await login(page);
  const made = await (await page.request.post("/api/projects", { data: { name: "home-empty" } })).json();
  const pid = made.project.id;
  try {
    await page.goto(`/${pid}`);
    await expect(page.locator(".guide")).toBeVisible();
    await expect(page.locator(".guide .gd-code").first()).toContainText(pid);
    await expect(page.locator(".home-greet")).toHaveCount(0);
  } finally {
    await page.request.delete("/api/projects/" + pid);
  }
});

test("peer-written names on Home are isolated from bidi reordering", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await expect(page.locator(".home-feed .fe-path").first()).toBeVisible();
  for (const sel of [".fe-who", ".fe-path", ".fe-dev", ".dev-name"]) {
    const el = page.locator(sel).first();
    if (!(await el.count())) continue;
    expect(await el.evaluate((n) => getComputedStyle(n).unicodeBidi), sel).toBe("isolate-override");
  }
});

test("a scope change refetches Home's history", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}`);
  await expect(page.locator(".home-feed").first()).toBeVisible();
  const refetch = page.waitForRequest((r) => r.url().includes(`/api/p/${pid}/history?`) && !r.url().includes("since="), {
    timeout: 15_000,
  });
  const put = await page.request.put(`/api/p/${pid}/folders`, { data: { prefix: "zz-scope-probe", default: "read" } });
  expect(put.ok(), await put.text()).toBeTruthy();
  await refetch;
  await page.request.delete(`/api/p/${pid}/folders?prefix=zz-scope-probe%2F`).catch(() => {});
});
