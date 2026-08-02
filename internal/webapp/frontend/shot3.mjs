import { chromium } from "@playwright/test";
const dir = process.argv[2];
const browser = await chromium.launch();

// desktop: the three picker states, cropped to the dialog
const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
await page.goto("http://localhost:8993/");
await page.waitForURL(/auth\/login/);
await page.fill('input[name="email"]', "e2e@example.com");
await page.fill('input[name="password"]', "e2e-pass-1");
await page.click("form button");
await page.waitForSelector("#sidebar");
await page.click("#projects .nav-add");
await page.waitForSelector(".modal-input");
await page.fill(".modal-input", "team-wiki");
const modal = page.locator(".modal");
for (const [file, label] of [
  ["template-select-empty", "Empty project"],
  ["template-select-docs", "Docs + decision records"],
  ["template-select-para", "PARA"],
]) {
  await page.click(`.start-point:has-text("${label}")`);
  await page.waitForTimeout(250);
  await modal.screenshot({ path: `${dir}/${file}.png` });
}
// full-window "after" shot of the dialog, default state
await page.click('.start-point:has-text("Empty project")');
await page.waitForTimeout(200);
await page.fill(".modal-input", "");
await page.screenshot({ path: `${dir}/new-project-dialog-after.png` });
await page.close();

// mobile
const m = await browser.newPage({ viewport: { width: 375, height: 812 } });
await m.goto("http://localhost:8993/");
await m.waitForSelector("#sidebar");
const burger = m.locator("#sb-toggle, .sb-toggle, [aria-label='Menu']").first();
if (await burger.count()) await burger.click();
await m.waitForTimeout(300);
await m.click("#projects .nav-add");
await m.waitForSelector(".modal-input");
await m.waitForTimeout(400);
await m.screenshot({ path: `${dir}/new-project-dialog-mobile-after.png` });
console.log("overflow:", await m.evaluate(() => document.documentElement.scrollWidth > document.documentElement.clientWidth));
await browser.close();
