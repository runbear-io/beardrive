import { test, expect } from "@playwright/test";
import { login, wikiId, MEMBER, PASSWORD, expectToast } from "./helpers";

// Phase 1: shell, session flags, project list/selection, routing, empty
// state, invite accept. Mutating specs (project creation) run last —
// specs share one seeded hub per run.

test("landing selects the first project and rewrites the URL", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.waitForURL("/" + pid);
  await expect(page.locator("#project-select")).toContainText("wiki");
  await expect(page).toHaveTitle("wiki — BearDrive");
  await expect(page.locator("#vault-name")).toHaveText("BearDrive");
});

test("deep link to a project resolves after reload", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto("/" + pid);
  await expect(page.locator("#project-select")).toContainText("wiki");
  await expect(page).toHaveURL("/" + pid);
});

test("unknown project id falls back to a real project", async ({ page }) => {
  await login(page);
  await page.goto("/p-00000000");
  await page.waitForURL(/\/[0-9a-f-]{36}$/);
  await expect(page.locator("#project-select")).toContainText(/.+/);
});

test("account menu: admin gets hub admin entry; member does not", async ({ page, browser }) => {
  await login(page); // admin, owner of "default"
  await page.click("#account-btn");
  await expect(page.locator("#menu-org-settings")).toContainText("default");
  await expect(page.locator("#menu-hub-admin")).toBeVisible();
  await expect(page.locator("#signout")).toBeVisible();
  await page.keyboard.press("Escape");

  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await login(p2, MEMBER);
  await p2.click("#account-btn");
  await expect(p2.locator("#menu-org-settings")).toContainText("default");
  await expect(p2.locator("#menu-hub-admin")).toHaveCount(0);
  await ctx.close();
});

test("the star ask is a plain link in the sidebar, not a prompt", async ({ page }) => {
  await login(page);
  const star = page.locator("#accountbar .gh-star");
  await expect(star).toBeVisible();
  await expect(star).toHaveAttribute("href", "https://github.com/runbear-io/beardrive");
  await expect(star).toHaveAttribute("target", "_blank");
  // Nothing may interrupt: no dialog, no toast, no dismissible banner.
  await expect(page.locator('[role="dialog"], [role="alertdialog"]')).toHaveCount(0);
});

test("join link accepts an invite after sign-in", async ({ page, browser }) => {
  await login(page); // admin mints the invite
  const orgs = await (await page.request.get("/api/orgs")).json();
  const org = orgs.orgs.find((o: { name: string }) => o.name === "default");
  const inv = await (
    await page.request.post(`/api/orgs/${org.id}/invites`, { data: {} })
  ).json();
  expect(inv.url).toContain("/join/");
  const token = inv.url.split("/join/")[1];

  // A signed-out visitor keeps the token through the login redirect.
  const ctx = await browser.newContext();
  const p2 = await ctx.newPage();
  await p2.goto("/join/" + token);
  await p2.waitForURL(/auth\/login/);
  await p2.fill('input[name="email"]', MEMBER);
  await p2.fill('input[name="password"]', PASSWORD);
  await p2.click("button[type=submit]");
  await expectToast(p2, "you joined");
  await p2.waitForURL(/\/[0-9a-f-]{36}$/); // lands on the org's project
  await ctx.close();
});

test("no projects: the create dialog opens itself, and the page behind is no dead end", async ({
  page,
}) => {
  await login(page, "solo@example.com");
  // With nothing to browse, the one useful action opens on arrival.
  await expect(page.locator(".modal .start-points")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".modal-input")).toHaveCount(0);

  // Closing it leaves a page that says what to do — and a way back in.
  await expect(page.locator(".onboard h1")).toHaveText("Welcome to BearDrive");
  await expect(page.locator(".ob-start h3")).toHaveText("Start a project");
  await page.click("#ob-new");
  await expect(page.locator(".modal-input")).toBeVisible();
  await page.keyboard.press("Escape");
  // Dismissed once, it stays dismissed until asked for again.
  await expect(page.locator(".modal-input")).toHaveCount(0);

  // The agent paste-prompt is still the other path, with this hub's real
  // origin filled in; the by-hand route is a docs link.
  await expect(page.locator(".ob-agent h3")).toHaveText("Or let your agent do it");
  await expect(page.locator(".onboard .gd-code code")).toContainText(
    "to set up a new BearDrive project on http://localhost:8993. Ask me which folder to sync.",
  );
  await expect(page.locator(".ob-alt a")).toHaveAttribute(
    "href",
    "https://docs.beardrive.ai/manual/setup-by-hand/",
  );
});

// An account that already has projects must not get the dialog thrown at it.
test("the create dialog does not open itself when projects exist", async ({ page }) => {
  await login(page);
  await expect(page.locator("#project-select")).toBeVisible();
  await expect(page.locator(".modal-input")).toHaveCount(0);
});

test("new project via the sidebar + modal seeds the recommended option", async ({ page }) => {
  await login(page);
  await page.click("#projects .nav-add");
  await page.fill(".modal-input", "scratch");
  // The badged row IS the checked row — asserted as an invariant, not by
  // name, so reordering the options can't quietly split them apart again.
  await expect(page.locator(".start-points")).toBeVisible();
  const on = page.locator(".start-point.on");
  const rec = page.locator(".start-point:has(.sp-rec)");
  await expect(on).toHaveCount(1);
  await expect(rec).toHaveCount(1);
  expect(await on.textContent()).toBe(await rec.textContent());
  await page.click(".modal .pbtn");
  await page.waitForURL(/\/[0-9a-f-]{36}$/);
  await expect(page.locator("#project-select")).toContainText("scratch");
  // Creating without touching the radios seeds that template. Asserted on the
  // file tree, not #content: a brand-new project's dashboard paints no
  // treemap cells (#93).
  for (const name of ["docs", "decisions", "AGENTS.md"]) {
    await expect(page.locator("#sidebar").getByText(name, { exact: true }).first()).toBeVisible();
  }
  // Open the switcher: both projects listed; picking one navigates.
  await page.click("#project-select");
  await expect(page.getByRole("option", { name: "wiki" })).toBeVisible();
  await page.getByRole("option", { name: "wiki" }).click();
  await page.waitForURL(/\/[0-9a-f-]{36}$/);
  await expect(page.locator("#project-select")).toContainText("wiki");
  await expectToast(page, "Created");
});

// Picking a template seeds the project on the hub, so the folder listing
// shows the structure before any device has ever connected.
test("new project from a template", async ({ page }) => {
  await login(page);
  await page.click("#projects .nav-add");
  await page.fill(".modal-input", "from-template");
  await page.click('.start-point:has-text("Docs + decision records")');
  await expect(page.locator(".start-point.on")).toContainText("Docs + decision records");
  await page.click(".modal .pbtn");
  await page.waitForURL(/\/[0-9a-f-]{36}$/);
  await expect(page.locator("#project-select")).toContainText("from-template");
  // Asserted on the file tree, not #content: a brand-new project's dashboard
  // deliberately paints no treemap cells (#93), so #content is not where a
  // seeded file reliably shows up.
  for (const name of ["docs", "decisions", "AGENTS.md"]) {
    await expect(page.locator("#sidebar").getByText(name, { exact: true }).first()).toBeVisible();
  }
});

// "I already have a folder" creates the same empty project as "Empty
// project" — the browser cannot touch your disk — so what it must change is
// the next screen: the paste prompt stops telling the agent to make a new
// folder, and the reassurance appears. The intent rides in the URL, so it
// survives a reload.
test("new project from an existing folder", async ({ page }) => {
  await login(page);
  await page.click("#projects .nav-add");
  await page.fill(".modal-input", "brought-my-own");
  await page.click('.start-point:has-text("I already have a folder")');
  await page.click(".modal .pbtn");
  await page.waitForURL(/connect=existing/);
  await expect(page.locator(".gd-note")).toContainText("never moves, renames or overwrites");
  await expect(page.locator(".gd-code code").first()).toContainText(
    "I already have a folder of notes — ask me which one to sync",
  );
  // Nothing was seeded: same artifact as an empty project. Checked on the
  // file tree, not #content — the paste prompt names INSTALL_FOR_AGENTS.md,
  // which a substring match on "AGENTS.md" happily finds.
  await expect(page.locator("#sidebar").getByText("AGENTS.md", { exact: true })).toHaveCount(0);
  await page.reload();
  await expect(page.locator(".gd-note")).toBeVisible();
});

test("account menu closes on Escape and outside click", async ({ page }) => {
  await login(page);
  await page.click("#account-btn");
  await expect(page.locator("#account-menu")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator("#account-menu")).toHaveCount(0);
  await page.click("#account-btn");
  await expect(page.locator("#account-menu")).toBeVisible();
  await page.click("#content", { position: { x: 10, y: 10 } });
  await expect(page.locator("#account-menu")).toHaveCount(0);
});

test("new-project modal cancels on Escape", async ({ page }) => {
  await login(page);
  await wikiId(page);
  await page.click("#projects .nav-add");
  await expect(page.locator(".modal-input")).toBeVisible();
  await page.keyboard.press("Escape");
  await expect(page.locator(".modal-input")).toHaveCount(0);
});

// BEA-75. Landing on "/" used to open whatever project sorts first, so a
// bookmark or a new tab threw away wherever you actually were. Both specs
// create or read state, so they sit at the end of the file with the other
// mutating ones.
test("landing returns to the last project opened, not the first one", async ({ page }) => {
  await login(page);
  // Named to sort last, so it can never be projects[0] — that is what makes
  // the assertion mean anything.
  const made = (await (await page.request.post("/api/projects", { data: { name: "zz-last" } })).json())
    .project;
  await page.goto("/" + made.id);
  await expect(page.locator("#project-select")).toContainText("zz-last");
  await page.goto("/"); // no project in the URL, the way a bookmark arrives
  await page.waitForURL("/" + made.id);
  await expect(page.locator("#project-select")).toContainText("zz-last");
});

test("a remembered project that is gone falls back silently", async ({ page }) => {
  await login(page);
  const errors: Error[] = [];
  page.on("pageerror", (e) => errors.push(e));
  await page.addInitScript(() =>
    localStorage.setItem("bdrive.lastProject", "00000000-0000-0000-0000-000000000000"),
  );
  await page.goto("/");
  await page.waitForURL(/\/[0-9a-f-]{36}$/);
  await expect(page.locator("#project-select")).toContainText(/.+/);
  await expect(page.locator("#toast.show, [data-sonner-toast]")).toHaveCount(0);
  expect(errors).toEqual([]);
});
