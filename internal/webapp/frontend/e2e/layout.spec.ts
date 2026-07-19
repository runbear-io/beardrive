import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* The column system (shell.tsx <Page>, style.css .page): every route renders
   exactly one .page, and pages of the same width share the same column edges.
   These used to range from 560px to unbounded — half of them uncentered — so
   no two routes lined up. Widths live in CSS tokens; this asserts the routes
   actually resolve to them. */

const WIDTHS = { read: 704, app: 880 }; // wide is viewport-capped at 1280

async function column(page: Page) {
  return page.evaluate(() => {
    const pages = document.querySelectorAll("#content > .page");
    if (pages.length !== 1) throw new Error(`expected 1 .page, got ${pages.length}`);
    const el = pages[0] as HTMLElement;
    const r = el.getBoundingClientRect();
    const kind = el.classList.contains("read") ? "read" : el.classList.contains("wide") ? "wide" : "app";
    return { kind, left: Math.round(r.left), width: Math.round(r.width) };
  });
}

test("every view shares one column system", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page); // the seeded project — other specs create their own
  const seen: Record<string, { left: number; width: number }> = {};

  const visit = async (path: string, want: "read" | "app" | "wide") => {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector("#content > .page");
    const col = await column(page);
    expect(col.kind, `${path || "/"} column kind`).toBe(want);
    if (want !== "wide") {
      expect(col.width, `${path || "/"} column width`).toBe(WIDTHS[want]);
    }
    // Same width class ⇒ identical edges, on every route.
    if (seen[want]) {
      expect(col.left, `${path || "/"} left edge matches other ${want} pages`).toBe(seen[want].left);
      expect(col.width, `${path || "/"} width matches other ${want} pages`).toBe(seen[want].width);
    } else {
      seen[want] = col;
    }
  };

  await visit("", "app"); // project home / install guide
  await visit("/history", "app");
  await visit("/settings", "app");
  await visit("/insights", "wide");
  await visit("/index.md", "read"); // rendered markdown
  await visit("/notes", "read"); // folder listing
});

test("the gutter belongs to the scroll container, not the column", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  // A markdown page used to carry the max-width on #content itself, so its
  // gutter came out of the reading measure and .md ran ~80px narrower than
  // every other page. The gutter must live on #content alone.
  for (const path of ["/index.md", "/notes", "/history"]) {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector("#content > .page");
    const r = await page.evaluate(() => {
      const c = document.querySelector("#content") as HTMLElement;
      const p = document.querySelector("#content > .page") as HTMLElement;
      return {
        contentMax: getComputedStyle(c).maxWidth,
        pad: getComputedStyle(c).paddingLeft,
        childMax: getComputedStyle(p.firstElementChild as HTMLElement).maxWidth,
      };
    });
    expect(r.contentMax, `${path}: #content must not constrain width`).toBe("none");
    expect(r.pad, `${path}: gutter`).toBe("40px");
    // Views may not re-declare a column of their own inside .page.
    expect(r.childMax, `${path}: view sets its own max-width`).toBe("none");
  }
});
