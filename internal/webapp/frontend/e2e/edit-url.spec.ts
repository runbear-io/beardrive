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
