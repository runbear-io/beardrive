/* beardrive web viewer: file tree + obsidian-like markdown pane. No dependencies. */
"use strict";

const $ = (id) => document.getElementById(id);
let flatFiles = [];   // [{path, name}] for wikilink resolution
let dirIndex = new Map(); // dir path → tree node, for folder listings
let currentPath = null;
let expanded = new Set();  // dir paths that are open (folders start closed)
let treeFirstLoad = true;  // apply the "lone root folder opens" rule once per project

const MD_EXT = /\.(md|markdown)$/i;
const IMG_EXT = /\.(png|jpe?g|gif|svg|webp|ico|bmp|avif)$/i;
const TEXT_EXT = /\.(txt|log|json|ya?ml|toml|csv|go|py|js|ts|jsx|tsx|sh|bash|zsh|rb|rs|c|h|cpp|java|kt|swift|sql|html|css|xml|ini|conf|env|mod|sum|jsonl)$/i;

/* The client is storage-blind: where files live and how uploads reach
   storage is the server's business, learned from /api/config. A hub server
   hosts many projects; volume-scoped calls go under api/p/<project-id>/. */
let serverConfig = { mode: "volume", upload: { enabled: false } };
let projects = [];
let currentProject = null; // hub mode: the selected project
let apiBase = "/api/";      // volume-scoped endpoint prefix
let orgs = [];             // hub mode: the orgs this account belongs to
let joinedOrgId = null;    // org just joined via an invite this page-load
let heatMap = null;        // hub: path → 30-day read counts, from the heat API
let heatAt = 0;            // when heatMap was last fetched (ms)

const fileURL = (p) => apiBase + "file?path=" + encodeURIComponent(p);

/* inline SVG icon, pulled from the <symbol> sprite in index.html */
function svgIcon(name) { return `<svg class="ico" aria-hidden="true"><use href="#i-${name}"/></svg>`; }

/* Deterministic accent for a project's letter-mark, so each project keeps a
   stable color across reloads without any server state. */
const PROJ_COLORS = ["#5b8def", "#f5a623", "#4cc38a", "#e0679b", "#8b7bf0", "#3ec8c8", "#e6934a"];
function projColor(s) {
  let h = 0;
  for (const c of s) h = (h * 31 + c.charCodeAt(0)) >>> 0;
  return PROJ_COLORS[h % PROJ_COLORS.length];
}

async function getJSON(url) {
  const r = await fetch(url);
  if (r.status === 401) { // auth required: sign in, then come back here
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
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
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
    throw new Error("signing in…");
  }
  if (!r.ok) throw new Error(await r.text());
  return r.json();
}

/* ---- boot ---- */
async function boot() {
  try {
    serverConfig = await getJSON("/api/config");
  } catch { /* non-fatal */ }
  document.title = serverConfig.brand || serverConfig.volume || "BearDrive";
  // If auth is on and we're not signed in, go straight to the login page
  // rather than firing authed API calls that 401 (noisy console, and the
  // redirect happens anyway). /api/config reports `me` only when signed in.
  if (serverConfig.auth && serverConfig.auth.enabled && !serverConfig.me) {
    location.href = "/auth/login?next=" + encodeURIComponent(location.pathname + location.search);
    return;
  }
  if (serverConfig.auth && serverConfig.auth.enabled) $("signout").hidden = false;
  if (serverConfig.mode === "hub") {
    await acceptInviteFromURL();
    await loadOrgs();
    await loadProjects();
    updateAdminBar();
    const { project, path } = parseRoute();
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
    const { path } = parseRoute();
    if (path) openPath(path);
  }
  setInterval(refreshTree, 15000); // pick up synced changes
}

/* ---- hub: projects ---- */
async function loadProjects() {
  let out;
  try {
    out = await getJSON("/api/projects");
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
  add.onclick = async () => {
    const name = await modalPrompt("New project", "Project name", "", "Create");
    if (name) createProject(name);
  };
  head.appendChild(add);
  nav.appendChild(head);
  const ul = document.createElement("ul");
  for (const p of projects) {
    const li = document.createElement("li");
    const row = document.createElement("div");
    row.className = "row" + (currentProject && currentProject.id === p.id ? " active" : "");
    const mark = document.createElement("span");
    mark.className = "proj-mark";
    mark.style.background = projColor(p.name);
    mark.textContent = (p.name.trim()[0] || "?");
    const lab = document.createElement("span");
    lab.className = "label";
    lab.textContent = p.name;
    row.append(mark, lab);
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
  expanded = new Set();   // fresh collapse state for the new project's tree
  treeFirstLoad = true;
  heatMap = null;         // the old project's heat means nothing here
  heatAt = 0;
  apiBase = "/api/p/" + p.id + "/";
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
  refreshTree().then(() => { if (path) openPath(path); });
  if (!path) pushURL("/" + p.id);
}

/* ---- hub: organizations ---- */

/* Opening "/join/<token>" joins the invite's org. If the visitor isn't
   signed in yet, postJSON's 401 handler sends them to /auth/login with the
   /join path intact in `next`, so after signing in the server re-serves the
   app there and the join completes — the token is never lost. */
async function acceptInviteFromURL() {
  const m = location.pathname.match(/^\/join\/([0-9a-f]+)\/?$/);
  if (!m) return;
  try {
    const out = await postJSON("/api/invites/" + m[1]); // may redirect to login (401)
    history.replaceState(null, "", "/");
    joinedOrgId = out.org && out.org.id;
    toast("Welcome — you joined the “" + out.org.name + "” team. Opening its projects…");
  } catch (e) {
    if (String(e.message).includes("signing in")) throw e; // redirecting; stop boot
    history.replaceState(null, "", "/");
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
          <input id="ob-invite" type="text" placeholder="https://…/join/…" autocomplete="off">
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
    const m = v.match(/join\/([0-9a-f]+)/) || v.match(/^([0-9a-f]{8,})$/);
    if (!m) { toast("That doesn't look like an invite link.", true); return; }
    location.href = "/join/" + m[1];
  };
  $("ob-create").onclick = () => createProject($("ob-name").value.trim());
}

async function createProject(name) {
  if (!name) { toast("Give the project a name.", true); return; }
  try {
    const out = await postJSON("/api/projects", { name });
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
    orgs = (await getJSON("/api/orgs")).orgs || [];
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
      try { await api("PATCH", "/api/orgs/" + org.id, { name }); toast("Renamed."); await loadOrgs(); refreshAll(); }
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
        try { await api("PATCH", "/api/orgs/" + org.id + "/members/" + encodeURIComponent(m.email), { role: sel.value }); toast("Role updated."); await loadOrgs(); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); showOrgAdmin(currentOrg()); }
      };
      row.appendChild(sel);
      const rm = el(row, "button", "ai-del", "Remove");
      rm.onclick = async () => {
        if (!(await modalConfirm("Remove member", "Remove " + m.email + " from " + org.name + "?", "Remove", true))) return;
        try { await api("DELETE", "/api/orgs/" + org.id + "/members/" + encodeURIComponent(m.email)); toast("Removed."); await loadOrgs(); showOrgAdmin(currentOrg()); }
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
      const name = await modalPrompt("Rename project", "New name", p.name, "Rename");
      if (!name || name === p.name) return;
      try { await api("PATCH", "/api/projects/" + p.id, { name }); toast("Renamed."); await loadProjects(); showOrgAdmin(currentOrg()); }
      catch (e) { toast(e.message, true); }
    };
    const del = el(row, "button", "ai-del", "Delete");
    del.onclick = async () => {
      if (!(await modalConfirm("Delete project", "Delete “" + p.name + "”? Its files stay in storage, but it's removed from the hub.", "Delete", true))) return;
      try {
        await api("DELETE", "/api/projects/" + p.id);
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
      const out = await postJSON("/api/orgs/" + org.id + "/invites");
      const ok = await copyText(out.url);
      toast(ok ? "Invite link copied to clipboard." : "Invite created — copy it from the list below.");
      showOrgAdmin(currentOrg());
    } catch (e) { toast(e.message, true); }
  };
  const ilist = el(panel, "div", "admin-list");
  try {
    const invs = (await getJSON("/api/orgs/" + org.id + "/invites")).invites || [];
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
        if (!(await modalConfirm("Revoke invite", "Revoke this invite link? Anyone still holding it won't be able to join.", "Revoke", true))) return;
        try { await api("DELETE", "/api/orgs/" + org.id + "/invites/" + inv.token); toast("Revoked."); showOrgAdmin(currentOrg()); }
        catch (e) { toast(e.message, true); }
      };
    }
  } catch { /* ignore */ }

  // Org-wide share audit
  el(panel, "h3", null, "Public share links");
  const slist = el(panel, "div", "admin-list");
  try {
    const shs = (await getJSON("/api/orgs/" + org.id + "/shares")).shares || [];
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
        if (!(await modalConfirm("Revoke share link", "Revoke the public link to “" + sh.path + "”? Anyone with the URL will lose access.", "Revoke", true))) return;
        try { await api("DELETE", "/api/shares/" + sh.token); toast("Share revoked."); showOrgAdmin(currentOrg()); }
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

/* In-app modal prompt (text input) — replaces native prompt(). Resolves to
   the trimmed string, or null if cancelled. */
function modalPrompt(title, label, value, okLabel) {
  return new Promise((resolve) => {
    const back = document.createElement("div");
    back.className = "modal-back";
    back.innerHTML = `<div class="modal">
      <h3></h3>
      <label class="modal-label"></label>
      <input class="modal-input" type="text" autocomplete="off">
      <div class="modal-actions">
        <button class="ai-btn" data-a="cancel">Cancel</button>
        <button class="pbtn" data-a="ok"></button>
      </div></div>`;
    back.querySelector("h3").textContent = title;
    back.querySelector(".modal-label").textContent = label || "";
    const input = back.querySelector(".modal-input");
    input.value = value || "";
    back.querySelector('[data-a="ok"]').textContent = okLabel || "OK";
    const done = (v) => { back.remove(); document.removeEventListener("keydown", onKey); resolve(v); };
    const onKey = (e) => { if (e.key === "Escape") done(null); if (e.key === "Enter") done(input.value.trim() || null); };
    back.querySelector('[data-a="cancel"]').onclick = () => done(null);
    back.querySelector('[data-a="ok"]').onclick = () => done(input.value.trim() || null);
    back.onclick = (e) => { if (e.target === back) done(null); };
    document.addEventListener("keydown", onKey);
    document.body.appendChild(back);
    input.focus();
    input.select();
  });
}

/* In-app confirm — replaces native confirm(). `danger` styles the confirm
   button as destructive. Resolves true/false. */
function modalConfirm(title, message, confirmLabel, danger) {
  return new Promise((resolve) => {
    const back = document.createElement("div");
    back.className = "modal-back";
    back.innerHTML = `<div class="modal">
      <h3></h3>
      <p class="modal-msg"></p>
      <div class="modal-actions">
        <button class="ai-btn" data-a="cancel">Cancel</button>
        <button class="${danger ? "danger-btn" : "pbtn"}" data-a="ok"></button>
      </div></div>`;
    back.querySelector("h3").textContent = title;
    back.querySelector(".modal-msg").textContent = message || "";
    back.querySelector('[data-a="ok"]').textContent = confirmLabel || "Confirm";
    const done = (v) => { back.remove(); document.removeEventListener("keydown", onKey); resolve(v); };
    const onKey = (e) => { if (e.key === "Escape") done(false); if (e.key === "Enter") done(true); };
    back.querySelector('[data-a="cancel"]').onclick = () => done(false);
    back.querySelector('[data-a="ok"]').onclick = () => done(true);
    back.onclick = (e) => { if (e.target === back) done(false); };
    document.addEventListener("keydown", onKey);
    document.body.appendChild(back);
    back.querySelector('[data-a="ok"]').focus();
  });
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
  try { pending = (await getJSON("/api/admin/pending")).pending || []; } catch { }
  bar.hidden = false;
  bar.innerHTML = svgIcon("shield") + `<span>Admin${pending.length ? " · " + pending.length : ""}</span>`;
  bar.title = "Hub administration — signup policy" + (pending.length ? " and pending approvals" : "");
  bar.onclick = () => showHubSettings();
}

/* Hub-admin settings: signup/access policy. Verification & approval are
   live toggles; the domain allowlist and admin list are shown read-only
   (they're server-config owned, deliberately not browser-editable). */
async function showHubSettings() {
  let pol = {};
  try { pol = await getJSON("/api/admin/policy"); } catch (e) { toast(e.message, true); return; }
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
  const mkToggle = (label, desc, key, on, disabled) => {
    const row = el(toggles, "label", "admin-item toggle");
    const left = el(row, "span", "ai-main");
    el(left, "div", "tg-label", label);
    el(left, "div", "tg-desc", desc);
    const cb = document.createElement("input");
    cb.type = "checkbox"; cb.checked = !!on; cb.dataset.key = key;
    if (disabled) { cb.disabled = true; row.style.opacity = ".55"; }
    row.appendChild(cb);
    return cb;
  };
  const ver = mkToggle("Require email verification",
    pol.mailer ? "New accounts must click an emailed link before they can sign in — proves they control the address."
      : "Configure SMTP on the server (auth.smtp) to enable email verification.",
    "require_verification", pol.require_verification && pol.mailer, !pol.mailer);
  const app = mkToggle("Require admin approval",
    "New accounts wait for a hub admin to approve them before they gain access.",
    "require_approval", pol.require_approval);
  const save = el(panel, "button", "pbtn", "Save policy");
  save.style.marginTop = "14px";
  save.onclick = async () => {
    try {
      await postJSON("/api/admin/policy", { require_verification: ver.checked, require_approval: app.checked });
      toast("Signup policy saved.");
    } catch (e) { toast(e.message, true); }
  };

  el(panel, "h3", null, "Who can sign up");
  const info = el(panel, "div", "admin-list");
  const dom = el(info, "div", "admin-item");
  el(dom, "span", "ai-main", "Allowed email domains");
  el(dom, "span", "ai-tag", (pol.allowed_domains && pol.allowed_domains.length) ? pol.allowed_domains.map((d) => "@" + d).join(", ") : "any");
  const sg = el(info, "div", "admin-item");
  el(sg, "span", "ai-main", "Self-signup");
  el(sg, "span", "ai-tag", pol.allow_signup ? "open" : "invite-only");
  const ad = el(info, "div", "admin-item");
  el(ad, "span", "ai-main", "Hub admins");
  el(ad, "span", "ai-tag", (pol.admins && pol.admins.length) ? pol.admins.join(", ") : "none");
  el(panel, "p", "admin-sub", "Domains and admins are set in the server config file (they can't be widened from the browser).");

  // Pending approvals live here too, so this is the single admin home.
  el(panel, "h3", null, "Pending signups");
  const plist = el(panel, "div", "admin-list");
  let pending = [];
  try { pending = (await getJSON("/api/admin/pending")).pending || []; } catch { }
  if (!pending.length) el(plist, "div", "admin-empty", "No one is waiting for approval.");
  for (const u of pending) {
    const row = el(plist, "div", "admin-item");
    el(row, "span", "ai-main", (u.name ? u.name + "  ·  " : "") + u.email);
    const ok = el(row, "button", "pbtn", "Approve");
    ok.onclick = async () => { try { await postJSON("/api/admin/pending/" + u.id + "/approve"); toast("Approved " + u.email); updateAdminBar(); showHubSettings(); } catch (e) { toast(e.message, true); } };
    const no = el(row, "button", "ai-del", "Deny");
    no.onclick = async () => { try { await postJSON("/api/admin/pending/" + u.id + "/deny"); toast("Denied " + u.email); updateAdminBar(); showHubSettings(); } catch (e) { toast(e.message, true); } };
  }
}

async function showPending() {
  let pending = [];
  try { pending = (await getJSON("/api/admin/pending")).pending || []; } catch { }
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
    ok.onclick = async () => { try { await postJSON("/api/admin/pending/" + u.id + "/approve"); toast("Approved " + u.email); updateAdminBar(); showPending(); } catch (e) { toast(e.message, true); } };
    const no = el(row, "button", "ai-del", "Deny");
    no.onclick = async () => { try { await postJSON("/api/admin/pending/" + u.id + "/deny"); toast("Denied " + u.email); updateAdminBar(); showPending(); } catch (e) { toast(e.message, true); } };
  }
}

function refreshAll() {
  loadProjects();
  updateOrgBar();
  if (currentProject) refreshTree();
}

/* Native path routing (no hash, no %2F):
     volume mode:  /<path>
     hub mode:     /<project-id>/<path>
     invite:       /join/<token>
   Each path segment is percent-encoded for odd characters, but the "/"
   separators stay literal so the URL reads like a real file path. */
function encodePath(p) { return p.split("/").map(encodeURIComponent).join("/"); }
function decodePath(p) { return p.split("/").map(decodeURIComponent).join("/"); }

function parseRoute() {
  const raw = location.pathname.replace(/^\/+/, "");
  if (serverConfig.mode !== "hub") return { path: raw ? decodePath(raw) : "" };
  const slash = raw.indexOf("/");
  if (slash === -1) return { project: raw, path: "" };
  return { project: raw.slice(0, slash), path: decodePath(raw.slice(slash + 1)) };
}

/* The URL for a file within the current context. */
function urlForPath(path) {
  const enc = encodePath(path);
  if (serverConfig.mode === "hub" && currentProject) {
    return "/" + currentProject.id + (enc ? "/" + enc : "");
  }
  return "/" + enc;
}

/* Push a route without reloading, skipping a no-op that would just stack a
   duplicate history entry (e.g. when boot opens the file already in the URL). */
function pushURL(url) {
  if (location.pathname === url) return;
  history.pushState(null, "", url);
}
function syncURL(path) { pushURL(urlForPath(path)); }

/* ---- tree ---- */
async function refreshTree() {
  if (serverConfig.mode === "hub" && !currentProject) return;
  let root;
  try {
    root = await getJSON(apiBase + "tree");
  } catch { return; } // keep the last good tree
  flatFiles = [];
  dirIndex = new Map();
  const kids = root.children || [];
  // First render of a project's tree: every folder starts closed, except a
  // lone root folder — opening it spares the user a single shut folder.
  if (treeFirstLoad) {
    treeFirstLoad = false;
    const rootDirs = kids.filter((c) => c.dir);
    if (rootDirs.length === 1) expanded.add(rootDirs[0].path);
  }
  const nav = $("tree");
  nav.innerHTML = "";
  nav.appendChild(renderChildren(kids));
  markActive();
  // A folder listing shows live tree data — keep it in step with the tree.
  if (currentPath && dirIndex.has(currentPath) && $("content").querySelector(".dirlist")) {
    renderFolderListing(currentPath);
  }
  refreshHeat();
}

/* ---- read heat ----
   30-day read counts per path from the heat API (hub only). Counts only —
   the server never says who read what. Fetched lazily alongside the tree. */
async function refreshHeat(force) {
  if (!(serverConfig.mode === "hub" && currentProject)) return;
  if (!(serverConfig.reads && serverConfig.reads.enabled)) return;
  if (!force && Date.now() - heatAt < 60000) return;
  heatAt = Date.now(); // set before the fetch so failures don't hammer
  let out;
  try {
    out = await getJSON(apiBase + "heat?days=30");
  } catch { return; } // keep the last good heat
  heatMap = out.entries || {};
  if (currentPath && dirIndex.has(currentPath) && $("content").querySelector(".dirlist")) {
    renderFolderListing(currentPath);
  }
}

/* Heat for one listing entry: a file's own bucket, or the subtree sum for a
   folder. Null when there is nothing to show. */
function heatFor(path, isDir) {
  if (!heatMap) return null;
  if (!isDir) return heatMap[path] || null;
  const agg = { human: 0, agent: 0, share: 0 };
  for (const [p, e] of Object.entries(heatMap)) {
    if (!p.startsWith(path + "/")) continue;
    agg.human += e.human || 0;
    agg.agent += e.agent || 0;
    agg.share += e.share || 0;
  }
  return agg.human || agg.agent || agg.share ? agg : null;
}

function heatTotal(e) { return (e.human || 0) + (e.agent || 0) + (e.share || 0); }

function heatText(e) {
  const total = heatTotal(e);
  if (!total) return "";
  let s = total + (total === 1 ? " read" : " reads");
  if (e.agent) s += " (" + e.agent + " agent)";
  return s;
}

/* Dot intensity 1–4, log-ish steps: 1–2, 3–9, 10–29, 30+ reads. */
function heatLevel(e) {
  const total = heatTotal(e);
  if (!total) return 0;
  if (total < 3) return 1;
  if (total < 10) return 2;
  if (total < 30) return 3;
  return 4;
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
  chev.innerHTML = svgIcon("chevd");
  const ticon = document.createElement("span");
  ticon.className = "ticon";
  ticon.innerHTML = svgIcon(n.dir ? "folder" : "doc");
  const label = document.createElement("span");
  label.className = "label";
  label.textContent = n.name;
  row.append(chev, ticon, label);
  // Keyboard-operable: the row behaves as a button.
  row.tabIndex = 0;
  row.setAttribute("role", "button");
  row.title = n.name;
  row.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); row.click(); } };
  li.appendChild(row);
  if (n.dir) {
    dirIndex.set(n.path, n);
    if (serverConfig.mode === "hub") {
      const hist = document.createElement("span");
      hist.className = "dir-history";
      hist.innerHTML = svgIcon("hist");
      hist.title = "Folder history";
      hist.onclick = (e) => { e.stopPropagation(); showHistory({ prefix: n.path + "/" }); };
      row.appendChild(hist);
    }
    li.appendChild(renderChildren(n.children || []));
    const open = expanded.has(n.path);
    if (!open) li.classList.add("collapsed");
    row.setAttribute("aria-expanded", String(open));
    const toggle = () => {
      li.classList.toggle("collapsed");
      const isCollapsed = li.classList.contains("collapsed");
      isCollapsed ? expanded.delete(n.path) : expanded.add(n.path);
      row.setAttribute("aria-expanded", String(!isCollapsed));
    };
    // The chevron only folds; the row selects the folder (opens it in the
    // tree and lists its contents in the main pane). Clicking the folder
    // whose listing is already showing folds/unfolds it, like a plain tree;
    // from any other view (file, history) it brings the listing back.
    chev.onclick = (e) => { e.stopPropagation(); toggle(); };
    row.onclick = () => {
      if (currentPath === n.path && $("content").querySelector(".dirlist")) { toggle(); return; }
      openFolder(n.path);
    };
  } else {
    flatFiles.push({ path: n.path, name: n.name, time: n.time });
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

/* Mark every ancestor folder of a path as open. */
function expandTo(filePath) {
  const parts = filePath.split("/");
  let acc = "";
  for (let i = 0; i < parts.length - 1; i++) {
    acc = acc ? acc + "/" + parts[i] : parts[i];
    expanded.add(acc);
  }
}

/* Reflect the `expanded` set onto the already-rendered tree DOM. */
function applyTreeExpansion() {
  for (const li of document.querySelectorAll("#tree li.dir")) {
    const row = li.querySelector(":scope > .row");
    const open = row && expanded.has(row.dataset.path);
    li.classList.toggle("collapsed", !open);
    if (row) row.setAttribute("aria-expanded", String(!!open));
  }
}

/* Opening a file (search, wikilink, deep link) unfolds the path to it and
   scrolls the row into view so the selection is never hidden in a shut folder. */
function revealInTree(p) {
  expandTo(p);
  applyTreeExpansion();
  const row = document.querySelector(`#tree .row[data-path="${CSS.escape(p)}"]`);
  if (row) row.scrollIntoView({ block: "center" });
}

/* ---- breadcrumb ----
   Every ancestor segment is a link to that folder's listing; the last
   segment is the current page. */
function setCrumb(p) {
  const c = $("crumb");
  c.innerHTML = "";
  const parts = p.split("/");
  let acc = "";
  parts.forEach((seg, i) => {
    acc = acc ? acc + "/" + seg : seg;
    if (i) el(c, "span", "crumb-sep", "/");
    const last = i === parts.length - 1;
    const s = el(c, "span", last ? null : "crumb-seg", seg);
    if (!last) {
      const target = acc;
      s.title = target;
      s.onclick = () => { if (dirIndex.has(target)) openFolder(target); };
    }
  });
}

/* ---- folder pane ---- */

/* Open a path of either kind — a route or link doesn't know which it is
   until the tree has loaded. */
function openPath(p) {
  if (dirIndex.has(p)) openFolder(p);
  else openFile(p);
}

/* Selecting a folder: unfold it in the tree, highlight it, and list what's
   inside it in the main pane. */
function openFolder(p) {
  if (!dirIndex.has(p)) return;
  currentPath = p;
  syncURL(p);
  expandTo(p);
  expanded.add(p); // the selected folder itself opens, not just its ancestors
  applyTreeExpansion();
  markActive();
  const row = document.querySelector(`#tree .row[data-path="${CSS.escape(p)}"]`);
  if (row) row.scrollIntoView({ block: "nearest" });
  closeSidebarOnMobile();
  setCrumb(p);
  $("meta").textContent = "";
  $("download").hidden = true;
  $("more-btn").hidden = !(serverConfig.mode === "hub" && currentProject);
  updateShareButton();
  renderFolderListing(p);
}

function renderFolderListing(p) {
  const node = dirIndex.get(p);
  if (!node) return;
  const content = $("content");
  content.className = "view";
  content.innerHTML = "";
  const wrap = el(content, "div", "dirlist");
  const head = el(wrap, "h1", "dl-title");
  const hicon = el(head, "span", "dl-title-icon");
  hicon.innerHTML = svgIcon("folder");
  el(head, "span", null, node.name);
  const kids = (node.children || []).slice()
    .sort((a, b) => (b.dir - a.dir) || a.name.localeCompare(b.name));
  const dirs = kids.filter((c) => c.dir).length;
  const files = kids.length - dirs;
  const counts = [];
  if (dirs) counts.push(dirs + (dirs === 1 ? " folder" : " folders"));
  if (files) counts.push(files + (files === 1 ? " file" : " files"));
  const folderHeat = heatFor(p, true);
  if (folderHeat) counts.push(heatText(folderHeat) + " in 30 days");
  el(wrap, "p", "dl-sub", counts.join(" · ") || "Empty folder");
  if (!kids.length) {
    el(wrap, "div", "dl-empty", "Nothing in this folder yet.");
    renderFolderHistory(wrap, p);
    return;
  }
  const list = el(wrap, "div", "dl-items");
  for (const c of kids) {
    const row = el(list, "div", "dl-row");
    row.tabIndex = 0;
    row.setAttribute("role", "button");
    row.title = c.path;
    const icon = el(row, "span", "ticon");
    icon.innerHTML = svgIcon(c.dir ? "folder" : "doc");
    el(row, "span", "dl-name", c.name);
    let meta = "";
    if (c.dir) {
      const n = (c.children || []).length;
      meta = n + (n === 1 ? " item" : " items");
    } else {
      meta = [c.size ? humanSize(c.size) : "", c.time ? new Date(c.time).toLocaleDateString() : ""]
        .filter(Boolean).join(" · ");
    }
    const he = heatFor(c.path, !!c.dir);
    if (he) {
      const dot = el(row, "span", "heatdot lvl" + heatLevel(he));
      dot.title = heatText(he) + " in 30 days";
      meta = heatText(he) + (meta ? " · " + meta : "");
    }
    el(row, "span", "dl-meta", meta);
    row.onclick = () => openPath(c.path);
    row.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); row.click(); } };
  }
  renderFolderHistory(wrap, p);
}

/* The folder's change feed, straight from the journals: files added, edited,
   and deleted anywhere under it, newest first. Hub-only — a plain-folder
   viewer has no journals to read. */
function renderFolderHistory(wrap, p) {
  if (!(serverConfig.mode === "hub" && currentProject)) return;
  const sec = el(wrap, "div", "dl-history");
  getJSON(apiBase + "history?prefix=" + encodeURIComponent(p + "/") + "&n=20").then((out) => {
    const entries = out.entries || [];
    if (!entries.length) return sec.remove();
    el(sec, "h3", "dl-h3", "Recent changes");
    const list = el(sec, "div", "history dl-hlist");
    for (const e of entries) list.appendChild(historyEntryRow(e));
    const more = el(sec, "button", "ai-btn dl-more", "Full history");
    more.onclick = () => showHistory({ prefix: p + "/" });
  }).catch(() => sec.remove());
}

/* ---- file pane ---- */
async function openFile(p) {
  currentPath = p;
  syncURL(p);
  markActive();
  revealInTree(p);
  closeSidebarOnMobile();
  setCrumb(p);
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
  const he = heatMap && heatMap[doc.path];
  if (he && heatTotal(he)) parts.push(heatText(he) + " / 30d");
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
      <h3>Public link created</h3>
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
    try { await api("DELETE", "/api/shares/" + token); toast("Link revoked — it no longer works."); close(); }
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
  // Shares are per-file; a selected folder has nothing to mint.
  btn.hidden = !(serverConfig.mode === "hub" && currentProject && currentPath && !dirIndex.has(currentPath));
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

/* ---- insights: the read×write matrix ----
   Every file plotted by how much it is read (30 days, from the heat API)
   against how long since it last changed (from the tree). The hot-but-stale
   quadrant is the danger zone: knowledge people still rely on that nobody
   maintains. Admin/org-owner only — members get the ambient heat dots. */

const HOT_READS = 3;    // ≥ this many reads/30d = hot
const STALE_DAYS = 30;  // ≥ this many days since last write = stale

function canSeeInsights() {
  if (!(serverConfig.mode === "hub" && currentProject)) return false;
  if (!(serverConfig.reads && serverConfig.reads.enabled)) return false;
  if (serverConfig.auth && serverConfig.auth.admin) return true;
  const org = currentOrg();
  return !!(org && org.role === "owner");
}

async function showInsights() {
  if (!canSeeInsights()) return;
  await refreshHeat(true);
  currentPath = null;
  markActive();
  $("crumb").textContent = "Insights — " + currentProject.name;
  $("meta").textContent = "";
  $("download").hidden = true;
  $("more-btn").hidden = true;
  const content = $("content");
  content.className = "view";
  renderInsights(content, "all");
}

function renderInsights(content, lens) {
  content.innerHTML = "";
  const wrap = el(content, "div", "insights");
  el(wrap, "h1", "in-title", "Reads × freshness");
  el(wrap, "p", "dl-sub",
    "Every file by 30-day reads and days since its last change. " +
    "Hot but stale (top right) is the danger zone — read a lot, maintained by nobody.");
  const bar = el(wrap, "div", "in-lens");
  for (const l of ["all", "human", "agent"]) {
    const label = l === "all" ? "All reads" : l === "human" ? "Human reads" : "Agent reads";
    const b = el(bar, "button", "in-lens-btn" + (l === lens ? " active" : ""), label);
    b.onclick = () => renderInsights(content, lens = l);
  }

  const readsOf = (e) => (lens === "all" ? heatTotal(e) : e[lens] || 0);
  const now = Date.now();
  const pts = flatFiles.map((f) => {
    const e = (heatMap && heatMap[f.path]) || {};
    const days = f.time ? Math.max(0, (now - new Date(f.time).getTime()) / 86400000) : 0;
    const reads = readsOf(e);
    return { path: f.path, reads, days, danger: reads >= HOT_READS && days >= STALE_DAYS };
  });

  wrap.appendChild(insightsChart(pts));

  const danger = pts.filter((p) => p.danger)
    .sort((a, b) => b.reads - a.reads || b.days - a.days).slice(0, 15);
  el(wrap, "h3", "dl-h3", "Danger zone — fix these first");
  if (!danger.length) {
    el(wrap, "div", "dl-empty", "No hot-but-stale files. The knowledge base is healthy.");
    return;
  }
  const list = el(wrap, "div", "dl-items");
  for (const p of danger) {
    const row = el(list, "div", "dl-row");
    row.tabIndex = 0;
    row.setAttribute("role", "button");
    const icon = el(row, "span", "ticon");
    icon.innerHTML = svgIcon("alert");
    el(row, "span", "dl-name", p.path);
    el(row, "span", "dl-meta",
      p.reads + (p.reads === 1 ? " read" : " reads") + " · untouched " + Math.round(p.days) + "d");
    row.onclick = () => openFile(p.path);
    row.onkeydown = (e) => { if (e.key === "Enter" || e.key === " ") { e.preventDefault(); row.click(); } };
  }
}

/* Dependency-free SVG scatter: x = days since last write, y = reads, both
   log-scaled; threshold lines split the quadrants. */
function insightsChart(pts) {
  const W = 720, H = 360, M = { l: 44, r: 16, t: 20, b: 34 };
  const maxDays = Math.max(STALE_DAYS * 2, ...pts.map((p) => p.days));
  const maxReads = Math.max(HOT_READS * 2, ...pts.map((p) => p.reads));
  const lx = (d) => Math.log10(d + 1) / Math.log10(maxDays + 1);
  const ly = (r) => Math.log10(r + 1) / Math.log10(maxReads + 1);
  const X = (d) => M.l + lx(d) * (W - M.l - M.r);
  const Y = (r) => H - M.b - ly(r) * (H - M.t - M.b);

  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("viewBox", `0 0 ${W} ${H}`);
  svg.setAttribute("class", "in-chart");
  const add = (tag, attrs, text) => {
    const n = document.createElementNS("http://www.w3.org/2000/svg", tag);
    for (const [k, v] of Object.entries(attrs)) n.setAttribute(k, v);
    if (text != null) n.textContent = text;
    svg.appendChild(n);
    return n;
  };

  // danger quadrant shading + threshold lines
  add("rect", { x: X(STALE_DAYS), y: M.t, width: W - M.r - X(STALE_DAYS), height: Y(HOT_READS) - M.t, class: "in-danger-zone" });
  add("line", { x1: X(STALE_DAYS), y1: M.t, x2: X(STALE_DAYS), y2: H - M.b, class: "in-threshold" });
  add("line", { x1: M.l, y1: Y(HOT_READS), x2: W - M.r, y2: Y(HOT_READS), class: "in-threshold" });
  // axes
  add("line", { x1: M.l, y1: H - M.b, x2: W - M.r, y2: H - M.b, class: "in-axis" });
  add("line", { x1: M.l, y1: M.t, x2: M.l, y2: H - M.b, class: "in-axis" });
  add("text", { x: (M.l + W - M.r) / 2, y: H - 8, class: "in-label" }, "days since last change →");
  add("text", { x: 12, y: (M.t + H - M.b) / 2, class: "in-label", transform: `rotate(-90 12 ${(M.t + H - M.b) / 2})` }, "reads / 30d →");
  add("text", { x: W - M.r - 6, y: M.t + 14, class: "in-quad in-quad-danger", "text-anchor": "end" }, "hot + stale");
  add("text", { x: M.l + 6, y: M.t + 14, class: "in-quad" }, "hot + fresh");
  add("text", { x: W - M.r - 6, y: H - M.b - 8, class: "in-quad", "text-anchor": "end" }, "cold + stale");

  for (const p of pts) {
    const c = add("circle", {
      cx: X(p.days).toFixed(1), cy: Y(p.reads).toFixed(1), r: 5,
      class: "in-pt" + (p.danger ? " danger" : p.reads ? "" : " cold"),
    });
    const tip = document.createElementNS("http://www.w3.org/2000/svg", "title");
    tip.textContent = `${p.path} — ${p.reads} read${p.reads === 1 ? "" : "s"} / 30d · changed ${Math.round(p.days)}d ago`;
    c.appendChild(tip);
    c.onclick = () => openFile(p.path);
  }
  return svg;
}

/* ---- history ----
   Every change ever made, straight from the journals: who (account), when,
   from which device (name, OS, IP as the server saw it), with view/download
   of that exact version. Groundwork for revert/rollback. */
function initHistory() {
  const btn = $("history-btn");
  btn.hidden = !(serverConfig.mode === "hub" && currentProjectOrNull());
  btn.onclick = () => {
    if (!currentPath) return showHistory({ prefix: "" });
    if (dirIndex.has(currentPath)) return showHistory({ prefix: currentPath + "/" });
    showHistory({ path: currentPath });
  };
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
  for (const e of out.entries || []) wrap.appendChild(historyEntryRow(e));
  content.appendChild(wrap);
}

/* One change as a row: what happened (added / edited / deleted), to which
   file, by whom, from where — with view/download of that exact version. */
const KIND_ICON = { add: "plus", edit: "edit", delete: "x" };
const KIND_LABEL = { add: "added", edit: "edited", delete: "deleted" };
function historyEntryRow(e) {
  const kind = e.kind === "put" ? "edit" : e.kind; // older servers report raw "put" ops
  const row = document.createElement("div");
  row.className = "hentry " + kind;
  const who = e.user_name ? `${e.user_name} <${e.user}>` : (e.user || e.author || "unknown");
  const dev = [e.device.name || e.device.id, e.device.os, e.device.ip].filter(Boolean).join(" · ");
  const when = new Date(e.time).toLocaleString();
  row.innerHTML =
    `<div class="hline"><span class="hkind"></span><span class="hpath"></span><span class="htag"></span><span class="htime"></span></div>` +
    `<div class="hmeta"><span class="hwho"></span><span class="hdev"></span><span class="hsize"></span><span class="hact"></span></div>`;
  row.querySelector(".hkind").innerHTML = svgIcon(KIND_ICON[kind] || "dot");
  row.querySelector(".hpath").textContent = e.path;
  row.querySelector(".htag").textContent = KIND_LABEL[kind] || kind;
  row.querySelector(".htime").textContent = when;
  row.querySelector(".hwho").textContent = who;
  row.querySelector(".hdev").textContent = dev;
  row.querySelector(".hsize").textContent = e.size ? humanSize(e.size) : "";
  if (kind !== "delete" && e.blob) {
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
  if (e.note) {
    const note = document.createElement("div");
    note.className = "hnote";
    // Linkify http(s) URLs (e.g. a Claude session link); everything else
    // stays plain text — notes are user/agent input, never markup.
    for (const tok of e.note.split(/(https?:\/\/\S+)/)) {
      if (/^https?:\/\//.test(tok)) {
        const a = document.createElement("a");
        a.href = tok; a.textContent = tok; a.target = "_blank"; a.rel = "noopener";
        note.appendChild(a);
      } else if (tok) {
        note.append(tok);
      }
    }
    row.appendChild(note);
  }
  const p = e.path;
  row.querySelector(".hpath").onclick = () => showHistory({ path: p });
  return row;
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
    // A selected folder receives the upload; a selected file means "next to it".
    const dir = !currentPath ? ""
      : dirIndex.has(currentPath) ? currentPath
      : currentPath.includes("/") ? currentPath.slice(0, currentPath.lastIndexOf("/")) : "";
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
    add("share", "Share: " + currentPath, "action", () => $("share-btn").click());
    add("hist", "History: " + currentPath, "action", () => showHistory({ path: currentPath }));
    add("download", "Download: " + currentPath, "action", () => $("download").click());
  }
  if (hub && currentProject) {
    add("hist", "History: whole project", "action", () => showHistory({ prefix: "" }));
  }
  if (!$("upload-btn").hidden) {
    add("upload", "Upload a file…", "action", () => $("upload-btn").click());
  }
  if (hub) {
    for (const p of projects) {
      if (!currentProject || p.id !== currentProject.id) {
        add("folder", "Switch to project: " + p.name, "project", () => selectProject(p, null));
      }
    }
  }
  if (serverConfig.auth && serverConfig.auth.enabled) {
    add("power", "Sign out", "action", () => { location.href = "/auth/logout"; });
  }
  for (const d of dirIndex.keys()) {
    add("folder", d, "folder", () => openFolder(d));
  }
  for (const f of flatFiles) {
    add("doc", f.path, "file", () => openFile(f.path));
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
    const pic = document.createElement("span");
    pic.className = "picon";
    pic.innerHTML = svgIcon(item.icon);
    const kind = document.createElement("span");
    kind.className = "pkind";
    kind.textContent = item.kind;
    li.append(pic, highlight(item.label, item.hits), kind);
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
  if (canSeeInsights()) {
    const b = document.createElement("button");
    b.className = "more-item";
    b.textContent = "Insights";
    b.onclick = () => { $("more-menu").hidden = true; showInsights(); };
    menu.appendChild(b);
    return items.length + 1;
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

/* Back/forward: re-resolve the route from the URL. selectProject/openFile
   dedup against the current URL, so replaying it here never stacks history. */
window.addEventListener("popstate", () => {
  const { project, path } = parseRoute();
  if (serverConfig.mode === "hub" && project && (!currentProject || currentProject.id !== project)) {
    const proj = projects.find((x) => x.id === project);
    if (proj) { selectProject(proj, path || null); return; }
  }
  if (path && path !== currentPath) openPath(path);
});

boot();
