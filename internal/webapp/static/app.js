/* sfs web viewer: file tree + obsidian-like markdown pane. No dependencies. */
"use strict";

const $ = (id) => document.getElementById(id);
let flatFiles = [];   // [{path, name}] for wikilink resolution
let currentPath = null;
let collapsed = new Set(); // dir paths the user collapsed

const MD_EXT = /\.(md|markdown)$/i;
const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
const TEXT_EXT = /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|html|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

const fileURL = (p) => "api/file?path=" + encodeURIComponent(p);

async function getJSON(url) {
  const r = await fetch(url);
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

/* ---- boot ---- */
async function boot() {
  try {
    const v = await getJSON("api/volume");
    $("vault-name").textContent = v.volume || "sfs";
    $("vault-remote").textContent = v.remote || "";
    document.title = (v.volume || "sfs") + " — sfs";
  } catch { /* non-fatal */ }
  await refreshTree();
  const p = decodeURIComponent(location.hash.slice(1));
  if (p) openFile(p);
  setInterval(refreshTree, 15000); // pick up synced changes
}

/* ---- tree ---- */
async function refreshTree() {
  let root;
  try {
    root = await getJSON("api/tree");
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
  location.hash = encodeURIComponent(p);
  markActive();
  $("crumb").textContent = p.split("/").join(" / ");
  const dl = $("download");
  dl.href = "api/download?path=" + encodeURIComponent(p);
  dl.hidden = false;
  const content = $("content");
  content.innerHTML = "";
  $("meta").textContent = "";
  try {
    if (MD_EXT.test(p)) {
      const doc = await getJSON("api/render?path=" + encodeURIComponent(p));
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

window.addEventListener("hashchange", () => {
  const p = decodeURIComponent(location.hash.slice(1));
  if (p && p !== currentPath) openFile(p);
});

boot();
