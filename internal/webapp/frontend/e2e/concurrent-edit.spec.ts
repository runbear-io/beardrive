import { test, expect, Page } from "@playwright/test";
import { login, ADMIN, MEMBER } from "./helpers";

/* Two people editing one file at the same time.

   This spec was written against the relay, where a client that lost its
   connection fell back to editing its OWN buffer — so two browsers held two
   documents and took turns overwriting each other through upload/content: no
   conflict copy, no warning, work gone. A real history feed showed it as
   versions alternating between two sizes several times a minute, and the fix
   at the time was to preserve the loser beside the winner.

   The hub holds the document now, so there is no second document to diverge
   into and nothing to preserve: concurrent typing CONVERGES. That is the
   stronger guarantee, and this asserts it directly — every character both
   people typed is in the file, and no conflict copy was needed to get it
   there.

   The conflict-copy machinery has not gone anywhere. It protects the file
   from writers that never touch a CRDT at all — an agent, the CLI, a device
   syncing — and internal/webapp/upload_ifmatch_test.go is where that lives
   now. */

const PROJECT = "zz-concurrent-edit";
let projectId = "";

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  const perm = await page.request.put(`/api/p/${projectId}/permissions`, {
    data: { default: "write" },
  });
  expect(perm.ok()).toBeTruthy();
  return projectId;
}

/* Named to sort last and removed afterwards, for the reason edit-url.spec.ts
   spells out: these specs share one hub, and a project left behind is in the
   project list of every spec that sorts later — the landing page picks the
   first one. */
test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

test("two editors of one file converge instead of overwriting", async ({ browser }) => {
  test.setTimeout(90_000);
  const a = await (await browser.newContext()).newPage();
  await login(a, ADMIN);
  const id = await project(a);
  const file = `concurrent-${Date.now()}.md`;
  await a.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  const b = await (await browser.newContext()).newPage();
  await login(b, MEMBER);

  for (const pg of [a, b]) {
    await pg.goto(`/${id}/edit/${file}`);
    await pg.waitForSelector(".cm-host .cm-content");
    await pg.locator(".cm-host .cm-line").first().click();
    await pg.keyboard.press("End");
  }

  // Interleaved, so each one's save lands while the other is mid-sentence —
  // which is precisely when a wholesale write destroys something.
  for (let i = 0; i < 6; i++) {
    await a.keyboard.type("A");
    await b.keyboard.type("B");
    await a.waitForTimeout(400); // shorter than the 700ms idle save
  }
  await a.waitForTimeout(4_000);

  const read = async (p: string) =>
    (await a.request.get(`/api/p/${id}/file?path=${encodeURIComponent(p)}`)).text();

  const feed = await (
    await a.request.get(`/api/p/${id}/history?prefix=&n=100`)
  ).json();
  const paths: string[] = [
    ...new Set(feed.entries.map((e: { path: string }) => e.path)),
  ];

  // Everything both people typed is in THE FILE. Not split across a winner
  // and a conflict copy — in one document, which is what a shared document
  // is for.
  const text = await read(file);
  expect(text, `A's characters are missing from ${file}`).toContain("AAAAAA");
  expect(text, `B's characters are missing from ${file}`).toContain("BBBBBB");

  // And nothing had to be parked to achieve it.
  const copies = paths.filter((p) => p.includes(".bdrive-conflict-"));
  expect(copies, `converged text still produced ${copies.join(", ")}`).toHaveLength(0);
});

/* The ordinary case must stay ordinary: one person editing alone keeps
   saving over their own work, and never manufactures a conflict copy out of
   their own previous version. */
test("editing alone never produces a conflict copy", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `solo-${Date.now()}.md`;
  await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  for (let i = 0; i < 4; i++) {
    await page.keyboard.type(` round${i}`);
    await expect(page.locator("#editor-state")).toHaveAttribute("data-state", "clean");
  }

  /* Scoped to THIS file, and filtered here rather than by the API: ?prefix=
     is a FOLDER prefix (history.go), so passing a file name returns nothing
     and every assertion below it would pass without measuring anything. The
     spec above deliberately leaves a conflict copy in this same project, so
     the scoping is not optional either. */
  const feed = await (
    await page.request.get(`/api/p/${id}/history?prefix=&n=100`)
  ).json();
  const mine = [
    ...new Set(feed.entries.map((e: { path: string }) => e.path)),
  ].filter((p) => (p as string).startsWith(file)) as string[];
  expect(mine.length, "this file has no history at all").toBeGreaterThan(0);
  const copies = mine.filter((p) => p.includes(".bdrive-conflict-"));
  expect(copies, `a lone editor conflicted with itself: ${copies.join(", ")}`).toHaveLength(0);
  expect(
    await (
      await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)
    ).text(),
  ).toContain("round3");
});

/* A peer's keystroke is not my reason to write the file.

   The idle-save timer was rearmed by ANY change to the shared document,
   including one that arrived from someone else. So in a room of N editors,
   700ms after the last keystroke by anyone, all N clients PUT the identical
   full body: N writes of one text, N times the bandwidth, and a History feed
   that grows N versions per quiet period. Four people in one file made it four
   of everything — which is the "realtime collab makes too many change
   histories" report, and part of the burst that had Cloud Run shedding
   requests (docs/hub-load-prd.md Stage 4).

   Only the typist writes now. Everyone else's copy is identical by
   construction — that is what a CRDT is — and the hub snapshots the room
   itself when the last editor leaves, so nothing depends on a bystander
   saving on the typist's behalf. */
test("a bystander in a shared document does not write the file", async ({ browser }) => {
  test.setTimeout(90_000);
  const a = await (await browser.newContext()).newPage();
  await login(a, ADMIN);
  const id = await project(a);
  const file = `bystander-${Date.now()}.md`;
  await a.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  const b = await (await browser.newContext()).newPage();
  await login(b, MEMBER);

  for (const pg of [a, b]) {
    await pg.goto(`/${id}/edit/${file}`);
    await pg.waitForSelector(".cm-host .cm-content");
  }
  // Let both rooms settle, and let the seed-driven save that joining schedules
  // land before anything is counted.
  await a.waitForTimeout(3_000);

  // B is the bystander: present, in the room, never touching the keyboard.
  const bWrites: string[] = [];
  b.on("request", (r) => {
    if (r.method() === "PUT" && r.url().includes("/upload/content")) {
      bWrites.push(r.url());
    }
  });

  await a.locator(".cm-host .cm-line").first().click();
  await a.keyboard.press("End");
  for (let i = 0; i < 6; i++) {
    await a.keyboard.type("A");
    await a.waitForTimeout(300);
  }
  // Well past the 700ms idle timer, so a save that was going to happen has.
  await a.waitForTimeout(4_000);

  // A's characters really did arrive in B's document — otherwise this test
  // proves nothing about a bystander who saw the change and declined to write.
  await expect(b.locator(".cm-host .cm-content")).toContainText("AAAAAA");

  expect(
    bWrites,
    `a bystander wrote the file ${bWrites.length} time(s) because a peer typed; ` +
      `in a room of N editors that is N writes of identical text per quiet period`,
  ).toHaveLength(0);
});

/* A save refused for a stale base must be retried, not parked.

   From a real session (2026-09-23). The HAR reads:

     02:59:51  your save        -> 200, base becomes a360…
     03:00:00  a PEER saves     -> the head moves to 40d2…
     03:00:01  your next save   -> If-Match: a360… -> 409
     03:00:02  your work parked -> .bdrive-conflict-…

   The window is between reading `base` and the PUT arriving, so no amount of
   care about WHEN a peer's sha is adopted can close it — something has to
   happen after the refusal. In a room a 409 is a LOST RACE, not a
   disagreement: everyone holds one document, so the peer's bytes are already
   in our CRDT and our text contains theirs. Re-read, confirm that, rebase,
   retry once.

   Forced rather than raced. Three earlier versions of this test tried to
   provoke the 409 with timing and all three passed against the broken code,
   which is worse than having no test: the window is milliseconds wide and
   losing it silently is exactly how this shipped. The first PUT is answered
   409 outright, which is the only part that needs to be true. */
test("a save refused for a stale base is retried, not parked beside the file", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `stale-base-${Date.now()}.md`;
  await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.waitForTimeout(2_000);

  // Refuse exactly one save the way a peer winning the race would, then let
  // everything through. The retry must land on the real path.
  let refused = false;
  await page.route("**/upload/content*", async (r) => {
    const u = r.request().url();
    if (!refused && r.request().method() === "PUT" && !u.includes("bdrive-conflict")) {
      refused = true;
      await r.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({
          error: "this file changed since you last read it",
          path: file,
          sha: "0".repeat(64),
        }),
      });
      return;
    }
    await r.continue();
  });

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" TYPED");
  await page.waitForTimeout(6_000);

  expect(refused, "the 409 was never delivered, so nothing was tested").toBeTruthy();

  const feed = await (await page.request.get(`/api/p/${id}/history?prefix=&n=100`)).json();
  const copies: string[] = [
    ...new Set(
      feed.entries
        .map((e: { path: string }) => e.path)
        .filter((p: string) => p.includes(".bdrive-conflict-")),
    ),
  ];
  expect(
    copies,
    `a stale base produced ${copies.join(", ")} — a conflict copy of the file ` +
      `against itself, when the text already contained everything the hub had`,
  ).toHaveLength(0);

  const text = await (
    await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)
  ).text();
  expect(text, "the retried save never landed").toContain("TYPED");
});

/* The other direction: the FILE is ahead of us.

   Shipped the first version of the retry and four more conflict copies
   appeared within minutes, each a strict SUBSET of the file beside it:

     live:  status: active aaaasdfasdfjoais djfoi
     copy:  status: active aaa

   A peer had saved a newer state of the shared document while our save was in
   flight. Our text held nothing the file lacked, so there was nothing to
   write and nothing to preserve — and parking it produced a copy of the file
   against an older version of itself.

   Containment has two directions and only one was handled. This is the
   other. */
test("a save the file has already moved past is dropped, not parked", async ({ page }) => {
  test.setTimeout(60_000);
  await login(page, ADMIN);
  const id = await project(page);
  const file = `file-ahead-${Date.now()}.md`;
  await page.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  await page.waitForTimeout(2_000);

  /* Refuse the save, and have the hub's copy be a SUPERSET of what the editor
     is holding — which is what a peer saving a newer state looks like from
     here. The browser's own text is "BASE TYPED"; the file gets more after
     it, so the editor's version is strictly behind. */
  let refused = false;
  await page.route("**/upload/content*", async (r) => {
    const u = r.request().url();
    if (!refused && r.request().method() === "PUT" && !u.includes("bdrive-conflict")) {
      refused = true;
      // Put the ahead-version on the server for the client to re-read.
      await page.request.put(
        `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
        { data: "BASE TYPED AND-MORE-FROM-A-PEER\n" },
      );
      await r.fulfill({
        status: 409,
        contentType: "application/json",
        body: JSON.stringify({ error: "this file changed since you last read it", path: file, sha: "0".repeat(64) }),
      });
      return;
    }
    await r.continue();
  });

  await page.locator(".cm-host .cm-line").first().click();
  await page.keyboard.press("End");
  await page.keyboard.type(" TYPED");
  await page.waitForTimeout(6_000);

  expect(refused, "the 409 was never delivered, so nothing was tested").toBeTruthy();

  const feed = await (await page.request.get(`/api/p/${id}/history?prefix=&n=100`)).json();
  const copies: string[] = [
    ...new Set(
      feed.entries
        .map((e: { path: string }) => e.path)
        .filter((p: string) => p.includes(".bdrive-conflict-")),
    ),
  ];
  expect(
    copies,
    `the file had already moved past this save, yet it was parked as ` +
      `${copies.join(", ")} — a conflict copy that is a subset of the file beside it`,
  ).toHaveLength(0);

  // And the peer's newer text is what survived.
  const text = await (
    await page.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)
  ).text();
  expect(text, "the newer version was overwritten by the stale save").toContain("AND-MORE-FROM-A-PEER");
/* Nobody in a live room writes the file. The hub does.

   Every co-editor used to PUT the whole file 700ms after their own last
   keystroke, each against whatever If-Match they last saw — N writers racing
   for one file. The hub holds the document now and writes once per pause
   (ycollab.go, watchRoom), so a client that is in a live room must send NO
   upload at all: not the typist, not the bystander. The file still updates,
   because the hub did it.

   NOT YET RUN when written: Chromium could not launch on the machine. Must be
   seen to fail against the pre-change bundle before it is believed. */
test("in a live room no client writes the file, and the file still updates", async ({ browser }) => {
  test.setTimeout(90_000);
  const a = await (await browser.newContext()).newPage();
  await login(a, ADMIN);
  const id = await project(a);
  const file = `hub-writes-${Date.now()}.md`;
  await a.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`, { data: "BASE\n" });

  const b = await (await browser.newContext()).newPage();
  await login(b, MEMBER);
  for (const pg of [a, b]) {
    await pg.goto(`/${id}/edit/${file}`);
    await pg.waitForSelector(".cm-host .cm-content");
  }
  await a.waitForTimeout(3_000); // both live

  const puts: string[] = [];
  for (const [pg, who] of [[a, "A"], [b, "B"]] as const) {
    pg.on("request", (r) => {
      if (r.method() === "PUT" && r.url().includes("/upload/content")) puts.push(who);
    });
  }

  await a.locator(".cm-host .cm-line").first().click();
  await a.keyboard.press("End");
  await a.keyboard.type(" FROM-A");
  await b.locator(".cm-host .cm-line").first().click();
  await b.keyboard.press("End");
  await b.keyboard.type(" FROM-B");
  await a.waitForTimeout(6_000); // past the hub's idle window with margin

  expect(puts, `clients in a live room wrote the file themselves: ${puts.join(",")}`).toHaveLength(0);

  const text = await (await a.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)).text();
  expect(text, "the hub never wrote the room's text").toContain("FROM-A");
  expect(text, "the hub never wrote the room's text").toContain("FROM-B");
  // And the status line reflects the hub's write landing, not a local one.
  await expect(a.locator("#editor-state")).toHaveAttribute("data-state", "clean", { timeout: 10_000 });
});

/* NOT KEPT: a timing-based version of the test above.

   Three attempts tried to provoke the 409 by racing two real editors — hold
   one client's saves, have the other save in the window, type through it.
   All three passed against the broken code. The window between reading
   `base` and the PUT arriving is milliseconds wide, and a test that cannot
   lose that race is a test that cannot fail, which is worse than no test:
   it reports confidence it has not earned. The forced-409 version above
   asserts the only thing that actually needs to be true. */
