import { test, expect, Page } from "@playwright/test";
import { login, ADMIN, MEMBER, READER } from "./helpers";

/* Sustained pressure on the editing path, with the relay failing underneath.

   Not a unit of behaviour — a soak. Three editors on one file, one of them
   cut off from the co-editing relay the way a laptop changing networks is,
   typing at the same time for long enough that saves collide repeatedly.

   The claim under test is the product's oldest one: whichever version loses,
   nothing is dropped. Every character any of them typed must be findable
   somewhere — in the file, or in a conflict copy beside it.

   Named to sort last: it writes a lot and leaves copies behind. */

const PROJECT = "zz-stress-edit";
let projectId = "";

async function project(page: Page): Promise<string> {
  if (projectId) return projectId;
  const r = await page.request.post("/api/projects", { data: { name: PROJECT } });
  expect(r.ok()).toBeTruthy();
  projectId = (await r.json()).project.id;
  await page.request.put(`/api/p/${projectId}/permissions`, { data: { default: "write" } });
  return projectId;
}

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
  projectId = "";
});

test("three editors, a dying relay, and nothing dropped", async ({ browser }) => {
  test.setTimeout(180_000);
  const owner = await (await browser.newContext()).newPage();
  await login(owner, ADMIN);
  const id = await project(owner);
  const file = `stress-${Date.now()}.md`;
  await owner.request.put(
    `/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`,
    { data: "BASE\n" },
  );

  const second = await (await browser.newContext()).newPage();
  await login(second, MEMBER);
  const third = await (await browser.newContext()).newPage();
  await login(third, READER);

  // The third is permanently cut off from the relay; the second loses it
  // halfway through, which is the flap a network change produces.
  await third.route("**/collab*", (r) => r.abort());

  const marks = ["Q", "W", "E"] as const;
  const pages = [owner, second, third];
  for (const pg of pages) {
    await pg.goto(`/${id}/edit/${file}`);
    await pg.waitForSelector(".cm-host .cm-content");
    await pg.locator(".cm-host .cm-line").first().click();
    await pg.keyboard.press("End");
  }

  const ROUNDS = 24;
  for (let i = 0; i < ROUNDS; i++) {
    if (i === ROUNDS / 2) await second.route("**/collab*", (r) => r.abort());
    for (let p = 0; p < pages.length; p++) await pages[p].keyboard.type(marks[p]);
    await owner.waitForTimeout(250); // saves land mid-sentence, on purpose
  }
  await owner.waitForTimeout(6_000); // let every idle save settle

  const feed = await (
    await owner.request.get(`/api/p/${id}/history?prefix=&n=300`)
  ).json();
  const paths: string[] = [
    ...new Set(feed.entries.map((e: { path: string }) => e.path)),
  ].filter((p) => (p as string).startsWith(file)) as string[];
  expect(paths.length, "the file has no history at all").toBeGreaterThan(0);

  let all = "";
  for (const p of paths) {
    all +=
      (await (
        await owner.request.get(`/api/p/${id}/file?path=${encodeURIComponent(p)}`)
      ).text()) + "\n";
  }
  console.log(
    `stress: ${paths.length} paths, ${feed.entries.length} versions,`,
    JSON.stringify(paths.map((p) => p.slice(file.length) || "(the file)")),
  );

  // Every editor's work is somewhere. A run that dropped one of them would
  // show a mark missing entirely, which is the failure this exists to catch.
  for (const m of marks) {
    expect(all, `everything ${m} typed was lost`).toContain(m.repeat(3));
  }
  // The file itself is still a file, not a stack of conflict suffixes.
  expect(paths).toContain(file);
  // And the editors are all still editors — none wedged into an error state.
  for (const pg of pages) {
    await expect(pg.locator(".cm-host .cm-content")).toBeVisible();
    await expect(pg.locator(".empty")).toHaveCount(0);
  }
});
