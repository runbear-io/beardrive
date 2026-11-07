import { test, expect, Browser, Page } from "@playwright/test";
import { login, ADMIN, MEMBER } from "./helpers";

/* A team taking meeting notes in one file.

   Several people open the same markdown file in the hub editor and type into
   it for the length of a meeting, while the ordinary interruptions happen: a
   tab reloads, a laptop closes and reopens, everyone leaves and comes back.
   Before the meeting an agent writes the day's template from outside the
   editor. What the team expects at the end is one file holding every line
   anyone typed — never an empty file, never a conflict copy.

   MEETING_SECONDS stretches the meeting; the default keeps the suite fast. */

const PROJECT = "zz-meeting-notes";
const EDITORS = Number(process.env.MEETING_EDITORS || 4);
const SECONDS = Number(process.env.MEETING_SECONDS || 60);
let projectId = "";

test.afterAll(async ({ browser }) => {
  if (!projectId) return;
  const page = await browser.newPage();
  await login(page, ADMIN);
  await page.request.delete(`/api/projects/${projectId}`).catch(() => {});
  await page.close();
});

type Seat = { who: string; page: Page | null };

async function open(browser: Browser, who: string, id: string, file: string): Promise<Page> {
  const page = await (await browser.newContext()).newPage();
  await login(page, who);
  await page.goto(`/${id}/edit/${file}`);
  await page.waitForSelector(".cm-host .cm-content");
  return page;
}

async function typeLine(page: Page, line: string) {
  await page.locator(".cm-host .cm-content").click();
  await page.keyboard.press("ControlOrMeta+End");
  await page.keyboard.press("Enter");
  await page.keyboard.type(line, { delay: 15 });
}

test("a meeting's worth of co-editing leaves one complete file", async ({ browser }) => {
  test.setTimeout((SECONDS + 120) * 1000);
  const admin = await (await browser.newContext()).newPage();
  await login(admin, ADMIN);
  const r = await admin.request.post("/api/projects", { data: { name: PROJECT } });
  projectId = (await r.json()).project.id;
  await admin.request.put(`/api/p/${projectId}/permissions`, { data: { default: "write" } });
  const id = projectId;
  const file = `daily-${Date.now()}.md`;
  const template = "# Daily meeting\n\n## Updates\n\n## Decisions\n\n## Action items\n";
  // The agent's template, written from outside the editor.
  await admin.request.put(`/api/p/${id}/upload/content?path=${encodeURIComponent(file)}`, {
    data: template,
  });

  const seats: Seat[] = [];
  for (let i = 0; i < EDITORS; i++) {
    const who = i % 2 ? MEMBER : ADMIN;
    seats.push({ who, page: await open(browser, who, id, file) });
  }

  const typed: string[] = [];
  const end = Date.now() + SECONDS * 1000;
  let n = 0;
  let everyoneLeft = false;
  while (Date.now() < end) {
    const seat = seats[n % seats.length];
    n++;
    const roll = Math.random();
    if (seat.page && roll < 0.06) {
      await seat.page.reload();
      await seat.page.waitForSelector(".cm-host .cm-content");
    } else if (seat.page && roll < 0.1) {
      await seat.page.context().close(); // the laptop lid
      seat.page = null;
    } else if (!seat.page) {
      seat.page = await open(browser, seat.who, id, file);
    } else {
      const line = `- s${n % seats.length} line ${n}`;
      await typeLine(seat.page, line);
      typed.push(line);
    }
    // Halfway through, everyone leaves at once and the room empties.
    if (!everyoneLeft && Date.now() > end - (SECONDS * 1000) / 2) {
      everyoneLeft = true;
      for (const s of seats) {
        await s.page?.context().close();
        s.page = null;
      }
      await admin.waitForTimeout(3_000);
    }
    await admin.waitForTimeout(150 + Math.random() * 400);
  }
  await admin.waitForTimeout(6_000); // let the last pause save
  for (const s of seats) await s.page?.context().close();
  await admin.waitForTimeout(4_000); // and the room's final snapshot

  const text = await (
    await admin.request.get(`/api/p/${id}/file?path=${encodeURIComponent(file)}`)
  ).text();
  const feed = await (await admin.request.get(`/api/p/${id}/history?prefix=&n=1000`)).json();
  const mine = (feed.entries as { path: string; kind: string; size?: number }[]).filter((e) =>
    e.path.startsWith(file),
  );
  const copies = [...new Set(mine.map((e) => e.path).filter((p) => p.includes(".bdrive-conflict-")))];
  const emptied = mine.filter((e) => e.path === file && e.kind !== "delete" && !e.size);
  const lost = typed.filter((l) => !text.includes(l));

  console.log(
    `meeting: ${EDITORS} editors, ${SECONDS}s, ${typed.length} lines typed, ` +
      `${mine.filter((e) => e.path === file).length} writes, ${copies.length} conflict copies, ` +
      `${emptied.length} empty writes, ${lost.length} lines lost`,
  );
  expect(text.length, "the file ended empty").toBeGreaterThan(template.length);
  expect(emptied, "the file was written empty at some point").toHaveLength(0);
  expect(copies, "co-editing produced conflict copies").toHaveLength(0);
  expect(lost, "typed lines are missing from the file").toHaveLength(0);
});
