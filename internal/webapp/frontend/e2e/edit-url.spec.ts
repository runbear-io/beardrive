import { test, expect, Page } from "@playwright/test";
import { login, ADMIN, MEMBER } from "./helpers";

/* The editor has a URL, and that URL is an invitation.

   Everyone with one file open is in the same co-editing room, so the only
   thing standing between "I am editing this" and "join me" was that edit mode
   lived in component state: the address bar still said the read page, and
   whatever you pasted opened your teammate somewhere else. The assertion that
   matters here is the last one — a second account, given nothing but the URL,
   lands in the same document and sees the typing.

   Everything happens in a project of this spec's own, for the reason
   inline-edit.spec.ts spells out: these specs share one hub, and a file left
   in the seeded project changes the timing of every other spec. */
const PROJECT = "edit-url-e2e";
let projectId = "";

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  // The invitation test needs the guest to be able to type.
  const perm = await page.request.put(`/api/p/${projectId}/permissions`, {
    data: { default: "write" },
  });
  expect(perm.ok()).toBeTruthy();
  return projectId;
}

const litter: (() => Promise<void>)[] = [];

async function ownFile(page: Page, id: string): Promise<string> {
  const name = `notes-${Date.now()}.md`;
  const r = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(name)}`,
    { data: "# Notes\n\nfirst line.\n", headers: { "Content-Type": "text/plain; charset=utf-8" } },
  );
  expect(r.ok()).toBeTruthy();
  litter.push(async () => {
    await page.request.post(`/api/p/${id}/remove`, { data: { path: name } }).catch(() => {});
  });
  return name;
}

test.afterEach(async () => {
  while (litter.length) {
    try {
      await litter.pop()!();
    } catch {
      /* the hub outlives the test either way */
    }
  }
});

/* The project goes too, and that is not tidiness.

   The specs share one hub AND run in file order, so a project left behind by
   a spec whose name sorts early is in the project list of every spec that
   sorts later — and the landing page picks the FIRST project. Leaving this one
   in place failed eight specs in home.spec.ts and hub.spec.ts, none of which
   have anything to do with editing: they landed in "edit-url-e2e" instead of
   the project they seeded. (inline-edit.spec.ts leaves its own project behind
   and gets away with it only because "inline-edit" sorts after both.) */
test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

test("Edit moves the file under /edit/, and Done brings it back", async ({ page }) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/${file}`);
  await page.click("#edit-btn");
  await page.waitForSelector(".cm-host .cm-content");
  await expect(page).toHaveURL(new RegExp(`/${id}/edit/${file}$`));
  await expect(page).toHaveTitle(/· Editing/);

  // The whole point of a URL: it survives a reload and Back.
  await page.reload();
  await expect(page.locator(".cm-host .cm-content")).toBeVisible();

  await page.click("#edit-btn");
  await expect(page).toHaveURL(new RegExp(`/${id}/${file}$`));
  await expect(page.locator(".cm-host .cm-content")).toHaveCount(0);
});

test("fullscreen does not close the editor", async ({ page }) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.click("#full-btn");
  await expect(page).toHaveURL(new RegExp(`/${id}/edit/${file}\\?full=1$`));
  await expect(page.locator(".cm-host .cm-content")).toBeVisible();
});

/* The caret is drawn by the browser, not by CodeMirror: this editor loads no
   drawSelection(), so .cm-cursor never exists and CodeMirror's base theme
   falls back to its LIGHT default — caret-color: black, on a near-black page.
   The caret was invisible, which is a hard bug to even report. */
test("the caret is visible against the app's dark background", async ({ page }) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  const seen = await page.evaluate(() => {
    const el = document.querySelector(".cm-host .cm-content")!;
    const root = getComputedStyle(document.documentElement);
    return {
      caret: getComputedStyle(el).caretColor,
      text: getComputedStyle(el).color,
      bg: root.getPropertyValue("--bg").trim(),
    };
  });
  // It is the text colour, which is the one colour guaranteed to be readable
  // on this background — asserting "not black" alone would pass for grey.
  expect(seen.caret).toBe(seen.text);
});

test("the URL is the invitation: a teammate lands in the same document", async ({
  page,
  browser,
}) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");

  // A second account, handed nothing but the URL.
  const guestCtx = await browser.newContext();
  const guest = await guestCtx.newPage();
  await login(guest, MEMBER);
  await guest.goto(`/${id}/edit/${file}`);
  await expect(guest.locator(".cm-host .cm-content")).toBeVisible();

  await page.click(".cm-host .cm-content");
  await page.keyboard.type("INVITED ");
  await expect(guest.locator(".cm-host .cm-content")).toContainText("INVITED", {
    timeout: 10_000,
  });
  await guestCtx.close();
});

/* An agent writing the file while somebody has it open.

   The editor still never re-seeds itself from the server — that would reset
   the document under a cursor. The write arrives as a splice instead, which
   is why the assertion is about what is ON SCREEN and not about the file. */
test("an outside write lands in the open editor", async ({ page }) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  // A cursor parked at the end of the body line, BEFORE the write arrives.
  await page.locator(".cm-host .cm-line", { hasText: "first line." }).click();
  await page.keyboard.press("End");

  // The same PUT a syncing device makes, from outside this browser's editor.
  // It rewrites the line ABOVE the cursor, so a document that was re-seeded
  // rather than spliced would drop the caret back to the top.
  const r = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "# Notes (rewritten by an agent)\n\nfirst line.\n" },
  );
  expect(r.ok()).toBeTruthy();

  const buf = page.locator(".cm-host .cm-content");
  await expect(buf).toContainText("rewritten by an agent");
  // Folded in, so there is nothing to warn about.
  await expect(page.locator("#peer-wrote")).toHaveCount(0);

  // The splice is the claim: everything it did not change is untouched, and
  // that includes where this account was typing.
  await page.keyboard.type("STILL HERE");
  await expect(buf).toContainText("first line.STILL HERE");
});

/* ...and the case it must refuse. Half a sentence in the buffer is work
   nothing may overwrite, so the outside write stays out and is announced. */
test("an outside write never overwrites unsaved typing", async ({ page }) => {
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  // The editor's own save can never land, so the buffer stays genuinely
  // unsaved for the whole test instead of racing the 700ms idle timer.
  // Routes do not apply to page.request, so the outside write still goes.
  await page.route("**/upload/content*", (route) => route.abort());
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.type("MID-SENTENCE");

  const r = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "# Notes\n\nfirst line, rewritten by an agent.\n" },
  );
  expect(r.ok()).toBeTruthy();

  await expect(page.locator("#peer-wrote")).toBeVisible();
  const buf = page.locator(".cm-host .cm-content");
  await expect(buf).toContainText("MID-SENTENCE");
  await expect(buf).not.toContainText("rewritten by an agent");
});

/* One failed read must not end an editing session.

   This is the failure a real user hit: the hub's rate limiter answered 429 on
   the file route, the editor's query gave up (retry: false), and EditView
   rendered "Could not open … for editing" INSTEAD of the editor — unmounting
   a buffer that was perfectly intact, along with its save timer and its
   co-editing stream. Only a reload brought it back. */
test("a failing read leaves the editor, and the work, alone", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-line", { hasText: "first line." }).click();
  await page.keyboard.press("End");
  await page.keyboard.type(" SURVIVES");
  await expect(page.locator("#editor-state")).toHaveAttribute("data-state", "clean");

  // Every read of this file now fails the way the rate limiter failed it.
  // Routes do not apply to page.request, so the write below still goes.
  await page.route("**/file?path=*", (route) =>
    route.fulfill({ status: 429, body: "Rate exceeded." }),
  );
  // An outside write is what asks the editor to re-read — and that re-read is
  // the one that now 429s, through every retry.
  const r = await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "# Notes\n\nrewritten while the reads were failing.\n" },
  );
  expect(r.ok()).toBeTruthy();

  await expect(page.locator("#read-stale")).toBeVisible({ timeout: 30_000 });
  // The whole point: still an editor, still holding the typed text.
  const buf = page.locator(".cm-host .cm-content");
  await expect(buf).toBeVisible();
  await expect(buf).toContainText("SURVIVES");
  await expect(page.locator(".empty")).toHaveCount(0);

  // And it is still a working editor once the hub recovers. The write that
  // landed while the reads were failing makes this buffer stale, so the save
  // is preserved BESIDE the file rather than on top of it — see
  // concurrent-edit.spec.ts. Either way it is written down somewhere, which
  // is the whole claim.
  await page.unroute("**/file?path=*");
  await page.keyboard.type(" AND SAVES");
  await expect(page.locator("#editor-state")).toHaveAttribute("data-state", "clean");
  // ?prefix= is a FOLDER prefix (history.go), so a file name matches nothing
  // there — ask for the whole project and pick this file's paths out of it.
  const feed = await (
    await page.request.get(`/api/p/${id}/history?prefix=&n=100`)
  ).json();
  const paths: string[] = [
    ...new Set(feed.entries.map((e: { path: string }) => e.path)),
  ].filter((p) => (p as string).startsWith(file)) as string[];
  expect(paths.length, "the file has no history at all").toBeGreaterThan(0);
  let found = "";
  for (const p of paths) {
    const body = await (
      await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(p)}`)
    ).text();
    if (body.includes("AND SAVES")) found = p;
  }
  expect(found, `"AND SAVES" is nowhere: ${paths.join(", ")}`).toBeTruthy();
});

/* Your own save must never look like somebody else's.

   The hub announces a write the moment it journals it, which is BEFORE that
   write's own response gets back — so the change frame, and the refetch it
   triggers, routinely beat the PUT home. `saved` only moves when the response
   lands, so for that window the editor's own text matched neither what it had
   saved nor the buffer it had typed further into, and the peer banner went up
   over the user's own keystrokes (the hazard #230 walked into from the other
   side). */
test("a save still in flight is not mistaken for a teammate", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = await ownFile(page, id);

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");

  /* Two delays, and both are the point.

     The PUT reaches the hub — so it journals and announces — and then its
     response is held, which is what keeps `saved` behind. The re-READ that
     the announcement triggers is held too, so it lands AFTER the next
     keystroke rather than before it.

     Without that second delay the refetch arrives while the buffer still
     equals the text just sent, merge answers "same" for an entirely
     different reason, and the test passes against the bug. It did. */
  await page.route("**/upload/content*", async (route) => {
    const res = await route.fetch();
    await new Promise((r) => setTimeout(r, 4_000));
    await route.fulfill({ response: res });
  });
  await page.route("**/file?path=*", async (route) => {
    const res = await route.fetch();
    await new Promise((r) => setTimeout(r, 1_500));
    await route.fulfill({ response: res });
  });

  await page.locator(".cm-host .cm-line", { hasText: "first line." }).click();
  await page.keyboard.press("End");
  await page.keyboard.type(" ONE");
  await page.waitForTimeout(900); // just past the 700ms idle save

  /* Watch the whole window, do not sample its end.

     Once the held response finally lands, `saved` catches up and the next
     merge says "same" — which CLEARS the banner. Checking after the fact
     therefore passes against the bug: the wrong answer was on screen for two
     seconds and gone by the time anyone looked. This resolves the moment it
     appears. */
  const banner = page
    .locator("#peer-wrote")
    .waitFor({ state: "visible", timeout: 4_000 })
    .then(() => true)
    .catch(() => false);

  await page.keyboard.type(" TWO"); // typed before the held re-read lands
  expect(await banner, "the editor blamed a teammate for this account's own save").toBe(false);
  await expect(page.locator(".cm-host .cm-content")).toContainText("ONE TWO");
});
