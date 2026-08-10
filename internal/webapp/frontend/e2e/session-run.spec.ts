import { test, expect } from "@playwright/test";
import { login, wikiId } from "./helpers";

/* One agent run, both halves (BEA-98). History used to show only what a run
   CHANGED; the reads lived in a daily aggregate with no session dimension and
   could not be joined to it. The seeded run reads three files and rewrites
   one of them. */

test("a run card shows what the session read as well as what it changed", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);

  const card = page.locator(".hrun").first();
  await expect(card).toBeVisible();
  // The header counts both halves now.
  await expect(card.locator(".hrun-meta")).toContainText("read 3");
  await expect(card.locator(".hrun-meta")).toContainText("changed 2");

  // The file the run read AND rewrote carries the read marker on its own row.
  const rewritten = card.locator(".hentry", { hasText: "notes/readme.md" });
  await expect(rewritten.locator(".hread")).toHaveText("read");
  // The file it created was never read, so that row has no marker.
  await expect(card.locator(".hentry", { hasText: "runbook.md" }).locator(".hread")).toHaveCount(0);

  // What it read and did not touch is its own list.
  const readOnly = card.locator(".hrun-read");
  await expect(readOnly).toHaveCount(2);
  await expect(readOnly.first()).toContainText("archive/retired-spec.md");
  await expect(readOnly.last()).toContainText("index.md");

  // Landmine 3 is on screen, not folded into a comment: a file the run read
  // and then deleted shows a write with no read, and the card says why.
  await expect(card.locator(".hrun-foot")).toHaveText(
    "Reads shown only for files the project still has.",
  );
});

test("a read-only row opens the file it names", async ({ page }) => {
  await login(page);
  const pid = await wikiId(page);
  await page.goto(`/${pid}/history`);
  await page.locator(".hrun-read", { hasText: "index.md" }).click();
  await expect(page).toHaveURL(new RegExp(`/${pid}/index.md`));
});
