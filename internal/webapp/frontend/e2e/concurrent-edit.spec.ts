import { test, expect, Page } from "@playwright/test";
import { login, ADMIN, MEMBER } from "./helpers";

/* Two people editing one file, and one of them cannot reach the relay.

   The co-editing CRDT makes concurrent typing merge — while the room works.
   When a client loses the relay (a dropped EventSource, a laptop changing
   networks) it falls back to single-writer editing over its OWN buffer, and
   until this suite existed the two browsers simply took turns overwriting
   each other through upload/content: no conflict copy, no warning, work
   gone. A real history feed showed it as versions alternating between two
   sizes, several times a minute.

   The guarantee asserted here is the one the sync path has always made:
   whichever version loses is preserved beside the winner, never dropped. */

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

test("a relay-less editor cannot overwrite a teammate's work", async ({ browser }) => {
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
  // B never reaches the relay — what a dropped EventSource leaves behind once
  // it stops retrying. B is now editing its own buffer, alone.
  await b.route("**/collab*", (route) => route.abort());

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
  const copy = paths.find((p) => p.includes(".bdrive-conflict-"));
  expect(copy, `no conflict copy was written; paths: ${paths.join(", ")}`).toBeTruthy();

  // Neither version was dropped: one is the file, the other is beside it.
  const both = (await read(file)) + "\n" + (await read(copy!));
  expect(both).toContain("AAAAAA");
  expect(both).toContain("BBBBBB");

  // And the copy is named the way the sync path names one, so the reader
  // meets the same explanation wherever it came from (lib/conflict.ts).
  expect(copy).toMatch(
    new RegExp(`^${file}\\.bdrive-conflict-[A-Za-z0-9_-]{0,32}-\\d{8}T\\d{6}Z$`),
  );
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
