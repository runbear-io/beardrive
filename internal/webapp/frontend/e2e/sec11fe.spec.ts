import { test, expect, Page } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* Round 11 — the frontend as the BROWSER sees it. Everything here needs a
   real HTML parser, a real XSLT engine or a real React render, which is why
   these are not Go tests. Each case asserts the SECURE outcome, so it goes
   green the moment the hole is closed and stays as a regression test.

   helper prefix: sec11fe */

async function sec11feWatch(page: Page, fired: string[]) {
  page.on("dialog", async (d) => {
    fired.push("dialog:" + d.message());
    await d.dismiss();
  });
  await page.addInitScript(() => {
    (window as unknown as { __xss: string[] }).__xss = [];
  });
}

async function sec11feFired(page: Page): Promise<string[]> {
  return page.evaluate(() => (window as unknown as { __xss?: string[] }).__xss || []);
}

async function sec11fePut(page: Page, pid: string, path: string, body: string) {
  const r = await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent(path)}`,
    { data: body },
  );
  expect(r.ok(), `seeding ${path}: ${r.status()}`).toBeTruthy();
}

// Every event handler / active-scheme URL / active element the mounted
// markdown subtree carries after the client's transform.
async function sec11feActive(page: Page): Promise<string[]> {
  return page.evaluate(() => {
    const out: string[] = [];
    for (const el of Array.from(document.querySelectorAll("#content *"))) {
      for (const a of Array.from(el.attributes)) {
        if (/^on/i.test(a.name)) out.push(el.tagName + "[" + a.name + "]");
        if (
          /^(href|src|xlink:href|action|formaction|srcdoc)$/i.test(a.name) &&
          /^\s*(javascript|vbscript|data)\s*:/i.test(a.value)
        )
          out.push(el.tagName + "[" + a.name + "=" + a.value.slice(0, 24) + "]");
      }
      if (/^(SCRIPT|IFRAME|OBJECT|EMBED|BASE|FORM|META|LINK)$/.test(el.tagName))
        out.push("<" + el.tagName + ">");
    }
    return out;
  });
}

/* ------------------------------------------------------------------ *
 * 1. An XML document is an HTML document with extra steps.
 *
 * Backs TestSec_Frontend_InlineXMLIsWalledOffLikeEveryOtherMarkup: the Go
 * test asserts the missing header, this asserts the capability the header
 * exists to remove — script running on the hub's own origin, reading the
 * signed-in reader's API with the reader's session.
 * ------------------------------------------------------------------ */
test("an .xml file cannot run script on the hub origin", async ({ page }) => {
  const fired: string[] = [];
  await sec11feWatch(page, fired);
  await login(page);
  const pid = await wikiId(page);

  // The stylesheet. Same origin, and served as XML purely because of its
  // extension — so the attacker needs nothing but write access.
  await sec11fePut(
    page,
    pid,
    "attack/theme.xml",
    `<?xml version="1.0"?>
<xsl:stylesheet version="1.0" xmlns:xsl="http://www.w3.org/1999/XSL/Transform">
 <xsl:output method="html"/>
 <xsl:template match="/">
  <html><body>
   <script>
   <![CDATA[
     document.title = 'XSS-' + document.domain;
     fetch('/api/projects', {credentials:'include'})
       .then(function(r){ return r.text(); })
       .then(function(t){ document.body.setAttribute('data-leak', t.slice(0,120)); });
   ]]>
   </script>
  </body></html>
 </xsl:template>
</xsl:stylesheet>`,
  );
  // The document a teammate is invited to open.
  await sec11fePut(
    page,
    pid,
    "attack/report.xml",
    `<?xml version="1.0"?><?xml-stylesheet type="text/xsl" href="/api/p/${pid}/file?path=attack%2Ftheme.xml"?><doc/>`,
  );

  // Delivery is inside the app: a synced markdown file links to it, and
  // FileView's handleLinkClick leaves every href starting with "/" to the
  // browser — an ordinary same-tab, top-level navigation to the hub origin.
  await sec11fePut(
    page,
    pid,
    "attack/index.md",
    `# Q3\n\n[Open the report](/api/p/${pid}/file?path=attack%2Freport.xml)\n`,
  );
  await page.goto(`/${pid}/attack/index.md`);
  await expect(
    page.locator("#content a", { hasText: "Open the report" }),
  ).toHaveAttribute("href", /^\/api\/p\//);

  await page.goto(`/api/p/${pid}/file?path=${encodeURIComponent("attack/report.xml")}`, {
    waitUntil: "domcontentloaded",
  });
  await page.waitForTimeout(1500);

  expect(
    await page.locator("body").getAttribute("data-leak"),
    "the .xml document read the signed-in reader's /api/projects with their session",
  ).toBeNull();
  expect(await page.title(), "script from the .xml document ran on the hub origin").not.toMatch(
    /^XSS-/,
  );
  expect(fired).toEqual([]);
});

/* ------------------------------------------------------------------ *
 * 2. The markdown string transform (FileView.transformHTML).
 *
 * goldmark escapes raw HTML, but the CLIENT re-parses the server's string
 * with DOMParser, mutates it and re-serializes it into
 * dangerouslySetInnerHTML — parse / serialize / parse, which is where mXSS
 * lives. Round 9 asserted the server's output; nobody had run the round trip.
 * ------------------------------------------------------------------ */
const SEC11FE_MD: [string, string][] = [
  ["raw script", "# a\n\n<script>window.__xss.push('script')</script>\n"],
  ["img onerror", "<img src=x onerror=\"window.__xss.push('img')\">\n"],
  ["svg onload", "<svg onload=\"window.__xss.push('svg')\"></svg>\n"],
  ["iframe srcdoc", '<iframe srcdoc="&lt;script&gt;parent.__xss.push(1)&lt;/script&gt;"></iframe>\n'],
  ["javascript link", "[go](javascript:window.__xss.push('a'))\n"],
  ["javascript link mixed case", "[go](JaVaScRiPt:window.__xss.push('a'))\n"],
  ["javascript link entity", "[go](java&#115;cript:window.__xss.push('a'))\n"],
  ["vbscript link", "[go](vbscript:msgbox(1))\n"],
  // The classic mXSS shapes: inert as the first parser reads them, active
  // when a serializer writes them back out and a second parser reads them.
  ["mxss style in svg", "<svg><style><a title=\"</style><img src=x onerror=window.__xss.push('mxss1')>\">\n"],
  ["mxss noscript", "<noscript><p title=\"</noscript><img src=x onerror=window.__xss.push('mxss2')>\">\n"],
  ["mxss math annotation", '<math><annotation-xml encoding="text/html"><style><img src=x onerror=window.__xss.push(\'mxss3\')></style></annotation-xml></math>\n'],
  ["mxss textarea", "<textarea><p title=\"</textarea><img src=x onerror=window.__xss.push('mxss4')>\">\n"],
  ["mxss xmp", "<xmp><p title=\"</xmp><img src=x onerror=window.__xss.push('mxss5')>\">\n"],
  ["mxss noembed", "<noembed><p title=\"</noembed><img src=x onerror=window.__xss.push('mxss6')>\">\n"],
  ["mxss title", "<title><p title=\"</title><img src=x onerror=window.__xss.push('mxss7')>\">\n"],
  ["mxss nbsp in attribute", '<a title="a\u00a0b" href="x">t</a>\n'],
  ["code fence class break", '```x" onmouseover="window.__xss.push(\'fence\')\ncode\n```\n'],
  ["heading id break", '# a" onmouseover="window.__xss.push(\'hid\')\n'],
  ["link title break", '[t](/x "a\\" onmouseover=\\"window.__xss.push(\'title\')")\n'],
  ["wikilink target break", '[[a" onerror="window.__xss.push(\'wt\')]]\n'],
  ["wikilink label injection", "[[t|<img src=x onerror=window.__xss.push('wl')>]]\n"],
  ["wikilink label nested link", "[[t|a](javascript:window.__xss.push('wl2'))x]]\n"],
  ["frontmatter table break", '---\ntitle: "</td></tr></table><img src=x onerror=window.__xss.push(\'fm\')>"\n---\n\nbody\n'],
  ["frontmatter nested break", '---\na:\n  b: "</code><img src=x onerror=window.__xss.push(\'fm2\')>"\n---\n\nbody\n'],
  ["autolink javascript", "<javascript:window.__xss.push('auto')>\n"],
  ["gfm table cell", "| a |\n|---|\n| <img src=x onerror=window.__xss.push('tbl')> |\n"],
  ["backslash protocol", "[go](\\\\evil.test/x)\n"],
  ["relative image traversal", "![x](../../../../etc/passwd)\n"],
];

test("no markdown document survives the client-side HTML transform as active content", async ({
  page,
}) => {
  const fired: string[] = [];
  await sec11feWatch(page, fired);
  await login(page);
  const pid = await wikiId(page);
  const bad: string[] = [];

  for (const [name, src] of SEC11FE_MD) {
    const path = `attack/md-${name.replace(/[^a-z0-9]+/gi, "-")}.md`;
    // The marker is what proves the document actually rendered: without it
    // the assertions below would pass against a pane that never loaded.
    await sec11fePut(page, pid, path, src + "\nRENDER-MARKER\n");
    await page.goto(`/${pid}/${path}`);
    await expect(page.locator("#content")).toContainText("RENDER-MARKER", { timeout: 10_000 });
    await page.waitForTimeout(150);
    const ran = await sec11feFired(page);
    if (ran.length) bad.push(`${name}: executed ${JSON.stringify(ran)}`);
    if (fired.length) bad.push(`${name}: dialog ${JSON.stringify(fired.splice(0))}`);
    const live = await sec11feActive(page);
    if (live.length) bad.push(`${name}: mounted ${JSON.stringify(live)}`);
  }
  expect(bad, "payloads that survived the transform as active content").toEqual([]);
});

/* Split out of the battery above because it is the one payload goldmark
   deliberately admits: IsDangerousURL allows any `data:image/svg+xml;` URL,
   for images, and applies the same allowance to <a href>. The client's
   transform then leaves it alone (only http/https get target/rel). Current
   browsers refuse a top-level navigation to data:, so this is defence in
   depth rather than a live exploit — but round 9's own definition of active
   content (secdefActiveContent) counts a `data:` href, and the hub emits one. */
test("rendered markdown mounts no data: URL the app would follow", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const svg =
    "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIG9ubG9hZD0iYWxlcnQoMSkiLz4=";
  await sec11fePut(page, pid, "attack/data-url.md", `[go](${svg})\n\n![x](${svg})\n`);
  await page.goto(`/${pid}/attack/data-url.md`);
  // Wait for the transform to have mounted something, or the assertion below
  // would pass against an empty pane.
  await expect(page.locator("#content a")).toHaveCount(1);
  expect(await sec11feActive(page)).toEqual([]);
});

/* ------------------------------------------------------------------ *
 * 3. A project's icon is a key into a plain object.
 *
 * shell.tsx: PROJECT_ICONS[name ?? ""] ?? Folder. The server validates the
 * SHAPE only (iconRe = ^[a-z0-9-]{1,32}$ — deliberately, so adding an icon
 * needs no server change), and "constructor" has that shape while resolving
 * through Object.prototype to a function rather than to undefined. So the
 * `?? Folder` fallback never fires and React is handed Object as a
 * component. One admin PATCH, and the whole SPA renders nothing — for every
 * member of the org, on every route, with no way back through the UI.
 * ------------------------------------------------------------------ */
test("a project icon nobody shipped falls back instead of taking the app down", async ({
  page,
}) => {
  await login(page);
  const pid = await wikiId(page);
  const errors: string[] = [];
  page.on("pageerror", (e) => errors.push(String(e)));
  try {
    // Control: with an ordinary icon the app mounts. Set explicitly so the
    // baseline holds however a previous run left the fixture.
    await page.request.patch(`/api/projects/${pid}`, { data: { icon: "folder" } });
    await page.goto(`/${pid}`);
    await expect(page.locator("#sidebar")).toBeVisible({ timeout: 10_000 });
    expect(errors, "control render").toEqual([]);

    const r = await page.request.patch(`/api/projects/${pid}`, {
      data: { icon: "constructor" },
    });
    expect(r.ok(), "the hub accepts it as an ordinary admin edit").toBeTruthy();

    await page.goto(`/${pid}`);
    // An unknown icon name is documented to fall back to the folder
    // placeholder, so the only difference must be the glyph.
    await expect(page.locator("#sidebar")).toBeVisible({ timeout: 10_000 });
    expect(errors.join("\n")).toBe("");
  } finally {
    await page.request.patch(`/api/projects/${pid}`, { data: { icon: "" } });
  }
});

/* ------------------------------------------------------------------ *
 * 4. A filename that reads as a different file.
 *
 * Backs TestSec_Frontend_APathCannotCarryTheControlsThatReorderARow: the Go
 * test asserts the ingest doors, this asserts what the row looks like once
 * one gets through.
 * ------------------------------------------------------------------ */
test("a filename cannot re-order its own row into a different extension", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  const evil = "bidi/invoice\u202egnp.exe";
  const r = await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent(evil)}`,
    { data: "MZ" },
  );
  if (!r.ok()) return; // refused at ingest — the Go test's preferred outcome

  await page.goto(`/${pid}/bidi`);
  const row = page.locator("#content .dl-row").first();
  await expect(row).toBeVisible();
  const shown = ((await row.textContent()) || "").trim();
  expect(
    /[\u061c\u200e\u200f\u202a-\u202e\u2066-\u2069]/.test(shown),
    `the listing renders ${JSON.stringify(shown)} — the bidi override reverses the rest of the ` +
      `row, so an .exe presents itself as a .png to every reader`,
  ).toBe(false);
});
