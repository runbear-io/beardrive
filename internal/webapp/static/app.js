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
let orgs = [];             // hub mode: the orgs this account belongs to
let joinedOrgId = null;    // org just joined via an invite this page-load

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

async function postJSON(url, body) {
  const r = await fetch(url, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(body || {}),
  });
  if (r.status === 401) {
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
  document.title = serverConfig.brand || serverConfig.volume || "BearDrive";
  // If auth is on and we're not signed in, go straight to the login page
  // rather than firing authed API calls that 401 (noisy console, and the
  // redirect happens anyway). /api/config reports `me` only when signed in.
  if (serverConfig.auth && serverConfig.auth.enabled && !serverConfig.me) {
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.hash);
    return;
  }
  if (serverConfig.auth && serverConfig.auth.enabled) $("signout").hidden = false;
  if (serverConfig.mode === "hub") {
    await acceptInviteFromHash();
    await loadOrgs();
    await loadProjects();
    updateAdminBar();
    const { project, path } = parseHash();
    // After accepting an invite, open a project in the org you just joined
    // rather than whatever happened to be first.
    const proj = projects.find((x) => x.id === project)
      || (joinedOrgId && projects.find((x) => x.org === joinedOrgId))
      || projects[0];
    if (proj) selectProject(proj, path);
    else { $("vault-name").textContent = serverConfig.brand || serverConfig.volume || "BearDrive"; updateOrgBar(); showEmptyState(); }
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
  const head = document.createElement("div");
  head.className = "nav-head";
  head.innerHTML = `<span>Projects</span>`;
  const add = document.createElement("button");
  add.className = "nav-add";
  add.title = "New project";
  add.textContent = "+";
  add.onclick = () => {
    const name = prompt("New project name:");
    if (name) createProject(name.trim());
  };
  head.appendChild(add);
  nav.appendChild(head);
  const ul = document.createElement("ul");
  for (const p of projects) {
    const li = document.createElement("li");
    const row = document.createElement("div");
    row.className = "row" + (currentProject && currentProject.id === p.id ? " active" : "");
    row.textContent = p.name;
    row.title = p.name;
    row.tabIndex = 0;
    row.setAttribute("role", "button");
    row.onclick = () => selectProject(p, null);
    row.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); row.click(); } };
    ul.appendChild(li).appendChild(row);
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
  $("content").className = "view";
  $("content").innerHTML = `<div class="empty">Select a file to read it.<br><span class="empty-hint">On a phone, tap ☰ to browse.</span></div>`;
  loadProjects(); // refresh active highlight
  updateOrgBar();
  initUpload();
  initHistory();
  updateShareButton();
  refreshTree().then(() => { if (path) openFile(path); });
  if (!path) location.hash = p.id;
}

/* ---- hub: organizations ---- */

/* Opening "#join/<token>" joins the invite's org. If the visitor isn't
   signed in yet, postJSON's 401 handler sends them to /auth/login with the
   #join hash intact in `next`, so after signing in they land right back
   here and the join completes — the token is never lost. */
async function acceptInviteFromHash() {
  const m = location.hash.match(/^#join\/([0-9a-f]+)$/);
  if (!m) return;
  try {
    const out = await postJSON("api/invites/" + m[1]); // may redirect to login (401)
    location.hash = "";
    joinedOrgId = out.org && out.org.id;
    toast("Welcome — you joined the “" + out.org.name + "” team. Opening its projects…");
  } catch (e) {
    if (String(e.message).includes("signing in")) throw e; // redirecting; stop boot
    location.hash = "";
    toast("Could not accept the invite: " + e.message, true);
  }
}

/* Onboarding: a signed-in account with no projects shouldn't hit a blank
   sidebar. Explain that access comes from an invite, let them paste one,
   and — since any member can — offer to start a new project. */
function showEmptyState() {
  $("orgbar").hidden = true;
  $("content").className = "view";
  const auth = serverConfig.auth && serverConfig.auth.enabled;
  $("content").innerHTML = `
    <div class="onboard">
      <h1>Welcome to BearDrive</h1>
      <p>You're signed in, but you're not part of any project yet.</p>
      ${auth ? `
      <div class="ob-card">
        <h3>Have an invite link?</h3>
        <p>A teammate can send you a join link. Paste it here:</p>
        <div class="ob-row">
          <input id="ob-invite" type="text" placeholder="https://…/#join/…" autocomplete="off">
          <button id="ob-join" class="pbtn">Join</button>
        </div>
      </div>` : ``}
      <div class="ob-card">
        <h3>Or start a new project</h3>
        <p>Create a shared space for your team's files.</p>
        <div class="ob-row">
          <input id="ob-name" type="text" placeholder="Project name, e.g. wiki" autocomplete="off">
          <button id="ob-create" class="pbtn">Create</button>
        </div>
      </div>
    </div>`;
  const join = $("ob-join");
  if (join) join.onclick = () => {
    const v = $("ob-invite").value.trim();
    const m = v.match(/#join\/([0-9a-f]+)/) || v.match(/^([0-9a-f]{8,})$/);
    if (!m) { toast("That doesn't look like an invite link.", true); return; }
    location.hash = "join/" + m[1];
    location.reload();
  };
  $("ob-create").onclick = () => createProject($("ob-name").value.trim());
}

async function createProject(name) {
  if (!name) { toast("Give the project a name.", true); return; }
  try {
    const out = await postJSON("api/projects", { name });
    await loadOrgs();
    await loadProjects();
    selectProject(out.project, null);
    toast("Created “" + out.project.name + "”.");
  } catch (e) {
    toast("Could not create the project: " + e.message, true);
  }
}

async function loadOrgs() {
  try {
    orgs = (await getJSON("api/orgs")).orgs || [];
  } catch { orgs = []; }
}

function currentOrg() {
  if (!currentProject || !currentProject.org) return null;
  return orgs.find((o) => o.id === currentProject.org) || null;
}

/* The sidebar footer names the project's org; clicking it lists members,
   and owners get an Invite button that mints a join link. */
function updateOrgBar() {
  const bar = $("orgbar"), org = currentOrg();
  // The top-of-sidebar gear is the always-visible admin entry point: any
  // account that owns an org (or is a hub admin) gets it, whatever project
  // is open.
  const owned = orgs.find((o) => o.role === "owner");
  const gear = $("settings-btn");
  if (gear) {
    const target = (org && org.role === "owner") ? org : owned;
    gear.hidden = !target;
    gear.onclick = () => showOrgAdmin(target);
  }
  if (!org) { bar.hidden = true; return; }
  bar.hidden = false;
  const nm = $("org-name");
  nm.textContent = org.name;
  nm.title = "Manage organization";
  nm.onclick = () => showOrgAdmin(org);
  nm.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); nm.click(); } };
  const btn = $("invite-btn");
  btn.hidden = org.role !== "owner";
  btn.textContent = "Manage";
  btn.onclick = () => showOrgAdmin(org);
}

/* The org admin panel: members (owners can change roles / remove), rename,
   invite links (create / revoke), and an org-wide audit of public shares. */
async function showOrgAdmin(org) {
  currentPath = null;
  markActive();
  closeSidebarOnMobile();
  $("crumb").textContent = org.name;
  $("meta").textContent = "";
  $("share-btn").hidden = $("history-btn").hidden = $("download").hidden = $("more-btn").hidden = true;
  const owner = org.role === "owner";
  const box = $("content");
  box.className = "view";
  box.innerHTML = `<div class="admin"><h1 id="org-title"></h1></div>`;
  box.querySelector("#org-title").textContent = org.name + (owner ? "" : "  ·  member");
  const panel = box.querySelector(".admin");

  if (owner) {
    const rn = el(panel, "div", "admin-row");
    rn.innerHTML = `<input id="org-rename" type="text" value=""><button class="pbtn" id="org-rename-btn">Rename org</button>`;
    rn.querySelector("#org-rename").value = org.name;
    rn.querySelector("#org-rename-btn").onclick = async () => {
      const name = rn.querySelector("#org-rename").value.trim();
      try { await api("PATCH", "api/orgs/" + org.id, { name }); toast("Renamed."); await loadOrgs(); refreshAll(); }
      catch (e) { toast(e.message, true); }
    };
  }

  // Members
  el(panel, "h3", null, "Members");
  const mlist = el(panel, "div", "admin-list");
  const myEmail = (serverConfig.me && serverConfig.me.email) || "";
  for (const m of org.members) {
    const row = el(mlist, "div", "admin-item");
    const isSelf = myEmail && m.email.toLowerCase() === myEmail.toLowerCase();
    el(row, "span", "ai-main", m.email + (isSelf ? "  (you)" : ""));
    if (owner && !isSelf) {
      const sel = document.createElement("select");
      for (const r of ["owner", "member"]) {
        const o = document.createElement("option"); o.value = r; o.textContent = r;
        if (m.role === r) o.selected = true; sel.appendChild(o);
      }
      sel.onchange = async () => {
        try { await api("PATCH", "api/orgs/" + org.id + "/members/" + encodeURIComponent(m.email), { role: sel.value }); toast("Role updated."); await loadOrgs(); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); showOrgAdmin(currentOrg()); }
      };
      row.appendChild(sel);
      const rm = el(row, "button", "ai-del", "Remove");
      rm.onclick = async () => {
        if (!confirm("Remove " + m.email + " from " + org.name + "?")) return;
        try { await api("DELETE", "api/orgs/" + org.id + "/members/" + encodeURIComponent(m.email)); toast("Removed."); await loadOrgs(); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); }
      };
    } else {
      el(row, "span", "ai-tag", m.role);
    }
  }

  if (!owner) return;

  // Projects in this org (rename / delete)
  el(panel, "h3", null, "Projects");
  const plist = el(panel, "div", "admin-list");
  const orgProjects = projects.filter((p) => p.org === org.id);
  if (!orgProjects.length) el(plist, "div", "admin-empty", "No projects yet.");
  for (const p of orgProjects) {
    const row = el(plist, "div", "admin-item");
    el(row, "span", "ai-main", p.name);
    const rn = el(row, "button", "ai-btn", "Rename");
    rn.onclick = async () => {
      const name = prompt("Rename project:", p.name);
      if (!name || name.trim() === p.name) return;
      try { await api("PATCH", "api/projects/" + p.id, { name: name.trim() }); toast("Renamed."); await loadProjects(); showOrgAdmin(currentOrg()); }
      catch (e) { toast(e.message, true); }
    };
    const del = el(row, "button", "ai-del", "Delete");
    del.onclick = async () => {
      if (!confirm("Delete project “" + p.name + "”? Its files stay in storage but it's removed from the hub.")) return;
      try {
        await api("DELETE", "api/projects/" + p.id);
        toast("Deleted “" + p.name + "”.");
        if (currentProject && currentProject.id === p.id) currentProject = null;
        await loadProjects();
        const next = currentOrg();
        if (next) showOrgAdmin(next); else showEmptyState();
      } catch (e) { toast(e.message, true); }
    };
  }

  // Invites
  const ih = el(panel, "div", "admin-h");
  el(ih, "h3", null, "Invite links");
  const mk = el(ih, "button", "pbtn", "New invite");
  mk.onclick = async () => {
    try {
      const out = await postJSON("api/orgs/" + org.id + "/invites");
      const ok = await copyText(out.url);
      toast(ok ? "Invite link copied to clipboard." : "Invite created — copy it from the list below.");
      showOrgAdmin(currentOrg());
    } catch (e) { toast(e.message, true); }
  };
  const ilist = el(panel, "div", "admin-list");
  try {
    const invs = (await getJSON("api/orgs/" + org.id + "/invites")).invites || [];
    if (!invs.length) el(ilist, "div", "admin-empty", "No active invite links.");
    for (const inv of invs) {
      const row = el(ilist, "div", "admin-item");
      const main = el(row, "span", "ai-main mono", inv.url);
      main.style.cursor = "pointer";
      main.title = "Copy";
      main.onclick = () => copyText(inv.url).then((ok) => toast(ok ? "Copied." : "Select and copy the link."));
      const meta = (inv.creator ? "by " + inv.creator + " · " : "") +
        (inv.uses ? inv.uses + " joined · " : "unused · ") +
        "expires " + new Date(inv.expires).toLocaleDateString();
      el(row, "span", "ai-tag", meta);
      const rv = el(row, "button", "ai-del", "Revoke");
      rv.onclick = async () => {
        if (!confirm("Revoke this invite link? Anyone still holding it won't be able to join.")) return;
        try { await api("DELETE", "api/orgs/" + org.id + "/invites/" + inv.token); toast("Revoked."); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); }
      };
    }
  } catch { /* ignore */ }

  // Org-wide share audit
  el(panel, "h3", null, "Public share links");
  const slist = el(panel, "div", "admin-list");
  try {
    const shs = (await getJSON("api/orgs/" + org.id + "/shares")).shares || [];
    if (!shs.length) el(slist, "div", "admin-empty", "No public shares.");
    for (const sh of shs) {
      const row = el(slist, "div", "admin-item");
      const main = el(row, "span", "ai-main mono", sh.path);
      main.title = sh.url; main.style.cursor = "pointer";
      main.onclick = () => window.open(sh.url, "_blank");
      const meta = (sh.project_name || "") +
        (sh.creator ? " · by " + sh.creator : "") +
        (sh.created ? " · " + new Date(sh.created).toLocaleDateString() : "");
      el(row, "span", "ai-tag", meta);
      const rv = el(row, "button", "ai-del", "Revoke");
      rv.onclick = async () => {
        if (!confirm("Revoke the public link to “" + sh.path + "”? Anyone with the URL will lose access.")) return;
        try { await api("DELETE", "api/shares/" + sh.token); toast("Share revoked."); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); }
      };
    }
  } catch { /* ignore */ }
}

/* small DOM helper */
function el(parent, tag, cls, text) {
  const n = document.createElement(tag);
  if (cls) n.className = cls;
  if (text != null) n.textContent = text;
  parent.appendChild(n);
  return n;
}

/* fetch wrapper for methods without a body-returning helper */
async function api(method, url, body) {
  const opt = { method };
  if (body !== undefined) { opt.headers = { "Content-Type": "application/json" }; opt.body = JSON.stringify(body); }
  const r = await fetch(url, opt);
  if (!r.ok) throw new Error(await r.text());
  return r.status === 204 ? {} : r.json();
}

/* clipboard copy that never throws on a non-HTTPS origin (where
   navigator.clipboard is undefined). Returns true on success. */
async function copyText(text) {
  try {
    if (navigator.clipboard) { await navigator.clipboard.writeText(text); return true; }
  } catch { /* fall through */ }
  return false;
}

/* transient toast, replacing blocking alert() */
let toastTimer = null;
function toast(msg, isErr) {
  let t = $("toast");
  if (!t) { t = document.createElement("div"); t.id = "toast"; document.body.appendChild(t); }
  t.textContent = msg;
  t.className = "show" + (isErr ? " err" : "");
  clearTimeout(toastTimer);
  toastTimer = setTimeout(() => { t.className = ""; }, 3200);
}

/* Admin approval bar: a hub admin sees pending signups to approve/deny. */
async function updateAdminBar() {
  const bar = $("adminbar");
  if (!bar) return;
  if (!(serverConfig.auth && serverConfig.auth.admin)) { bar.hidden = true; return; }
  let pending = [];
  try { pending = (await getJSON("api/admin/pending")).pending || []; } catch { }
  bar.hidden = false;
  bar.textContent = pending.length ? "⚑ Admin · " + pending.length : "⚙ Admin";
  bar.title = "Hub administration — signup policy" + (pending.length ? " and pending approvals" : "");
  bar.onclick = () => showHubSettings();
}

/* Hub-admin settings: signup/access policy. Verification & approval are
   live toggles; the domain allowlist and admin list are shown read-only
   (they're server-config owned, deliberately not browser-editable). */
async function showHubSettings() {
  let pol = {};
  try { pol = await getJSON("api/admin/policy"); } catch (e) { toast(e.message, true); return; }
  currentPath = null; markActive(); closeSidebarOnMobile();
  $("crumb").textContent = "Signup & access";
  $("share-btn").hidden = $("history-btn").hidden = $("download").hidden = $("more-btn").hidden = true;
  const box = $("content");
  box.className = "view";
  box.innerHTML = `<div class="admin"><h1>Signup &amp; access</h1>
    <p class="admin-sub">Who can create an account on this hub, and how new accounts are vetted.</p></div>`;
  const panel = box.querySelector(".admin");

  el(panel, "h3", null, "New-account vetting");
  const toggles = el(panel, "div", "admin-list");
  const mkToggle = (label, desc, key, on) => {
    const row = el(toggles, "label", "admin-item toggle");
    const left = el(row, "span", "ai-main");
    el(left, "div", "tg-label", label);
    el(left, "div", "tg-desc", desc);
    const cb = document.createElement("input");
    cb.type = "checkbox"; cb.checked = !!on; cb.dataset.key = key;
    row.appendChild(cb);
    return cb;
  };
  const ver = mkToggle("Require email verification",
    pol.mailer ? "New accounts must click an emailed link before they can sign in." :
      "New accounts must verify via a link (no mailer configured — the link is written to the server log).",
    "require_verification", pol.require_verification);
  const app = mkToggle("Require admin approval",
    "New accounts wait for a hub admin to approve them before they gain access.",
    "require_approval", pol.require_approval);
  const save = el(panel, "button", "pbtn", "Save policy");
  save.style.marginTop = "14px";
  save.onclick = async () => {
    try {
      await postJSON("api/admin/policy", { require_verification: ver.checked, require_approval: app.checked });
      toast("Signup policy saved.");
    } catch (e) { toast(e.message, true); }
  };

  el(panel, "h3", null, "Who can sign up");
  const info = el(panel, "div", "admin-list");
  const dom = el(info, "div", "admin-item");
  el(dom, "span", "ai-main", "Allowed email domains");
  el(dom, "span", "ai-tag", (pol.allowed_domains && pol.allowed_domains.length) ? pol.allowed_domains.map((d) => "@" + d).join(", ") : "any (open signup)");
  const sg = el(info, "div", "admin-item");
  el(sg, "span", "ai-main", "Self-signup");
  el(sg, "span", "ai-tag", pol.allow_signup ? "open" : "closed");
  const ad = el(info, "div", "admin-item");
  el(ad, "span", "ai-main", "Hub admins");
  el(ad, "span", "ai-tag", (pol.admins && pol.admins.length) ? pol.admins.join(", ") : "none");
  el(panel, "p", "admin-sub", "Domains and admins are set in the server config file (they can't be widened from the browser).");

  // Pending approvals live here too, so this is the single admin home.
  el(panel, "h3", null, "Pending signups");
  const plist = el(panel, "div", "admin-list");
  let pending = [];
  try { pending = (await getJSON("api/admin/pending")).pending || []; } catch { }
  if (!pending.length) el(plist, "div", "admin-empty", "No one is waiting for approval.");
  for (const u of pending) {
    const row = el(plist, "div", "admin-item");
    el(row, "span", "ai-main", (u.name ? u.name + "  ·  " : "") + u.email);
    const ok = el(row, "button", "pbtn", "Approve");
    ok.onclick = async () => { try { await postJSON("api/admin/pending/" + u.id + "/approve"); toast("Approved " + u.email); updateAdminBar(); showHubSettings(); } catch (e) { toast(e.message, true); } };
    const no = el(row, "button", "ai-del", "Deny");
    no.onclick = async () => { try { await postJSON("api/admin/pending/" + u.id + "/deny"); toast("Denied " + u.email); updateAdminBar(); showHubSettings(); } catch (e) { toast(e.message, true); } };
  }
}

async function showPending() {
  let pending = [];
  try { pending = (await getJSON("api/admin/pending")).pending || []; } catch { }
  currentPath = null; markActive();
  $("crumb").textContent = "Pending signups";
  $("share-btn").hidden = $("history-btn").hidden = $("download").hidden = $("more-btn").hidden = true;
  const box = $("content");
  box.className = "view";
  box.innerHTML = `<div class="admin"><h1>Pending signups</h1></div>`;
  const panel = box.querySelector(".admin");
  const list = el(panel, "div", "admin-list");
  if (!pending.length) el(list, "div", "admin-empty", "No one is waiting for approval.");
  for (const u of pending) {
    const row = el(list, "div", "admin-item");
    el(row, "span", "ai-main", (u.name ? u.name + "  ·  " : "") + u.email);
    const ok = el(row, "button", "pbtn", "Approve");
    ok.onclick = async () => { try { await postJSON("api/admin/pending/" + u.id + "/approve"); toast("Approved " + u.email); updateAdminBar(); showPending(); } catch (e) { toast(e.message, true); } };
    const no = el(row, "button", "ai-del", "Deny");
    no.onclick = async () => { try { await postJSON("api/admin/pending/" + u.id + "/deny"); toast("Denied " + u.email); updateAdminBar(); showPending(); } catch (e) { toast(e.message, true); } };
  }
}

function refreshAll() {
  loadProjects();
  updateOrgBar();
  if (currentProject) refreshTree();
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
  label.className = "label";
  label.textContent = n.name;
  row.append(chev, label);
  // Keyboard-operable: the row behaves as a button.
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  row.title = n.name;
  row.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); row.click(); } };
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
  closeSidebarOnMobile();
  $("crumb").textContent = p.split("/").join(" / ");
  updateShareButton();
  const dl = $("download");
  dl.href = apiBase + "download?path=" + encodeURIComponent(p);
  dl.hidden = false;
  $("more-btn").hidden = false;
  const content = $("content");
  content.className = "markdown"; // document view: markdown type rules apply
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

/* A clear, explicitly-public share confirmation: warns that anyone with the
   link can view, and offers copy / open / revoke. */
function showShareDialog(url, copied) {
  const back = document.createElement("div");
  back.className = "modal-back";
  back.innerHTML = `
    <div class="modal">
      <h3>🔗 Public link created</h3>
      <p><b>Anyone with this link can view this file</b> — no account needed. It always shows the latest version until you revoke it.</p>
      <div class="modal-url"></div>
      <div class="modal-actions">
        <button class="pbtn" data-a="copy">${copied ? "Copied ✓" : "Copy link"}</button>
        <button class="ai-btn" data-a="open">Open</button>
        <button class="ai-del" data-a="revoke">Revoke</button>
        <button class="ai-btn" data-a="close">Done</button>
      </div>
    </div>`;
  back.querySelector(".modal-url").textContent = url;
  const close = () => back.remove();
  back.onclick = (e) => { if (e.target === back) close(); };
  const token = url.split("/s/")[1];
  back.querySelector('[data-a="copy"]').onclick = () => copyText(url).then((ok) => toast(ok ? "Copied." : "Select and copy the link above."));
  back.querySelector('[data-a="open"]').onclick = () => window.open(url, "_blank");
  back.querySelector('[data-a="close"]').onclick = close;
  back.querySelector('[data-a="revoke"]').onclick = async () => {
    try { await api("DELETE", "api/shares/" + token); toast("Link revoked — it no longer works."); close(); }
    catch (e) { toast(e.message, true); }
  };
  document.body.appendChild(back);
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
      const copied = await copyText(share.url);
      showShareDialog(share.url, copied);
    } catch (err) {
      toast("Share failed: " + err.message, true);
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
  content.className = "view";
  $("more-btn").hidden = true;
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
      dl.setAttribute("download", e.path.split("/").pop());
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

/* ---- command palette (⌘K / Ctrl+K) ----
   One box for everything: fuzzy-jump to any file, switch projects, and run
   quick actions (share, history, upload, download, sign out). */
let paletteItems = [];
let paletteSel = 0;

function paletteOpen() {
  buildPaletteItems("");
  $("palette-overlay").hidden = false;
  const input = $("palette-input");
  input.value = "";
  input.focus();
}

function paletteClose() {
  $("palette-overlay").hidden = true;
}

/* Subsequence fuzzy match. Returns a score (higher = better) or -1, plus
   the matched positions for highlighting. */
function fuzzy(query, text) {
  if (!query) return { score: 0, hits: [] };
  const q = query.toLowerCase();
  const t = text.toLowerCase();
  let ti = 0, score = 0, streak = 0;
  const hits = [];
  for (let qi = 0; qi < q.length; qi++) {
    const found = t.indexOf(q[qi], ti);
    if (found === -1) return null;
    streak = found === ti ? streak + 1 : 1;
    score += streak * 3;                                  // consecutive runs
    if (found === 0 || "/ -_.".includes(t[found - 1])) score += 8; // word starts
    hits.push(found);
    ti = found + 1;
  }
  score -= Math.floor(t.length / 8); // mild preference for short targets
  return { score, hits };
}

function highlight(text, hits) {
  const span = document.createElement("span");
  span.className = "plabel";
  let last = 0;
  for (const h of hits) {
    if (h > last) span.append(text.slice(last, h));
    const b = document.createElement("b");
    b.textContent = text[h];
    span.append(b);
    last = h + 1;
  }
  span.append(text.slice(last));
  return span;
}

/* Candidate sources: context-aware actions, then projects, then files. */
function paletteCandidates() {
  const items = [];
  const add = (icon, label, kind, run) => items.push({ icon, label, kind, run });
  const hub = serverConfig.mode === "hub";

  if (hub && currentProject && currentPath) {
    add("↗", "Share: " + currentPath, "action", () => $("share-btn").click());
    add("⌚", "History: " + currentPath, "action", () => showHistory({ path: currentPath }));
    add("↓", "Download: " + currentPath, "action", () => $("download").click());
  }
  if (hub && currentProject) {
    add("⌚", "History: whole project", "action", () => showHistory({ prefix: "" }));
  }
  if (!$("upload-btn").hidden) {
    add("+", "Upload a file…", "action", () => $("upload-btn").click());
  }
  if (hub) {
    for (const p of projects) {
      if (!currentProject || p.id !== currentProject.id) {
        add("▣", "Switch to project: " + p.name, "project", () => selectProject(p, null));
      }
    }
  }
  if (serverConfig.auth && serverConfig.auth.enabled) {
    add("⏻", "Sign out", "action", () => { location.href = "/auth/logout"; });
  }
  for (const f of flatFiles) {
    add("◦", f.path, "file", () => openFile(f.path));
  }
  return items;
}

/* Match a query against a label, tolerating a simple English plural so
   "ideas" still finds idea.md. Tries the raw query first, then a lightly
   de-pluralized form (…ies→…y, …es→…, …s→…). */
function fuzzyStemmed(query, label) {
  const m = fuzzy(query, label);
  if (m) return m;
  const q = query.toLowerCase();
  let stem = null;
  if (q.length > 3 && q.endsWith("ies")) stem = q.slice(0, -3) + "y";
  else if (q.length > 3 && q.endsWith("es")) stem = q.slice(0, -2);
  else if (q.length > 2 && q.endsWith("s")) stem = q.slice(0, -1);
  return stem ? fuzzy(stem, label) : null;
}

function buildPaletteItems(query) {
  const scored = [];
  for (const c of paletteCandidates()) {
    const m = fuzzyStemmed(query, c.label);
    if (m) scored.push({ ...c, score: m.score, hits: m.hits });
  }
  scored.sort((a, b) => b.score - a.score);
  paletteItems = scored.slice(0, 40);
  paletteSel = 0;
  renderPalette();
}

function renderPalette() {
  const ul = $("palette-results");
  ul.innerHTML = "";
  if (paletteItems.length === 0) {
    const li = document.createElement("li");
    li.className = "pempty";
    li.textContent = "No matches — search covers file names, projects, and actions";
    ul.appendChild(li);
    return;
  }
  paletteItems.forEach((item, i) => {
    const li = document.createElement("li");
    if (i === paletteSel) li.classList.add("selected");
    const icon = document.createElement("span");
    icon.className = "picon";
    icon.textContent = item.icon;
    const kind = document.createElement("span");
    kind.className = "pkind";
    kind.textContent = item.kind;
    li.append(icon, highlight(item.label, item.hits), kind);
    li.onclick = () => runPaletteItem(item);
    li.onmousemove = () => { if (paletteSel !== i) { paletteSel = i; renderPalette(); } };
    ul.appendChild(li);
  });
  const sel = ul.children[paletteSel];
  if (sel) sel.scrollIntoView({ block: "nearest" });
}

function runPaletteItem(item) {
  paletteClose();
  item.run();
}

window.addEventListener("keydown", (e) => {
  if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
    e.preventDefault();
    $("palette-overlay").hidden ? paletteOpen() : paletteClose();
    return;
  }
  if ($("palette-overlay").hidden) return;
  if (e.key === "Escape") {
    e.preventDefault();
    paletteClose();
  } else if (e.key === "ArrowDown" || e.key === "ArrowUp") {
    e.preventDefault();
    const n = paletteItems.length;
    if (n) {
      paletteSel = (paletteSel + (e.key === "ArrowDown" ? 1 : n - 1)) % n;
      renderPalette();
    }
  } else if (e.key === "Enter") {
    e.preventDefault();
    if (paletteItems[paletteSel]) runPaletteItem(paletteItems[paletteSel]);
  }
});

$("palette-input").addEventListener("input", (e) => buildPaletteItems(e.target.value));
$("palette-overlay").addEventListener("click", (e) => {
  if (e.target.id === "palette-overlay") paletteClose();
});

/* Visible search affordance in the top bar → opens the palette. */
$("search-btn").addEventListener("click", paletteOpen);

/* Mobile "⋯ More": the secondary file actions (History, Upload, Download)
   collapse behind one 44px button so every target stays tappable without
   the header overflowing. The menu proxies to the real buttons, so their
   behavior and visibility rules are the single source of truth. */
function buildMoreMenu() {
  const menu = $("more-menu");
  menu.innerHTML = "";
  const items = [
    ["History", $("history-btn")],
    ["Upload", $("upload-btn")],
    ["Download", $("download")],
  ].filter(([, el]) => el && !el.hidden);
  for (const [label, el] of items) {
    const b = document.createElement("button");
    b.className = "more-item";
    b.textContent = label;
    b.onclick = () => { $("more-menu").hidden = true; el.click(); };
    menu.appendChild(b);
  }
  return items.length;
}
$("more-btn").addEventListener("click", (e) => {
  e.stopPropagation();
  const menu = $("more-menu");
  if (menu.hidden) { buildMoreMenu(); menu.hidden = false; }
  else menu.hidden = true;
});
document.addEventListener("click", () => { $("more-menu").hidden = true; });

/* Mobile: the sidebar is off-canvas; a hamburger toggles it. */
function toggleSidebar() { document.body.classList.toggle("sb-open"); }
function closeSidebarOnMobile() { document.body.classList.remove("sb-open"); }
$("menu-btn").addEventListener("click", toggleSidebar);
$("sb-backdrop").addEventListener("click", closeSidebarOnMobile);

window.addEventListener("hashchange", () => {
  const { project, path } = parseHash();
  if (serverConfig.mode === "hub" && project && (!currentProject || currentProject.id !== project)) {
    const proj = projects.find((x) => x.id === project);
    if (proj) { selectProject(proj, path); return; }
  }
  if (path && path !== currentPath) openFile(path);
});

boot();
