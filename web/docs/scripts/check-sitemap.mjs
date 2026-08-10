// Check a site's sitemap the way a crawler would.
//
//   node scripts/check-sitemap.mjs https://docs.beardrive.ai
//   node scripts/check-sitemap.mjs http://localhost:4321        # npm run preview
//   node scripts/check-sitemap.mjs https://beardrive.ai https://docs.beardrive.ai
//
// Every check below is one Search Console performs, in the order it performs
// them: find the sitemap from robots.txt, follow the index, parse it, then
// confirm the URLs it advertises actually resolve. A sitemap listing 404s or
// redirects is the most common Search Console complaint, and it is invisible
// until something crawls it -- which is weeks after the deploy that broke it.
//
// No dependencies and no test framework: it runs against a live origin, which
// is the only place these can actually be wrong.

const W3C_DATETIME = /^\d{4}-\d\d-\d\d(T\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d))?$/;

// The sitemap namespace, matched loosely. Tag names are compared with the
// namespace stripped, so a document declaring the schema with a prefix still
// parses -- the point is to catch a wrong ROOT element, not to validate XML.
const tag = (xml, name) => [...xml.matchAll(new RegExp(`<${name}>(.*?)</${name}>`, "gs"))].map((m) => m[1]);

async function fetchOk(url, method = "GET") {
  const res = await fetch(url, { method, redirect: "manual" });
  return { status: res.status, type: res.headers.get("content-type") ?? "", body: method === "GET" ? await res.text() : "" };
}

async function check(origin) {
  console.log(`\n=== ${origin} ===`);
  const fail = [];

  // A built sitemap always holds absolute PRODUCTION URLs -- `site` in
  // astro.config.mjs -- even when served from localhost. So the origin the
  // sitemap claims is read from the sitemap itself, and requests are made
  // against the origin being checked. Against production the two are the same
  // and this does nothing; against a local server it is what makes the check
  // possible at all. What it does NOT paper over is a sitemap listing more
  // than one origin, which is a real misconfiguration and fails below.
  let claimed = origin;
  const here = (url) => origin + new URL(url).pathname;

  // robots.txt is how a crawler finds the sitemap without being told.
  let declared = [];
  const robots = await fetchOk(`${origin}/robots.txt`);
  if (robots.status !== 200) {
    fail.push(`robots.txt returned ${robots.status} — the sitemap is undiscoverable on this host`);
  } else {
    declared = [...robots.body.matchAll(/^\s*sitemap:\s*(\S+)/gim)].map((m) => m[1]);
    console.log(`  robots.txt          200, declares ${declared.join(", ") || "NOTHING"}`);
    if (!declared.length) fail.push("robots.txt declares no Sitemap:");
  }

  const index = await fetchOk(`${origin}/sitemap-index.xml`);
  console.log(`  sitemap-index.xml   ${index.status}  ${index.type}`);
  if (index.status !== 200) return [...fail, `sitemap-index.xml returned ${index.status}`];
  if (!index.type.includes("xml")) fail.push(`index served as ${index.type}, not XML`);
  if (!/<sitemapindex[\s>]/.test(index.body)) fail.push("index root is not <sitemapindex>");

  const children = tag(index.body, "loc");
  if (children[0]) {
    claimed = new URL(children[0]).origin;
    if (claimed !== origin) console.log(`  serving             ${claimed} (checked at ${origin})`);
  }
  if (declared.length && !declared.includes(`${claimed}/sitemap-index.xml`)) {
    fail.push(`robots.txt declares ${declared.join(", ")}, not ${claimed}/sitemap-index.xml`);
  }

  const locs = [];
  let dated = 0;
  for (const child of children) {
    if (new URL(child).origin !== claimed) {
      fail.push(`index mixes origins: ${child} is not on ${claimed}`);
      continue;
    }
    const doc = await fetchOk(here(child));
    console.log(`  ${child.split("/").pop().padEnd(19)} ${doc.status}  ${doc.type}`);
    if (doc.status !== 200) {
      fail.push(`index points at ${child}, which returned ${doc.status}`);
      continue;
    }
    if (!/<urlset[\s>]/.test(doc.body)) fail.push(`${child} root is not <urlset>`);
    for (const entry of tag(doc.body, "url")) {
      const [loc] = tag(entry, "loc");
      if (loc) locs.push(loc);
      const [lastmod] = tag(entry, "lastmod");
      if (lastmod !== undefined) {
        dated++;
        if (!W3C_DATETIME.test(lastmod)) fail.push(`${loc} lastmod "${lastmod}" is not a W3C datetime`);
      }
    }
  }
  console.log(`  urls                ${locs.length} (${dated} with lastmod)`);
  if (!locs.length) fail.push("the sitemap advertises no URLs");

  // Two entries for one page split its ranking signals between them.
  const dupes = [...new Set(locs.filter((l, i) => locs.indexOf(l) !== i))];
  if (dupes.length) fail.push(`duplicate <loc>: ${dupes.join(", ")}`);

  // A 3xx here is a finding, not a pass: a sitemap should list the URL a page
  // actually lives at, which is why redirect: "manual" is set above.
  let ok = 0;
  for (const loc of locs) {
    if (new URL(loc).origin !== claimed) {
      fail.push(`${loc} is not on ${claimed}`);
      continue;
    }
    const { status } = await fetchOk(here(loc), "HEAD");
    if (status === 200) ok++;
    else fail.push(`${loc} returns ${status}`);
  }
  console.log(`  resolve             ${ok}/${locs.length} return 200`);

  return fail;
}

const origins = process.argv.slice(2);
if (!origins.length) {
  console.error("usage: node scripts/check-sitemap.mjs <origin> [origin...]");
  process.exit(2);
}

const failures = [];
for (const origin of origins) {
  failures.push(...(await check(origin.replace(/\/$/, ""))).map((f) => [origin, f]));
}

console.log();
if (failures.length) {
  console.log(`FAIL — ${failures.length} problem(s):`);
  for (const [origin, f] of failures) console.log(`  [${origin}] ${f}`);
  process.exit(1);
}
console.log("PASS — sitemaps are well-formed, complete, and discoverable.");
