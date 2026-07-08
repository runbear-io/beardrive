/* beardrive web viewer: file tree + obsidian-like markdown pane. No dependencies. */
"use strict";

const $ = (id) => document.getElementById(id);
let flatFiles = [];   // [{path, name}] for wikilink resolution
let currentPath = null;
let collapsed = new Set(); // dir paths the user collapsed

const MD_EXT = /\.(md|markdown)$/i;
const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
const TEXT_EXT = /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|html|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

/* The client is storage-blind: where files live and how uploads reach
   storage is the server's business, learned from /api/config. A hub server
   hosts many projects; volume-scoped calls go under api/p/<project-id>/. */
let serverConfig = { mode: "volume", upload: { enabled: false } };
let projects = [];
let currentProject = null; // hub mode: the selected project
let apiBase = "api/";      // volume-scoped endpoint prefix

const fileURL = (p) => apiBase + "file?path=" + encodeURIComponent(p);

async function getJSON(url) {
  const r = await fetch(url);
  if (r.status === 401) { // auth required: sign in, then come back here
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.hash);
    throw new Error("signing in…");
  }
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

/* ---- boot ---- */
async function boot() {
  try {
    serverConfig = await getJSON("api/config");
  } catch { /* non-fatal */ }
  document.title = (serverConfig.volume || "beardrive") + " — BearDrive";
  if (serverConfig.auth && serverConfig.auth.enabled) $("signout").hidden = false;
  if (serverConfig.mode === "hub") {
    await loadProjects();
    const { project, path } = parseHash();
    const proj = projects.find((x) => x.id === project) || projects[0];
    if (proj) selectProject(proj, path);
    else $("vault-name").textContent = serverConfig.volume || "BearDrive";
    setInterval(loadProjects, 30000); // pick up new projects
  } else {
    $("vault-name").textContent = serverConfig.volume || "BearDrive";
    initUpload();
    await refreshTree();
    const { path } = parseHash();
    if (path) openFile(path);
  }
  setInterval(refreshTree, 15000); // pick up synced changes
}

/* ---- hub: projects ---- */
async function loadProjects() {
  let out;
  try {
    out = await getJSON("api/projects");
  } catch { return; }
  projects = out.projects || [];
  const nav = $("projects");
  nav.hidden = false;
  nav.innerHTML = "";
  const ul = document.createElement("ul");
  for (const p of projects) {
    const li = document.createElement("li");
    const row = document.createElement("div");
    row.className = "row" + (currentProject && currentProject.id === p.id ? " active" : "");
    row.textContent = p.name;
    row.title = p.id;
    row.onclick = () => selectProject(p, null);
    li.appendChild(row);
    ul.appendChild(li);
  }
  nav.appendChild(ul);
}

function selectProject(p, path) {
  currentProject = p;
  apiBase = "api/p/" + p.id + "/";
  $("vault-name").textContent = p.name;
  document.title = p.name + " — BearDrive";
  currentPath = null;
  $("crumb").textContent = "";
  $("meta").textContent = "";
  $("download").hidden = true;
  $("content").innerHTML = `<div class="empty">Select a file from the sidebar</div>`;
  loadProjects(); // refresh active highlight
  initUpload();
  initHistory();
  updateShareButton();
  refreshTree().then(() => { if (path) openFile(path); });
  if (!path) location.hash = p.id;
}

/* Hash routing: "#<path>" in volume mode, "#<project-id>/<path>" in hub mode. */
function parseHash() {
  const h = decodeURIComponent(location.hash.slice(1));
  if (serverConfig.mode !== "hub") return { path: h };
  const slash = h.indexOf("/");
  if (slash === -1) return { project: h, path: "" };
  return { project: h.slice(0, slash), path: h.slice(slash + 1) };
}

function setHash(path) {
  location.hash = serverConfig.mode === "hub" && currentProject
    ? currentProject.id + "/" + encodeURIComponent(path)
    : encodeURIComponent(path);
}

/* ---- tree ---- */
async function refreshTree() {
  if (serverConfig.mode === "hub" && !currentProject) return;
  let root;
  try {
    root = await getJSON(apiBase + "tree");
  } catch { return; } // keep the last good tree
  flatFiles = [];
  const nav = $("tree");
  nav.innerHTML = "";
  nav.appendChild(renderChildren(root.children || []));
  markActive();
}

function renderChildren(children) {
  const ul = document.createElement("ul");
  for (const n of children) ul.appendChild(renderNode(n));
  return ul;
}

function renderNode(n) {
  const li = document.createElement("li");
  li.className = n.dir ? "dir" : "file";
  const row = document.createElement("div");
  row.className = "row";
  row.dataset.path = n.path;
  const chev = document.createElement("span");
  chev.className = "chev";
  chev.textContent = "▾"; // ▾
  const label = document.createElement("span");
  label.textContent = n.name;
  row.append(chev, label);
  li.appendChild(row);
  if (n.dir) {
    if (serverConfig.mode === "hub") {
      const hist = document.createElement("span");
      hist.className = "dir-history";
      hist.textContent = "⌚";
      hist.title = "Folder history";
      hist.onclick = (e) => { e.stopPropagation(); showHistory({ prefix: n.path + "/" }); };
      row.appendChild(hist);
    }
    li.appendChild(renderChildren(n.children || []));
    if (collapsed.has(n.path)) li.classList.add("collapsed");
    row.onclick = () => {
      li.classList.toggle("collapsed");
      li.classList.contains("collapsed") ? collapsed.add(n.path) : collapsed.delete(n.path);
    };
  } else {
    flatFiles.push({ path: n.path, name: n.name });
    row.onclick = () => openFile(n.path);
  }
  return li;
}

function markActive() {
  for (const el of document.querySelectorAll("#tree .row.active")) el.classList.remove("active");
  if (!currentPath) return;
  const row = document.querySelector(`#tree .row[data-path="${CSS.escape(currentPath)}"]`);
  if (row) row.classList.add("active");
}

/* ---- file pane ---- */
async function openFile(p) {
  currentPath = p;
  setHash(p);
  markActive();
  $("crumb").textContent = p.split("/").join(" / ");
  updateShareButton();
  const dl = $("download");
  dl.href = apiBase + "download?path=" + encodeURIComponent(p);
  dl.hidden = false;
  const content = $("content");
  content.innerHTML = "";
  $("meta").textContent = "";
  try {
    if (MD_EXT.test(p)) {
      const doc = await getJSON(apiBase + "render?path=" + encodeURIComponent(p));
      content.innerHTML = doc.html;
      fixLinks(content, p);
      showMeta(doc);
    } else if (IMG_EXT.test(p)) {
      const img = document.createElement("img");
      img.src = fileURL(p);
      img.alt = p;
      content.appendChild(img);
    } else if (TEXT_EXT.test(p)) {
      const r = await fetch(fileURL(p));
      if (!r.ok) throw new Error(await r.text());
      const pre = document.createElement("pre");
      pre.className = "plain";
      pre.textContent = await r.text();
      content.appendChild(pre);
    } else {
      content.innerHTML =
        `<div class="filecard"><div class="name"></div>` +
        `<p>No preview for this file type.</p>` +
        `<a class="btn" download href="${dl.href}">Download</a></div>`;
      content.querySelector(".name").textContent = p.split("/").pop();
    }
  } catch (err) {
    content.innerHTML = `<div class="empty"></div>`;
    content.querySelector(".empty").textContent = "Could not load file: " + err.message;
  }
}

function showMeta(doc) {
  const parts = [];
  if (doc.author) parts.push(doc.author + (doc.device ? " on " + doc.device : ""));
  if (doc.time) parts.push(new Date(doc.time).toLocaleString());
  $("meta").textContent = parts.join(" · ");
}

/* Rewrite rendered-markdown links: wiki: targets resolve by basename, and
   relative links/images resolve against the current file's folder. */
function fixLinks(scope, p) {
  const dir = p.includes("/") ? p.slice(0, p.lastIndexOf("/")) : "";
  for (const img of scope.querySelectorAll("img")) {
    const src = img.getAttribute("src") || "";
    if (!/^([a-z]+:|\/)/i.test(src)) img.src = fileURL(join(dir, src));
  }
  for (const a of scope.querySelectorAll("a")) {
    const href = a.getAttribute("href") || "";
    if (href.startsWith("wiki:")) {
      const target = decodeURIComponent(href.slice(5));
      a.onclick = (e) => { e.preventDefault(); openWikilink(target); };
    } else if (!/^([a-z]+:|\/|#)/i.test(href)) {
      const target = join(dir, decodeURIComponent(href));
      a.onclick = (e) => { e.preventDefault(); openFile(target); };
    } else if (/^https?:/i.test(href)) {
      a.target = "_blank";
      a.rel = "noopener";
    }
  }
}

function openWikilink(target) {
  const want = target.toLowerCase();
  const hit =
    flatFiles.find((f) => f.path.toLowerCase() === want || f.path.toLowerCase() === want + ".md") ||
    flatFiles.find((f) => {
      const n = f.name.toLowerCase();
      return n === want || n === want + ".md";
    });
  if (hit) openFile(hit.path);
}

function join(dir, rel) {
  const parts = (dir ? dir.split("/") : []).concat(rel.split("/"));
  const out = [];
  for (const s of parts) {
    if (s === "" || s === ".") continue;
    if (s === "..") out.pop();
    else out.push(s);
  }
  return out.join("/");
}

/* ---- share ----
   Mint a public URL for the open file: anyone with the link can view it
   (rendered, sandboxed), no account needed. Always the latest content. */
function updateShareButton() {
  const btn = $("share-btn");
  btn.hidden = !(serverConfig.mode === "hub" && currentProject && currentPath);
  btn.onclick = async () => {
    try {
      const r = await fetch(apiBase + "shares", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ path: currentPath }),
      });
      if (!r.ok) throw new Error(await r.text());
      const share = await r.json();
      let copied = "";
      if (navigator.clipboard) {
        try { await navigator.clipboard.writeText(share.url); copied = " (copied)"; } catch { /* http origin */ }
      }
      $("meta").innerHTML = "";
      const a = document.createElement("a");
      a.href = share.url;
      a.target = "_blank";
      a.textContent = share.url;
      $("meta").append("public link: ", a, copied);
    } catch (err) {
      $("meta").textContent = "Share failed: " + err.message;
    }
  };
}

/* ---- history ----
   Every change ever made, straight from the journals: who (account), when,
   from which device (name, OS, IP as the server saw it), with view/download
   of that exact version. Groundwork for revert/rollback. */
function initHistory() {
  const btn = $("history-btn");
  btn.hidden = !(serverConfig.mode === "hub" && currentProjectOrNull());
  btn.onclick = () => showHistory(currentPath ? { path: currentPath } : { prefix: "" });
}

function currentProjectOrNull() {
  return serverConfig.mode === "hub" ? currentProject : null;
}

async function showHistory(q) {
  const content = $("content");
  const qs = "path" in q
    ? "path=" + encodeURIComponent(q.path)
    : "prefix=" + encodeURIComponent(q.prefix);
  let out;
  try {
    out = await getJSON(apiBase + "history?" + qs + "&n=200");
  } catch (err) {
    $("meta").textContent = "History unavailable: " + err.message;
    return;
  }
  const title = "path" in q ? q.path : (q.prefix ? q.prefix + " (folder)" : "all changes");
  $("crumb").textContent = "History — " + title;
  $("meta").textContent = "";
  $("download").hidden = true;
  content.innerHTML = "";
  const wrap = document.createElement("div");
  wrap.className = "history";
  if (!out.entries || out.entries.length === 0) {
    wrap.innerHTML = `<div class="empty">No history yet.</div>`;
  }
  for (const e of out.entries || []) {
    const row = document.createElement("div");
    row.className = "hentry " + e.kind;
    const who = e.user_name ? `${e.user_name} <${e.user}>` : (e.user || e.author || "unknown");
    const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
    const when = new Date(e.time).toLocaleString();
    row.innerHTML =
      `<div class="hline"><span class="hkind"></span><span class="hpath"></span><span class="htime"></span></div>` +
      `<div class="hmeta"><span class="hwho"></span><span class="hdev"></span><span class="hsize"></span><span class="hact"></span></div>`;
    row.querySelector(".hkind").textContent = e.kind === "delete" ? "✕" : "●";
    row.querySelector(".hpath").textContent = e.path;
    row.querySelector(".htime").textContent = when;
    row.querySelector(".hwho").textContent = who;
    row.querySelector(".hdev").textContent = dev;
    row.querySelector(".hsize").textContent = e.size ? humanSize(e.size) : "";
    if (e.kind !== "delete" && e.blob) {
      const view = document.createElement("a");
      view.textContent = "view";
      view.href = apiBase + "blob?sha=" + e.blob + "&name=" + encodeURIComponent(e.path);
      view.target = "_blank";
      const dl = document.createElement("a");
      dl.textContent = "download";
      dl.href = view.href + "&download=1";
      row.querySelector(".hact").append(view, " ", dl);
    }
    const p = e.path;
    row.querySelector(".hpath").onclick = () => showHistory({ path: p });
    wrap.appendChild(row);
  }
  content.appendChild(wrap);
}

function humanSize(n) {
  if (n < 1024) return n + " B";
  const units = ["KB", "MB", "GB", "TB"];
  let i = -1;
  do { n /= 1024; i++; } while (n >= 1024 && i < units.length - 1);
  return n.toFixed(1) + " " + units[i];
}

/* ---- upload ----
   The client asks the server how to upload (upload/init): "direct" hands
   back a short-lived presigned URL and the bytes go straight to the object
   store; "server" means relay the bytes through the bdrive server. */
function initUpload() {
  const btn = $("upload-btn");
  const enabled = serverConfig.upload && serverConfig.upload.enabled &&
    (serverConfig.mode !== "hub" || currentProject);
  if (!enabled) { btn.hidden = true; return; }
  btn.hidden = false;
  const input = $("upload-input");
  btn.onclick = () => input.click();
  input.onchange = async () => {
    const file = input.files[0];
    input.value = "";
    if (!file) return;
    const dir = currentPath && currentPath.includes("/")
      ? currentPath.slice(0, currentPath.lastIndexOf("/")) : "";
    const dest = dir ? dir + "/" + file.name : file.name;
    const status = $("meta");
    try {
      status.textContent = `Uploading ${dest}…`;
      await uploadFile(dest, file);
      status.textContent = `Uploaded ${dest}`;
      await refreshTree();
      openFile(dest);
    } catch (err) {
      status.textContent = "Upload failed: " + err.message;
    }
  };
}

async function uploadFile(dest, file) {
  const buf = await file.arrayBuffer();
  const sha = await sha256Hex(buf);
  const post = async (url, body) => {
    const r = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body),
    });
    if (!r.ok) throw new Error(await r.text());
    return r.json();
  };
  const req = { path: dest, sha256: sha, size: file.size };
  const plan = await post(apiBase + "upload/init", req);
  if (plan.mode === "direct") {
    if (!plan.exists) { // identical content already in the store? skip the PUT
      const r = await fetch(plan.url, { method: plan.method || "PUT", headers: plan.headers || {}, body: buf });
      if (!r.ok) throw new Error("storage upload failed: " + r.status);
    }
    await post(apiBase + "upload/commit", req);
  } else {
    const r = await fetch(apiBase + "upload/content?path=" + encodeURIComponent(dest), {
      method: "PUT", body: buf,
    });
    if (!r.ok) throw new Error(await r.text());
  }
}

async function sha256Hex(buf) {
  if (crypto.subtle) {
    const d = await crypto.subtle.digest("SHA-256", buf);
    return [...new Uint8Array(d)].map((b) => b.toString(16).padStart(2, "0")).join("");
  }
  return sha256Fallback(new Uint8Array(buf)); // plain-http origins have no crypto.subtle
}

/* Minimal SHA-256 (FIPS 180-4) for non-secure contexts. */
function sha256Fallback(bytes) {
  const K = new Uint32Array([
    0x428a2f98,0x71374491,0xb5c0fbcf,0xe9b5dba5,0x3956c25b,0x59f111f1,0x923f82a4,0xab1c5ed5,
    0xd807aa98,0x12835b01,0x243185be,0x550c7dc3,0x72be5d74,0x80deb1fe,0x9bdc06a7,0xc19bf174,
    0xe49b69c1,0xefbe4786,0x0fc19dc6,0x240ca1cc,0x2de92c6f,0x4a7484aa,0x5cb0a9dc,0x76f988da,
    0x983e5152,0xa831c66d,0xb00327c8,0xbf597fc7,0xc6e00bf3,0xd5a79147,0x06ca6351,0x14292967,
    0x27b70a85,0x2e1b2138,0x4d2c6dfc,0x53380d13,0x650a7354,0x766a0abb,0x81c2c92e,0x92722c85,
    0xa2bfe8a1,0xa81a664b,0xc24b8b70,0xc76c51a3,0xd192e819,0xd6990624,0xf40e3585,0x106aa070,
    0x19a4c116,0x1e376c08,0x2748774c,0x34b0bcb5,0x391c0cb3,0x4ed8aa4a,0x5b9cca4f,0x682e6ff3,
    0x748f82ee,0x78a5636f,0x84c87814,0x8cc70208,0x90befffa,0xa4506ceb,0xbef9a3f7,0xc67178f2]);
  const H = new Uint32Array([
    0x6a09e667,0xbb67ae85,0x3c6ef372,0xa54ff53a,0x510e527f,0x9b05688c,0x1f83d9ab,0x5be0cd19]);
  const rr = (x, n) => (x >>> n) | (x << (32 - n));
  const len = bytes.length;
  const padded = new Uint8Array((((len + 8) >> 6) + 1) << 6);
  padded.set(bytes);
  padded[len] = 0x80;
  const dv = new DataView(padded.buffer);
  dv.setUint32(padded.length - 8, Math.floor((len * 8) / 0x100000000));
  dv.setUint32(padded.length - 4, (len * 8) >>> 0);
  const w = new Uint32Array(64);
  for (let off = 0; off < padded.length; off += 64) {
    for (let i = 0; i < 16; i++) w[i] = dv.getUint32(off + i * 4);
    for (let i = 16; i < 64; i++) {
      const s0 = rr(w[i-15],7) ^ rr(w[i-15],18) ^ (w[i-15] >>> 3);
      const s1 = rr(w[i-2],17) ^ rr(w[i-2],19) ^ (w[i-2] >>> 10);
      w[i] = (w[i-16] + s0 + w[i-7] + s1) >>> 0;
    }
    let [a,b,c,d,e,f,g,h] = H;
    for (let i = 0; i < 64; i++) {
      const S1 = rr(e,6) ^ rr(e,11) ^ rr(e,25);
      const t1 = (h + S1 + ((e & f) ^ (~e & g)) + K[i] + w[i]) >>> 0;
      const S0 = rr(a,2) ^ rr(a,13) ^ rr(a,22);
      const t2 = (S0 + ((a & b) ^ (a & c) ^ (b & c))) >>> 0;
      h=g; g=f; f=e; e=(d+t1)>>>0; d=c; c=b; b=a; a=(t1+t2)>>>0;
    }
    H[0]+=a; H[1]+=b; H[2]+=c; H[3]+=d; H[4]+=e; H[5]+=f; H[6]+=g; H[7]+=h;
  }
  return [...H].map((x) => (x >>> 0).toString(16).padStart(8, "0")).join("");
}

window.addEventListener("hashchange", () => {
  const { project, path } = parseHash();
  if (serverConfig.mode === "hub" && project && (!currentProject || currentProject.id !== project)) {
    const proj = projects.find((x) => x.id === project);
    if (proj) { selectProject(proj, path); return; }
  }
  if (path && path !== currentPath) openFile(path);
});

boot();
