import { expect, test } from "@playwright/test";
import { login, wikiId } from "./helpers";

// "Share my open doc with my agent" (BEA-275): off by default, announced to
// the hub only when on, remembered per browser, and handed back only to the
// same account through presence/me.
test("agent share toggle is opt-in, announces agent:true and persists", async ({ page }) => {
  await login(page);
  const id = await wikiId(page);
  await page.evaluate(() => localStorage.removeItem("bdrive.agentShare"));
  await page.goto(`/${id}/guide.md`);
  const toggle = page.locator("#agent-share");
  await expect(toggle).toHaveAttribute("aria-pressed", "false");
  expect(await (await page.request.get(`/api/p/${id}/presence/me`)).json()).toEqual({});

  const announced = page.waitForRequest(
    (r) => r.url().endsWith("/presence") && r.method() === "POST" && r.postDataJSON()?.agent === true,
  );
  await toggle.click();
  await announced;
  await expect(toggle).toHaveAttribute("aria-pressed", "true");
  await expect
    .poll(async () => (await (await page.request.get(`/api/p/${id}/presence/me`)).json()).path)
    .toBe("guide.md");

  await page.reload();
  await expect(page.locator("#agent-share")).toHaveAttribute("aria-pressed", "true");

  await page.locator("#agent-share").click();
  await expect(page.locator("#agent-share")).toHaveAttribute("aria-pressed", "false");
  await expect
    .poll(async () => await (await page.request.get(`/api/p/${id}/presence/me`)).json())
    .toEqual({});
});
