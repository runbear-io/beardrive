import { test, expect, Page } from "@playwright/test";
import { login } from "./helpers";

/* The file page: its top bar, the right rail's table of contents, and the
   comments panel's Open/Resolved tabs.

   A project of its own, files removed afterwards — the same reasons and the
   same sort-after-home/hub naming as review-comments.spec.ts. */

const PROJECT = "review-fileview-e2e";
let projectId = "";
let n = 0;
const litter: (() => Promise<void>)[] = [];

test.afterEach(async () => {
  while (litter.length) await litter.pop()!().catch(() => {});
});

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  return projectId;
}

async function ownFile(page: Page, id: string, body: string): Promise<string> {
  const name = `fileview-${Date.now()}-${n++}.md`;
  const r = await page.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`, {
    data: body,
    headers: { "Content-Type": "text/plain; charset=utf-8" },
  });
  expect(r.ok()).toBeTruthy();
  litter.push(async () => {
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } });
  });
  return name;
}

const filler = (k: number) =>
  Array.from({ length: k }, (_, i) => `Paragraph ${i} of filler so the page scrolls well past one screen.`).join("\n\n");

const DOC =
  "---\ntype: Policy\nokf_version: 0.2\nstatus: active\n---\n\n# Handbook\n\nIntro text.\n\n" +
  "## First section\n\n" + filler(30) + "\n\n## Second section\n\n" + filler(30) +
  "\n\n### A subsection\n\nTail text here.\n\n## Last section\n\n" + filler(10) + "\n";

test.use({ viewport: { width: 1440, height: 900 } });

test("top bar: Share is the only primary button, ⋯ holds Download/Print/Copy", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.goto(`/${id}/${file}`);
  await expect(page.locator("#content h1")).toHaveText("Handbook");

  const bar = page.locator("#topbar");
  await expect(bar.locator(".sync-pill")).toHaveText("Synced");
  // One primary action, and it is Share.
  await expect(bar.locator(".pbtn")).toHaveCount(1);
  await expect(bar.locator(".pbtn")).toHaveAttribute("id", "share-btn");
  await expect(bar.locator("#share-btn")).toContainText("Share");
  // Labelled ghost buttons and an outline Edit.
  await expect(bar.locator("#comments-btn")).toContainText("Comments");
  await expect(bar.locator("#comments-btn")).toHaveClass(/ghost/);
  await expect(bar.locator("#history-btn")).toContainText("History");
  await expect(bar.locator("#history-btn")).toHaveClass(/ghost/);
  await expect(bar.locator("#edit-btn")).toContainText("Edit");
  await expect(bar.locator("#edit-btn")).not.toHaveClass(/ghost|pbtn/);
  // The raw provenance string is gone from the bar; it is the doc header now.
  await expect(page.locator("#meta")).toBeEmpty();
  await expect(page.locator(".fv-head .fv-chip")).toHaveText([/Policy/, /OKF v0\.2/, /Markdown · /]);

  await page.click("#more-btn");
  const menu = page.locator("#more-menu");
  await expect(menu).toBeVisible();
  for (const item of ["Download", "Print", "Copy"]) {
    await expect(menu.getByRole("button", { name: item, exact: true })).toBeVisible();
  }

  await expect(bar.locator("#history-btn")).toHaveAttribute("aria-label", "History");
  await page.click("#history-btn");
  await expect(page).toHaveURL(new RegExp(`/${id}/activity/`));
});

test("right rail: table of contents follows the scroll and jumps on click", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.goto(`/${id}/${file}`);
  const toc = page.locator("aside.fv-rail .fv-toc");
  await expect(toc.locator("li")).toHaveText(["First section", "Second section", "A subsection", "Last section"]);
  await expect(toc.locator("li.sub")).toHaveText("A subsection");
  await expect(toc.locator("li.on")).toHaveText("First section");
  // Properties carries the whole frontmatter.
  await expect(page.locator("aside.fv-rail .fv-props dt")).toHaveText(["type", "okf_version", "status"]);

  await toc.getByRole("link", { name: "Second section" }).click();
  await expect(toc.locator("li.on")).toHaveText("Second section");
  await expect(toc.locator("li.on a")).toHaveAttribute("aria-current", "location");
  const top = await page.evaluate(() => document.getElementById("second-section")!.getBoundingClientRect().top);
  expect(top).toBeLessThan(300);

  // Scrolling by hand moves the highlight too.
  await page.evaluate(() => {
    const c = document.getElementById("content")!;
    c.style.scrollBehavior = "auto";
    c.scrollTop = c.scrollHeight;
  });
  await expect(toc.locator("li.on")).toHaveText("Last section");
});

test("comments panel: Open/Resolved tabs, quoted anchor, sweep note, jump to text", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  const make = async (quote: string, body: string) => {
    const r = await page.request.post(`/api/p/${id}/comments`, { data: { path: file, body, anchor: { quote } } });
    expect(r.ok()).toBeTruthy();
    return (await r.json()).comment.id as string;
  };
  await make("Intro text", "Is this still true?");
  const second = await make("Tail text here", "Expand this.");
  const r = await page.request.post(`/api/p/${id}/comments/${second}`, { data: { action: "resolve", body: "" } });
  expect(r.ok()).toBeTruthy();

  await page.goto(`/${id}/${file}`);
  await expect(page.locator("#comments-btn .cm-count")).toHaveText("1");
  await page.click("#comments-btn");
  const panel = page.locator(".cm-panel");
  const tabs = panel.locator(".cm-tabs [role=tab]");
  await expect(tabs).toHaveText(["Open · 1", "Resolved · 1"]);
  await expect(tabs.first()).toHaveAttribute("aria-selected", "true");
  // The panel takes the rail's place.
  await expect(page.locator("aside.fv-rail")).toBeHidden();

  const card = panel.locator(".cm-card");
  await expect(card).toHaveCount(1);
  await expect(card.locator(".cm-q")).toHaveText("Intro text");
  await expect(card.locator(".cm-queued")).toContainText("Queued for next agent sweep");
  await expect(card.getByRole("button", { name: "Resolve" })).toBeVisible();
  await card.getByRole("button", { name: "Jump to text" }).click();
  await expect(card).toHaveClass(/active/);
  // The commented passage carries a count badge in the margin.
  await expect(page.locator(".cm-pin")).toHaveText("1");

  await tabs.nth(1).click();
  await expect(tabs.nth(1)).toHaveAttribute("aria-selected", "true");
  await expect(panel.locator(".cm-card")).toHaveCount(1);
  await expect(panel.locator(".cm-card .cm-q")).toHaveText("Tail text here");
  await expect(panel.locator(".cm-card .cm-queued")).toHaveCount(0);
  await expect(panel.locator(".cm-card")).toContainText("Resolved by");
});

test("top bar fits between the phone breakpoint and a wide desktop", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  for (const width of [901, 1000, 1100, 1200]) {
    await page.setViewportSize({ width, height: 800 });
    await page.goto(`/${id}/${file}`);
    await expect(page.locator("#content h1")).toHaveText("Handbook");
    const bar = await page.locator("#topbar").boundingBox();
    for (const sel of ["#comments-btn", "#history-btn", "#edit-btn", "#share-btn", "#more-btn"]) {
      const b = await page.locator(sel).boundingBox();
      expect(b, `${sel} at ${width}px`).not.toBeNull();
      expect(b!.x + b!.width, `${sel} right edge at ${width}px`).toBeLessThanOrEqual(bar!.x + bar!.width + 0.5);
    }
  }
});

test("print drops the file rail; phone pins on one line do not overlap", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.goto(`/${id}/${file}`);
  await expect(page.locator("aside.fv-rail")).toBeVisible();
  await page.emulateMedia({ media: "print" });
  await expect(page.locator("aside.fv-rail")).toBeHidden();
  await page.emulateMedia({ media: "screen" });

  // Two open threads on the same quote.
  const anchor = { quote: "Intro text." };
  for (const body of ["first", "second"]) {
    const r = await page.request.post(`/api/p/${id}/comments`, { data: { path: file, anchor, body } });
    expect(r.ok(), await r.text()).toBeTruthy();
  }
  await page.setViewportSize({ width: 600, height: 900 });
  await page.goto(`/${id}/${file}`);
  const pins = page.locator(".cm-pin");
  await expect(pins).toHaveCount(2);
  const [a, b] = [await pins.nth(0).boundingBox(), await pins.nth(1).boundingBox()];
  const overlap = a!.y < b!.y + b!.height && b!.y < a!.y + a!.height && a!.x < b!.x + b!.width && b!.x < a!.x + a!.width;
  expect(overlap).toBe(false);
});

test("the page title is the first heading, ahead of the rail's section labels", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.goto(`/${id}/${file}`);
  await expect(page.locator("aside.fv-rail")).toBeVisible();
  const first = page.locator("#content :is(h1,h2,h3,h4,h5,h6)").first();
  await expect(first).toHaveText("Handbook");
  await expect(page.locator(".fv-rail :is(h1,h2,h3,h4,h5,h6)")).toHaveCount(0);
});

test("with comments open the hidden rail no longer limits the reading width", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.setViewportSize({ width: 1700, height: 900 });
  await page.goto(`/${id}/${file}`);
  await page.click("#comments-btn");
  await expect(page.locator("aside.fv-rail")).toBeHidden();
  const handle = page.locator(".rw-handle").last();
  await handle.focus();
  const before = (await page.locator(".fv-prose").boundingBox())!.width;
  for (let i = 0; i < 40; i++) await page.keyboard.press("ArrowRight");
  const after = (await page.locator(".fv-prose").boundingBox())!.width;
  expect(after).toBeGreaterThan(before);
});

test("leaving edit mode clears the editor's status; TOC links set the hash", async ({ page }) => {
  await login(page);
  const id = await project(page);
  const file = await ownFile(page, id, DOC);
  await page.goto(`/${id}/edit/${file}`);
  await expect(page.locator("#editor-state")).toBeVisible();
  await page.goto(`/${id}/${file}`);
  await page.click("#edit-btn");
  await expect(page.locator("#editor-state")).toBeVisible();
  await page.click("#edit-btn");
  await expect(page.locator("#editor-state")).toHaveCount(0);

  await page.locator(".fv-toc a", { hasText: "Second section" }).click();
  await expect(page).toHaveURL(/#second-section$/);
});
