import { chromium } from "@playwright/test";
const OUT = process.argv[2], B = "http://localhost:8993";
const b = await chromium.launch();
async function as(email) {
  const c = await b.newContext({ viewport: { width: 1280, height: 860 } });
  const p = await c.newPage();
  await p.goto(B + "/"); await p.waitForURL(/auth\/login/);
  await p.fill('input[name="email"]', email); await p.fill('input[name="password"]', "e2e-pass-1");
  await p.click("form button"); await p.waitForSelector("#sidebar");
  return p;
}
const r = await as("reader@example.com");
const pid = (await (await r.request.get(B+"/api/projects")).json()).projects.find(x=>x.name==="wiki").id;
await r.goto(`${B}/${pid}/dashboard`); await r.waitForTimeout(1200);
console.log("reader crumb:", await r.locator("#crumb").innerText());
console.log("reader treemap count:", await r.locator(".in-treemap").count());
const m = await as("member@example.com");
await m.goto(`${B}/${pid}/notes`); await m.waitForTimeout(800);
await m.click("#more-btn"); await m.waitForTimeout(300);
console.log("member ⋯ items:", await m.locator("#more-menu .more-item").allInnerTexts());
await m.screenshot({ path: `${OUT}/after-05-member-more-menu.png` });
const res = await m.request.get(`${B}/api/p/${pid}/heat`);
const body = await res.text();
console.log("member /heat:", res.status(), "identity leak:", /@|token|device_id/.test(body));
await b.close();
