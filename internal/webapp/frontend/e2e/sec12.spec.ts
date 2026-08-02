import { test, expect, type Page } from "@playwright/test";
import { login, wikiId, ADMIN, MEMBER } from "./helpers";

/* Round 12, row 24 — the in-repo router (nav.ts + router.ts) driven in a
   browser for the first time, plus /join/<token> as a document.

   Round 11 read this router and reported no escape "by reading". These are
   the two things reading missed, both reachable from another account's
   content and both persistent across a reload.

   EVERY assertion below first proves the page actually rendered — round 11's
   first cut passed against an empty pane. `alive()` is that proof: it asserts
   the SPA shell mounted AND #root has real children, so "no crash" can never
   be satisfied by "nothing on screen". */

async function alive(page: Page, where: string, errs: string[] = []) {
  // The shell is React's output, not the server's: index.html ships an empty
  // <div id="root">, so a sidebar means the app booted and stayed up. The
  // collected page errors ride in the message so a failure names its cause
  // instead of leaving the reader to guess at a blank pane.
  try {
    await expect(page.locator("#sidebar")).toBeVisible({ timeout: 10_000 });
  } catch {
    // Report at failure time, not at call time: the uncaught error lands a
    // tick after page.goto resolves, so a message built eagerly says "(none)"
    // for a page that plainly threw.
    const len = await page.evaluate(() => document.getElementById("root")?.innerHTML.length ?? -1);
    throw new Error(
      `SPA did not render at ${where}: #root innerHTML is ${len} chars; uncaught: ${errs.join(" | ") || "(none)"}`,
    );
  }
  const kids = await page.locator("#root > *").count();
  expect(kids, `#root is empty at ${where} — the app unmounted`).toBeGreaterThan(0);
}

// Collects uncaught page errors so a silent unmount is named, not guessed at.
function watchErrors(page: Page): string[] {
  const errs: string[] = [];
  page.on("pageerror", (e) => errs.push(String(e)));
  return errs;
}

// The e2e login helper caches one session per identity and reuses its cookies;
// switching identity inside a spec has to drop the previous one first, or the
// old session survives and the second login never happens.
async function sec12as(page: Page, who: string) {
  await page.context().clearCookies();
  await login(page, who);
}

/* ------------------------------------------------------------------ *
 * FINDING 1 — decodePath's unguarded decodeURIComponent
 * ------------------------------------------------------------------ */

/* router.ts:

     export function decodePath(p: string): string {
       return p.split("/").map(decodeURIComponent).join("/");
     }

   "%80" is a syntactically valid percent escape — Go's URL parser accepts it
   and the hub serves the SPA shell for it (asserted from Go in
   sec_frontend2_test.go: TestSec_Router_TheShellIsServedForPathsTheClient
   MustSurvive) — but it is not valid UTF-8, so decodeURIComponent throws
   URIError. parseRoute runs inside HubApp's useMemo during render, the app has
   no error boundary (main.tsx mounts <App/> bare), so the throw unmounts the
   whole SPA. The address bar still holds the URL, so a reload reproduces it.

   Reachable from another account: any synced markdown may contain
   [x](/<pid>/%80). The href starts with "/" so handleLinkClick leaves it to
   the browser, which does a full navigation to a path the router dies on.

   Secure behavior: an undecodable segment is just a path that names no file —
   the app must stay up and say so. */
test("TestSec_Router_AnUndecodablePathSegmentDoesNotUnmountTheApp", async ({ page }) => {
  const errs = watchErrors(page);
  await login(page, ADMIN);
  const pid = await wikiId(page);

  // Control: the same navigation shape with a decodable segment works, so the
  // failure below is the escape and not the route.
  await page.goto(`/${pid}/no-such-file-here`);
  await alive(page, "a plain missing path");

  for (const seg of ["%80", "a%C0%80b", "%ED%A0%80"]) {
    await page.goto(`/${pid}/${seg}`);
    await alive(page, `/${pid}/${seg}`, errs);
  }

  // And it must survive a reload, since that is what a victim does next.
  await page.reload();
  await alive(page, "reload of /%ED%A0%80", errs);

  expect(errs.filter((e) => /URIError|URI malformed/.test(e)), "router threw on a URL a browser hands it").toEqual([]);
});

/* The same input arriving the way it actually would: a link inside a document
   another member wrote. Proves the finding is cross-user, not "type a weird
   URL at yourself". */
test("TestSec_Router_ALinkInATeammatesDocumentCannotKillTheReadersApp", async ({ page }) => {
  const errs = watchErrors(page);
  await sec12as(page, MEMBER);
  const pid = await wikiId(page);

  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("notes/sec12-trap.md")}`,
    { data: `# Trap\n\n[open the report](/${pid}/%80)\n` },
  );

  await sec12as(page, ADMIN);
  await page.goto(`/${pid}/notes/sec12-trap.md`);
  // Prove the document really rendered before clicking anything in it.
  await expect(page.locator("#content h1")).toHaveText("Trap");

  await page.click("#content a:has-text('open the report')");
  await alive(page, "after following a teammate's link", errs);
  expect(errs.filter((e) => /URIError|URI malformed/.test(e))).toEqual([]);
});

/* ------------------------------------------------------------------ *
 * FINDING 2 — LEGACY_VIEWS is indexed with a server/user-supplied string
 * ------------------------------------------------------------------ */

/* router.ts:

     const LEGACY_VIEWS: Record<string, ViewName> = { insights: "dashboard" };
     ...
     if (VIEW_ROUTES.has(head) || LEGACY_VIEWS[head]) {
       r.view = LEGACY_VIEWS[head] || (head as ViewName);
       if (LEGACY_VIEWS[head]) r.legacyView = true;

   `head` is the first path segment after the project id — i.e. a file or
   folder name any member can create. LEGACY_VIEWS["constructor"] resolves
   through Object.prototype to the Object constructor: truthy, so the segment
   is treated as a renamed view, `view` is set to a FUNCTION, and HubApp's
   legacy-view redirect (HubApp.tsx:261) rewrites the address bar to
   urlForView(<function>, pid) — literally "/<pid>/function Object() { [native
   code] }". The file is then unreachable by URL for every member of the org,
   permanently, and nothing in the UI explains it.

   This is the exact shape round 11 found in ProjectIcon ({"icon":"constructor"})
   and fixed there with Object.hasOwn. The router kept the bug.

   Secure behavior: "constructor" is not a view name, so /<pid>/constructor is
   the file path "constructor" — no redirect, no rewrite. */
test("TestSec_Router_APathNamedLikeAnObjectPrototypeMemberIsNotAViewRoute", async ({ page }) => {
  const errs = watchErrors(page);
  await login(page, ADMIN);
  const pid = await wikiId(page);

  // Control: a real legacy view DOES normalize, so the mechanism is live.
  await page.goto(`/${pid}/insights`);
  await alive(page, "/insights");
  await expect(page).toHaveURL(new RegExp(`/${pid}/dashboard$`));

  for (const seg of ["constructor", "toString", "valueOf", "hasOwnProperty", "__proto__"]) {
    await page.goto(`/${pid}/${seg}`);
    await alive(page, `/${pid}/${seg}`, errs);
    expect(
      decodeURIComponent(new URL(page.url()).pathname),
      `/${pid}/${seg} was rewritten by the legacy-view redirect`,
    ).toBe(`/${pid}/${seg}`);
  }

  expect(errs).toEqual([]);
});

/* The same finding as a teammate would trigger it: a folder named
   "constructor" at the project root, opened from the file tree. */
test("TestSec_Router_AFolderNamedConstructorStaysReachableFromTheTree", async ({ page }) => {
  await sec12as(page, MEMBER);
  const pid = await wikiId(page);
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("constructor/plan.md")}`,
    { data: "# Quarterly plan\n\nInside a folder named constructor.\n" },
  );

  await sec12as(page, ADMIN);
  await page.goto(`/${pid}`);
  const row = page.locator('#tree .row[data-path="constructor"]');
  await expect(row, "the tree never showed the folder — nothing was measured").toBeVisible({
    timeout: 15_000,
  });
  await row.click();
  await alive(page, "after opening the folder");
  expect(decodeURIComponent(new URL(page.url()).pathname)).toBe(`/${pid}/constructor`);
});

/* ------------------------------------------------------------------ *
 * Clean-surface assertions — these are expected to PASS and exist so the
 * next round does not have to re-run them blind.
 * ------------------------------------------------------------------ */

/* Back/forward and reload must land on the same page the link did — the whole
   reason CLAUDE.md forbids URL-less panel state. */
test("TestSec_Router_BackForwardAndReloadLandWhereTheLinkDid", async ({ page }) => {
  await login(page, ADMIN);
  const pid = await wikiId(page);

  await page.goto(`/${pid}/notes`);
  await alive(page, "/notes");
  await page.goto(`/${pid}/history`);
  await expect(page.locator("#content")).toBeVisible();
  const historyURL = page.url();

  await page.goBack();
  await expect(page).toHaveURL(new RegExp(`/${pid}/notes$`));
  await alive(page, "back");
  await page.goForward();
  await expect(page).toHaveURL(historyURL);
  await alive(page, "forward");
  await page.reload();
  await expect(page).toHaveURL(historyURL);
  await alive(page, "reload");
});

/* linkProps is the single place that decides ours-vs-the-browser's, and
   parseRoute is what reads the result back. A same-origin path that LOOKS
   like an authority must stay a path: it may not be pushed as a cross-origin
   destination, and it may not take the shell down on the way. */
test("TestSec_Router_AnAuthorityShapedPathStaysAPath", async ({ page }) => {
  const errs = watchErrors(page);
  await login(page, ADMIN);
  const pid = await wikiId(page);
  const origin = new URL(page.url()).origin;

  for (const attempt of [`/${pid}//example.invalid`, `/${pid}/%2f%2fexample.invalid`, `/${pid}/\\example.invalid`]) {
    await page.goto(attempt).catch(() => {});
    expect(new URL(page.url()).origin, `${attempt} left the origin`).toBe(origin);
    await alive(page, attempt, errs);
  }
  expect(errs).toEqual([]);
});

/* /join/<token> as a document. A forged token must not read as a join, must
   not crash the shell, and must not leave the token sitting in history for
   the next person on the machine to press Back into. */
test("TestSec_Join_AForgedTokenNeitherJoinsNorCrashesTheShell", async ({ page }) => {
  const errs = watchErrors(page);
  await login(page, ADMIN);

  const before = await (await page.request.get("/api/orgs")).json();

  // Every one of these is a path the SPA shell is served for; "../.." forms
  // are excluded because a browser normalizes them before the app ever runs.
  for (const tok of ["deadbeefdeadbeefdeadbeefdeadbeef", "NOTHEX", "0", "ffffffffffffffffffffffffffffffff"]) {
    await page.goto(`/join/${tok}`);
    await alive(page, `/join/${tok}`, errs);
  }

  const after = await (await page.request.get("/api/orgs")).json();
  expect(after.orgs.length, "a forged invite changed org membership").toBe(before.orgs.length);
  expect(errs).toEqual([]);
});

/* A real invite, redeemed in a browser: the token must be replaced in history
   (not pushed), so Back does not re-expose it. */
test("TestSec_Join_ARedeemedTokenIsReplacedInHistoryNotPushed", async ({ page }) => {
  await login(page, ADMIN);
  const orgs = await (await page.request.get("/api/orgs")).json();
  const org = orgs.orgs[0].id;
  const inv = await (await page.request.post(`/api/orgs/${org}/invites`)).json();
  expect(inv.token, "could not mint an invite — nothing was measured").toBeTruthy();

  await page.goto("/");
  await alive(page, "home");
  await page.goto(`/join/${inv.token}`);
  // The join completes and replaces the URL.
  await expect(page).not.toHaveURL(new RegExp(`/join/${inv.token}`), { timeout: 10_000 });

  await page.goBack();
  expect(page.url(), "Back re-exposed the invite token").not.toContain(inv.token);
});

/* ShareDialog's "Open" is `window.open(url, "_blank")` with no rel/noopener.
   The destination is the hub's own /s/<token> viewer, which serves a shared
   HTML file as the TOP-LEVEL document under `CSP: sandbox allow-scripts` —
   scripts run. If that document can still reach window.opener, any member who
   can mint a share can repoint the tab of whoever clicks Open, from a page on
   the hub's own hostname. Reverse tabnabbing with the hub's name in the
   address bar is a credible sign-in phish.

   Secure behavior: the opener tab stays where it was. */
test("TestSec_Share_AnOpenedShareCannotRepointTheHubTab", async ({ page, context }) => {
  await login(page, ADMIN);
  const pid = await wikiId(page);
  await page.request.put(
    `/api/p/${pid}/upload/content?path=${encodeURIComponent("pages/sec12-tab.html")}`,
    {
      data:
        "<h1 id='t'>shared</h1><script>try{if(window.opener){window.opener.location='/sec12-PWNED'}}catch(e){}</scr" +
        "ipt>",
    },
  );
  const mint = await (
    await page.request.post(`/api/p/${pid}/shares`, { data: { path: "pages/sec12-tab.html" } })
  ).json();
  expect(mint.token, "no share minted — nothing was measured").toBeTruthy();

  await page.goto(`/${pid}/pages/sec12-tab.html`);
  await alive(page, "the file view before opening the share");
  const before = page.url();

  // Exactly what the Open button does.
  const [shared] = await Promise.all([
    context.waitForEvent("page"),
    page.evaluate((u) => window.open(u, "_blank"), `/s/${mint.token}`),
  ]);
  // Prove the shared document really ran before judging what it could reach.
  await expect(shared.locator("#t")).toHaveText("shared");
  await page.waitForTimeout(1000);

  expect(page.url(), "a shared file repointed the tab that opened it").toBe(before);
  await alive(page, "the hub tab after the share ran");
});

/* The /auth/* pages as documents. They carry a single-use reset grant and, on
   the device pages, an account name — so nothing on them may be fetched from
   another host (a third party would learn the URL, and the reset token is IN
   the URL), and the grant must not ride in a link a click can leak. */
test("TestSec_AuthPages_CarryNoThirdPartySubresourcesAndLeakNoGrant", async ({ page, baseURL }) => {
  const external: string[] = [];
  page.on("request", (r) => {
    if (new URL(r.url()).origin !== new URL(baseURL!).origin) external.push(r.url());
  });

  const token = "a".repeat(32);
  for (const path of [
    "/auth/login",
    "/auth/signup",
    "/auth/reset",
    `/auth/reset/confirm?token=${token}`,
  ]) {
    await page.goto(path);
    // Prove the document rendered before asserting about it.
    await expect(page.locator(".card"), `${path} rendered nothing`).toBeVisible();
  }
  expect(external, "an /auth page loaded a third-party subresource").toEqual([]);

  // The grant sits in a hidden field posted back, never in an href a click or
  // a Referer could carry off.
  await page.goto(`/auth/reset/confirm?token=${token}`);
  await expect(page.locator("input[type=hidden][name=token]")).toHaveValue(token);
  const hrefs = await page
    .locator("a")
    .evaluateAll((as) => as.map((a) => (a as HTMLAnchorElement).href));
  expect(hrefs.filter((h) => h.includes(token)), "the reset grant appears in a link").toEqual([]);
});
