import { test, expect, type Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* The column system (shell.tsx <Page>, style.css .page): every route renders
   exactly one .page, and pages of the same width share the same column edges.
   These used to range from 560px to unbounded — half of them uncentered — so
   no two routes lined up. Widths live in CSS tokens; this asserts the routes
   actually resolve to them. */

const WIDTHS = { read: 768, app: 768 }; // both = Tailwind md; wide is viewport-capped at 1280

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

  await visit("", "app"); // project home
  await visit("/install", "app"); // the same guide, so the same column
  await visit("/settings", "app");
  await visit("/insights", "app"); // charts cap their own measure; the column is normal
  await visit("/history", "app"); // structured view, not a file render
  await visit("/index.md", "read"); // rendered markdown — the only read surface
  await visit("/notes", "app"); // folder listing is a structured view too
});

test("the install route and the project home render the guide identically", async ({ page }) => {
  // They are two sidebar items apart and show the same component; /install
  // used to wrap it in the .onboard card — 320px narrower, 90px lower.
  await login(page);
  const pid = await wikiId(page);
  const box = async (path: string) => {
    await page.goto(`http://localhost:8993/${pid}${path}`);
    await page.waitForSelector(".guide");
    return page.evaluate(() => {
      const r = (document.querySelector(".guide") as HTMLElement).getBoundingClientRect();
      return { left: Math.round(r.left), width: Math.round(r.width), top: Math.round(r.top) };
    });
  };
  expect(await box("/install")).toEqual(await box(""));
});

test("charts never scale past the size they were drawn at", async ({ page }) => {
  // .in-chart SVGs are viewBox="0 0 720 …" at width:100%, so an unbounded
  // column magnifies them — labels ended up larger than the page title.
  await page.setViewportSize({ width: 1600, height: 900 });
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`http://localhost:8993/${pid}/insights`);
  await page.waitForSelector(".in-chart");
  const worst = await page.evaluate(() => {
    let max = 0;
    for (const el of document.querySelectorAll(".in-chart")) {
      const vb = (el.getAttribute("viewBox") || "0 0 720 0").split(/\s+/);
      max = Math.max(max, el.getBoundingClientRect().width / Number(vb[2]));
    }
    return max;
  });
  expect(worst, "chart scale factor").toBeLessThanOrEqual(1.06);
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
