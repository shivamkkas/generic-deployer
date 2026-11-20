"use strict";

const $ = (sel) => document.querySelector(sel);
const $$ = (sel) => document.querySelectorAll(sel);
const enc = encodeURIComponent;

function el(tag, props = {}, ...kids) {
  const n = document.createElement(tag);
  for (const [k, v] of Object.entries(props)) {
    if (k === "class") n.className = v;
    else if (k.startsWith("on")) n.addEventListener(k.slice(2), v);
    else n[k] = v;
  }
  for (const k of kids) n.append(k);
  return n;
}

class HTTPError extends Error { constructor(status, msg) { super(msg); this.status = status; } }

// When the console restarts (e.g. it restarted its own service) requests fail
// with a network error: show a banner, poll until it answers, then refresh.
let reconnecting = null;
function waitForServer() {
  if (reconnecting) return reconnecting;
  $("#conn-banner").hidden = false;
  reconnecting = (async () => {
    for (let i = 0; i < 90; i++) {
      await new Promise((r) => setTimeout(r, 2000));
      try {
        const r = await fetch("/api/me", { credentials: "same-origin" });
        if (r.ok || r.status === 401) {
          $("#conn-banner").hidden = true;
          reconnecting = null;
          if (r.status === 401) showLogin();
          else loadTargets(state.cur?.name).catch(() => {});
          return;
        }
      } catch { /* still down */ }
    }
    $("#conn-banner").textContent = "The console is not answering. Check `systemctl status` on the host, then reload this page.";
  })();
  return reconnecting;
}

async function api(method, url, body, json = false) {
  const headers = { "X-Console": "1" };
  if (json) { headers["Content-Type"] = "application/json"; body = JSON.stringify(body); }
  let r;
  try {
    r = await fetch(url, { method, body, headers, credentials: "same-origin" });
  } catch (e) {
    waitForServer();
    throw new HTTPError(0, "console unreachable (reconnecting…)");
  }
  if (r.status === 401 && url !== "/api/login") { showLogin(); throw new HTTPError(401, "signed out"); }
  if (!r.ok) throw new HTTPError(r.status, (await r.text()).trim() || r.statusText);
  return r;
}

const state = {
  me: null, targets: [], cur: null, service: "", root: 0, cwd: "",
  file: null, isNew: false, original: "", running: false, timer: null,
};

const CONSOLE_MAX = 200_000, CONSOLE_KEEP = 150_000;
const lines = (s) => s.split("\n").map((l) => l.trim()).filter(Boolean);

function stateClass(s) {
  if (s === "active") return "ok";
  const m = /^(\d+)\/(\d+) running$/.exec(s);
  if (m) return m[1] === m[2] ? "ok" : (m[1] === "0" ? "bad" : "warn");
  if (["failed", "inactive", "no containers"].includes(s)) return "bad";
  return "warn";
}

// second click confirms
function armable(btn, label, onConfirm) {
  let timer = null;
  btn.textContent = label;
  btn.classList.remove("armed");
  btn.onclick = () => {
    if (!btn.classList.contains("armed")) {
      btn.classList.add("armed"); btn.textContent = "Confirm " + label + "?";
      timer = setTimeout(() => { btn.classList.remove("armed"); btn.textContent = label; }, 4000);
      return;
    }
    clearTimeout(timer); btn.classList.remove("armed"); btn.textContent = label;
    onConfirm();
  };
}

/* =========================================================== login */

function showLogin() {
  clearInterval(state.timer);
  state.me = null;
  $("#app-view").hidden = true;
  $("#login-view").hidden = false;
  $("#login-form").password.value = "";
  $("#login-form").user.focus();
}

async function startApp() {
  try {
    state.me = await (await api("GET", "/api/me")).json();
  } catch { return; } // showLogin already ran on 401
  $("#login-view").hidden = true;
  $("#app-view").hidden = false;
  $("#host").textContent = `${state.me.hostname} · v${state.me.version}`;
  $("#whoami").textContent = state.me.name + (state.me.admin ? " (admin)" : "");
  $("#admin-nav").hidden = !state.me.admin;
  $$(".admin-only").forEach((n) => (n.hidden = !state.me.admin));
  await loadTargets(decodeURIComponent(location.hash.slice(1)));
  clearInterval(state.timer);
  state.timer = setInterval(refreshAll, 15000);
}

$("#login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target;
  $("#login-error").textContent = "";
  try {
    await api("POST", "/api/login", { user: f.user.value.trim(), password: f.password.value }, true);
    await startApp();
  } catch (err) {
    $("#login-error").textContent = err.message;
  }
});

$("#logout").addEventListener("click", async () => {
  if (!confirmDiscard()) return;
  try { await api("POST", "/api/logout"); } catch { /* ignore */ }
  location.hash = "";
  showLogin();
});

/* =========================================================== views */

const VIEWS = ["empty", "form", "users", "user-form", "target"];
function show(view) {
  $("#empty-card").hidden = view !== "empty";
  $("#form-card").hidden = view !== "form";
  $("#users-card").hidden = view !== "users" && view !== "user-form";
  $("#user-form-card").hidden = view !== "user-form";
  $("#target-card").hidden = $("#console-card").hidden = view !== "target";
  $("#files-card").hidden = view !== "target" || !state.cur?.roots?.length;
  $("#nav-users")?.classList.toggle("active", view === "users" || view === "user-form");
  if (view !== "target") $$(".side #targets button").forEach((b) => b.classList.remove("active"));
  console.assert(VIEWS.includes(view));
}

/* =========================================================== targets */

async function loadTargets(selectName) {
  state.targets = await (await api("GET", "/api/targets")).json();
  $("#targets").replaceChildren(...state.targets.map((t) =>
    el("li", {}, el("button", { id: "tb-" + t.name, onclick: () => select(t) },
      el("span", { class: "dot", id: "dot-" + t.name }),
      el("span", { class: "tname", textContent: t.name }),
      el("span", { class: "tag", textContent: t.type })))));
  const pick = state.targets.find((t) => t.name === selectName) || state.targets[0];
  state.cur = null;
  if (pick) select(pick);
  else {
    $("#empty-title").textContent = "No targets yet";
    $("#empty-text").textContent = state.me.admin
      ? "Add a docker compose project or a systemd service, then give users access in Users & permissions."
      : "No targets are assigned to you yet. Ask an admin to grant you access.";
    $("#empty-add").hidden = !state.me.admin;
    show("empty");
  }
  refreshAll();
}

async function refreshState(t) {
  try {
    const s = await (await api("GET", `/api/targets/${enc(t.name)}/state`)).json();
    const label = s.busy ? s.state + " · busy" : s.state;
    const dot = document.getElementById("dot-" + t.name);
    if (dot) { dot.className = "dot " + stateClass(s.state); dot.title = label; }
    if (state.cur?.name === t.name) { const p = $("#t-state"); p.textContent = label; p.className = "pill " + stateClass(s.state); }
  } catch { /* leave as is */ }
}
const refreshAll = () => state.targets.forEach(refreshState);

function confirmDiscard() {
  return !isDirty() || confirm("Discard unsaved changes to " + state.file + "?");
}

function select(t) {
  if (!t || !confirmDiscard()) return;
  state.cur = t;
  history.replaceState(null, "", "#" + t.name);
  $$(".side #targets button").forEach((b) => b.classList.toggle("active", b.id === "tb-" + t.name));
  $("#t-name").textContent = t.name;
  $("#t-meta").textContent = [t.type === "systemd" ? "unit " + t.unit : "compose", t.dir || "", t.config?.sudo ? "sudo" : ""].filter(Boolean).join("  ·  ");
  $("#t-desc").textContent = t.description || "";
  $("#t-problem").textContent = t.problem ? "⚠ " + t.problem : "";
  $("#t-problem").hidden = !t.problem;
  $("#t-state").textContent = "…"; $("#t-state").className = "pill";
  if (state.me.admin) armable($("#delete-target"), "Delete", deleteTarget);
  loadServices(t);
  renderActions();
  refreshState(t);
  closeFile();
  show("target");
  const rs = $("#root-select");
  rs.replaceChildren(...t.roots.map((r, i) => el("option", { value: i, textContent: `${r.path}  (${r.mode === "rw" ? "read-write" : "read-only"})` })));
  rs.hidden = t.roots.length < 2;
  state.root = 0;
  syncRootMode();
  if (t.roots.length) loadDir("");
}

const curRoot = () => state.cur?.roots[state.root];
function syncRootMode() {
  const ro = curRoot()?.mode !== "rw";
  $("#new-name").hidden = $("#new-btn").hidden = ro;
}

/* ---------- compose services ---------- */

async function loadServices(t) {
  const row = $("#svc-row"), sel = $("#svc-select");
  state.service = "";
  row.hidden = t.type !== "compose";
  if (t.type !== "compose") return;
  const limited = !!(t.services && t.services.length);
  sel.replaceChildren(...(limited ? [] : [el("option", { value: "", textContent: "All services (whole project)" })]),
    ...(limited ? t.services : []).map((n) => el("option", { value: n, textContent: n })));
  $("#svc-note").textContent = limited ? "You can act on these services only." : "";
  state.service = sel.value;
  renderActions();
  try {
    const svcs = await (await api("GET", `/api/targets/${enc(t.name)}/services`)).json();
    if (state.cur !== t) return;
    const keep = sel.value;
    sel.replaceChildren(...(limited ? [] : [el("option", { value: "", textContent: "All services (whole project)" })]),
      ...svcs.map((s) => el("option", { value: s.name, textContent: s.name })));
    if ([...sel.options].some((o) => o.value === keep)) sel.value = keep;
    state.service = sel.value;
    renderActions();
  } catch (e) {
    if (!limited) $("#svc-note").textContent = "could not list services: " + e.message;
  }
}

$("#svc-select").addEventListener("change", (e) => { state.service = e.target.value; renderActions(); });

// Download links follow the chosen service / time range; the browser saves the file directly.
function updateDownloads() {
  const t = state.cur;
  if (!t) return;
  const has = (n) => t.actions.some((a) => a.name === n);
  const base = `/api/targets/${enc(t.name)}`;
  const canLogs = has("logs");
  $("#dl-logs-wrap").hidden = !canLogs;
  if (canLogs) {
    const q = new URLSearchParams({ since: $("#dl-since").value });
    if (state.service) q.set("service", state.service);
    $("#dl-logs").href = `${base}/logs?${q}`;
    $("#dl-logs").textContent = state.service ? `Logs file · ${state.service}` : "Logs file";
  }
  const canCfg = has(t.type === "compose" ? "config" : "status");
  $("#dl-config").hidden = !canCfg;
  if (canCfg) $("#dl-config").href = `${base}/config`;
  $("#dl-row").hidden = !canLogs && !canCfg;
}
$("#dl-since").addEventListener("change", updateDownloads);

/* ---------- actions ---------- */

function renderActions() {
  const t = state.cur;
  if (!t) return;
  $("#actions").replaceChildren(...t.actions.map((a) => {
    const b = el("button", { class: "btn" + (a.danger ? " danger" : "") });
    const label = state.service && a.scopable ? `${a.label} · ${state.service}` : a.label;
    if (a.danger) armable(b, label, () => run(t, a));
    else { b.textContent = label; b.onclick = () => run(t, a); }
    b.dataset.blocked = state.service && !a.scopable ? "1" : "";
    b.title = b.dataset.blocked ? "Applies to the whole project. Choose \"All services\" to use it." : a.name;
    return b;
  }));
  if (!t.actions.length) $("#actions").append(el("span", { class: "muted", textContent: "No actions granted on this target." }));
  setRunning(state.running);
  updateDownloads();
}

function setRunning(on) {
  state.running = on;
  $$("#actions button").forEach((b) => (b.disabled = on || !!b.dataset.blocked));
}

async function run(t, a) {
  if (state.running) return;
  setRunning(true);
  const out = $("#out");
  const append = (s) => {
    const stick = out.scrollHeight - out.scrollTop - out.clientHeight < 40;
    out.append(s);
    // keep the page light: drop the oldest output past ~200 KB (use "Logs file" for everything)
    if (out.textContent.length > CONSOLE_MAX) {
      out.textContent = "… (older output trimmed — download the logs file for the full log)\n" + out.textContent.slice(-CONSOLE_KEEP);
    }
    if (stick) out.scrollTop = out.scrollHeight;
  };
  const svc = a.scopable ? state.service : "";
  if (out.textContent) append("\n");
  append(`── ${t.name}${svc ? " / " + svc : ""} · ${a.label} · ${new Date().toLocaleTimeString()} ──\n`);
  $("#run-status").textContent = `running ${a.label} on ${t.name}…`;
  try {
    const r = await api("POST", `/api/targets/${enc(t.name)}/run/${enc(a.name)}${svc ? "?service=" + enc(svc) : ""}`);
    const reader = r.body.getReader();
    const dec = new TextDecoder();
    for (;;) {
      const { value, done } = await reader.read();
      if (done) break;
      append(dec.decode(value, { stream: true }));
    }
    append(dec.decode());
  } catch (e) {
    append("error: " + e.message + "\n");
  } finally {
    $("#run-status").textContent = "";
    setRunning(false);
    refreshState(t);
  }
}

/* =========================================================== admin: targets */

function splitArgs(s) {
  const out = [];
  for (const m of s.matchAll(/"((?:[^"\\]|\\.)*)"|'([^']*)'|(\S+)/g)) {
    out.push(m[1] !== undefined ? m[1].replace(/\\(.)/g, "$1") : m[2] !== undefined ? m[2] : m[3]);
  }
  return out;
}
const quoteArg = (a) => (a === "" || /[\s"'\\]/.test(a) ? '"' + a.replace(/[\\"]/g, "\\$&") + '"' : a);

function openForm(view) {
  if (!confirmDiscard()) return;
  closeFile();
  const t = view?.config;
  const f = $("#target-form");
  f.reset();
  f.dataset.mode = t ? "edit" : "create";
  $("#form-title").textContent = t ? "Edit " + t.name : "Add target";
  $("#form-error").textContent = "";
  $("#vol-status").textContent = "";
  if (t) {
    f.name.value = t.name;
    f.type.value = t.type;
    f.description.value = t.description || "";
    f.unit.value = t.unit || "";
    f.dir.value = t.dir || "";
    f.dirs.value = (t.dirs || []).join("\n");
    f.composeFiles.value = (t.composeFiles || []).join("\n");
    f.commands.value = Object.entries(t.commands || {}).map(([k, v]) => k + " = " + v.map(quoteArg).join(" ")).join("\n");
    f.readOnly.checked = !!t.readOnly;
    f.sudo.checked = !!t.sudo;
  }
  f.sudo.disabled = (state.me.root || !state.me.sudo) && !f.sudo.checked;
  $("#sudo-note").textContent = state.me.root ? "(not needed: the console already runs as root)"
    : state.me.sudo ? "(uses the sudo password from deploy.yaml)"
    : "(not available: sudo.password in deploy.yaml is missing or wrong; fix it and restart)";
  f.name.disabled = f.type.disabled = !!t;
  syncFormType();
  show("form");
  (t ? f.description : f.name).focus();
}

function syncFormType() {
  const f = $("#target-form");
  const sd = f.type.value === "systemd";
  $$("#target-form .only-systemd").forEach((n) => (n.hidden = !sd));
  $$("#target-form .only-compose").forEach((n) => (n.hidden = sd));
  $("#vol-row").hidden = sd || f.dataset.mode !== "edit";
  $("#dir-label").textContent = sd ? "Working / data directory (optional)" : "Compose project directory";
  f.dir.required = !sd;
  f.unit.required = sd;
}

// Volume folders outside the project dir must be added as target directories
// before they can be granted to users.
$("#add-volumes").addEventListener("click", async () => {
  const f = $("#target-form");
  $("#vol-status").textContent = "reading compose config…";
  try {
    const svcs = await (await api("GET", `/api/targets/${enc(f.name.value)}/services`)).json();
    const dir = f.dir.value.trim().replace(/\/+$/, "");
    const have = new Set(lines(f.dirs.value));
    const added = [];
    for (const s of svcs) for (const m of s.mounts || []) {
      if (m === dir || m.startsWith(dir + "/") || have.has(m)) continue;
      have.add(m); added.push(m);
    }
    f.dirs.value = [...have].join("\n");
    $("#vol-status").textContent = added.length ? `added ${added.length} folder(s); save to apply` : "all volume folders are already covered";
  } catch (e) {
    $("#vol-status").textContent = e.message;
  }
});

async function submitTarget(e) {
  e.preventDefault();
  const f = $("#target-form");
  const sd = f.type.value === "systemd";
  const commands = {};
  for (const l of lines(f.commands.value)) {
    const i = l.indexOf("=");
    if (i < 1) { $("#form-error").textContent = `command line "${l}" must look like: name = program args`; return; }
    commands[l.slice(0, i).trim()] = splitArgs(l.slice(i + 1));
  }
  const body = {
    name: f.name.value.trim(), type: f.type.value,
    description: f.description.value.trim() || undefined,
    dir: f.dir.value.trim() || undefined,
    dirs: lines(f.dirs.value),
    unit: sd ? f.unit.value.trim() : undefined,
    composeFiles: sd ? [] : lines(f.composeFiles.value),
    commands, readOnly: f.readOnly.checked, sudo: f.sudo.checked,
  };
  const edit = f.dataset.mode === "edit";
  try {
    await api(edit ? "PUT" : "POST", edit ? `/api/admin/targets/${enc(body.name)}` : "/api/admin/targets", body, true);
    await loadTargets(body.name);
  } catch (err) {
    $("#form-error").textContent = err.message;
  }
}

async function deleteTarget() {
  try {
    await api("DELETE", `/api/admin/targets/${enc(state.cur.name)}`);
    await loadTargets();
  } catch (err) {
    $("#run-status").textContent = "delete failed: " + err.message;
  }
}

/* =========================================================== admin: users */

let usersCache = [];

async function openUsers() {
  if (!confirmDiscard()) return;
  closeFile();
  state.cur = null;
  history.replaceState(null, "", "#");
  show("users");
  try {
    const res = await (await api("GET", "/api/admin/users")).json();
    usersCache = res.users;
    $("#builtin-note").textContent = `"${res.builtinAdmin}" is the built-in admin from deploy.yaml. It always has full control and is not listed here.`;
    $("#user-rows").replaceChildren(...(usersCache.length ? usersCache.map((u) =>
      el("tr", {},
        el("td", { class: "mono", textContent: u.name + (u.disabled ? " (disabled)" : "") }),
        el("td", { textContent: u.admin ? "admin" : "user" }),
        el("td", { class: "mono small", textContent: u.admin ? "all" : (u.grants.map((g) => g.target).join(", ") || "—") }),
        el("td", {}, el("button", { class: "btn ghost", textContent: "Edit", onclick: () => openUserForm(u) }))))
      : [el("tr", {}, el("td", { colSpan: 4, class: "muted", textContent: "No users yet." }))]));
  } catch (e) {
    $("#user-rows").replaceChildren(el("tr", {}, el("td", { colSpan: 4, class: "err", textContent: e.message })));
  }
}

function openUserForm(u) {
  const f = $("#user-form");
  f.reset();
  f.dataset.mode = u ? "edit" : "create";
  $("#user-form-title").textContent = u ? "Edit " + u.name : "New user";
  $("#user-form-error").textContent = "";
  f.name.value = u?.name || "";
  f.name.disabled = !!u;
  f.password.required = !u;
  f.password.placeholder = u ? "leave blank to keep the current password" : "min 8 characters";
  f.admin.checked = !!u?.admin;
  f.disabled.checked = !!u?.disabled;
  $("#user-delete").hidden = !u;
  if (u) armable($("#user-delete"), "Delete user", () => deleteUser(u.name));
  renderGrants(u?.grants || []);
  syncAdminBox();
  show("user-form");
  (u ? f.password : f.name).focus();
}

function syncAdminBox() { $("#grants-wrap").hidden = $("#user-form").admin.checked; }

// One block per target: access toggle, actions, services, paths.
function renderGrants(grants) {
  const wrap = $("#grants");
  wrap.replaceChildren(...state.targets.map((t) => {
    const g = grants.find((x) => x.target === t.name);
    const cfg = t.config;
    const box = el("div", { class: "grant" });
    box.dataset.target = t.name;
    const on = el("input", { type: "checkbox", checked: !!g, class: "g-on" });
    const body = el("div", { class: "grant-body", hidden: !g });
    on.addEventListener("change", () => { body.hidden = !on.checked; if (on.checked) loadGrantServices(box, t, g); });

    // actions
    const all = el("input", { type: "checkbox", class: "g-all", checked: !!g?.actions?.includes("*") });
    const acts = el("div", { class: "chips" }, el("label", {}, all, "All actions"),
      ...t.actions.map((a) => el("label", {},
        el("input", { type: "checkbox", class: "g-act", value: a.name, checked: !!g?.actions?.includes(a.name) }),
        a.label + (a.scopable ? "" : " ¹"))));
    const syncAll = () => acts.querySelectorAll(".g-act").forEach((c) => (c.disabled = all.checked));
    all.addEventListener("change", syncAll); syncAll();

    // services (compose)
    const svc = el("div", { class: "g-svc-wrap", hidden: t.type !== "compose" },
      el("h3", { textContent: "Services" }),
      el("div", { class: "chips g-svcs" }, el("span", { class: "muted small", textContent: "loading…" })));

    // paths
    const pathList = el("div", { class: "g-paths" });
    const addPath = (p = "", mode = "ro") => {
      const row = el("div", { class: "path-row" },
        el("input", { class: "input mono g-path", value: p, placeholder: "/opt/app/config" }),
        (() => { const s = el("select", { class: "input g-mode" },
          el("option", { value: "ro", textContent: "read-only" }), el("option", { value: "rw", textContent: "read-write" })); s.value = mode; return s; })(),
        el("button", { type: "button", class: "btn ghost", textContent: "✕", onclick: () => row.remove() }));
      pathList.append(row);
    };
    (g?.paths || []).forEach((p) => addPath(p.path, p.mode));
    const suggest = el("div", { class: "suggest g-suggest" },
      ...(cfg?.roots || []).map((r) => el("button", { type: "button", textContent: "+ " + r, onclick: () => addPath(r) })));

    body.append(
      el("div", {}, el("h3", { textContent: "Actions" }), acts),
      svc,
      el("div", {}, el("h3", { textContent: "Folders (files the user can open)" }), pathList,
        el("button", { type: "button", class: "btn ghost", textContent: "+ Add folder", onclick: () => addPath() }), suggest));
    box.append(el("label", { class: "grant-head" }, on, t.name, el("span", { class: "tag", textContent: t.type })), body);
    box._addPath = addPath;
    if (g) loadGrantServices(box, t, g);
    return box;
  }));
  if (!state.targets.length) wrap.append(el("p", { class: "muted", textContent: "Add a target first." }));
  wrap.append(el("p", { class: "muted small", textContent: "¹ whole-project action: hidden from users who are limited to specific services." }));
}

async function loadGrantServices(box, t, g) {
  if (t.type !== "compose" || box.dataset.svcLoaded) return;
  box.dataset.svcLoaded = "1";
  const chips = box.querySelector(".g-svcs");
  chips.replaceChildren(el("span", { class: "muted small", textContent: "loading services…" }));
  try {
    const svcs = await (await api("GET", `/api/targets/${enc(t.name)}/services`)).json();
    const chosen = new Set(g?.services || []);
    const names = [...new Set([...svcs.map((s) => s.name), ...chosen])].sort();
    chips.replaceChildren(...names.map((n) => el("label", {},
      el("input", { type: "checkbox", class: "g-svc", value: n, checked: chosen.has(n) }), n)));
    if (!names.length) chips.replaceChildren(el("span", { class: "muted small", textContent: "no services found" }));
    // offer each service's volume folders as path suggestions
    const sug = box.querySelector(".g-suggest");
    const roots = t.config?.roots || [];
    for (const s of svcs) for (const m of s.mounts || []) {
      const inside = roots.some((r) => m === r || m.startsWith(r.replace(/\/$/, "") + "/"));
      sug.append(el("button", {
        type: "button", textContent: `+ ${m} (${s.name} volume)`,
        title: inside ? "" : "Outside the target's directories: add it to the target first (Edit target → Add the services' volume folders)",
        disabled: !inside, onclick: () => box._addPath(m, "rw"),
      }));
    }
  } catch (e) {
    // allow a retry, and let the admin type the service names meanwhile
    delete box.dataset.svcLoaded;
    chips.replaceChildren(
      el("span", { class: "err small", textContent: "could not list services: " + e.message }),
      el("button", { type: "button", class: "btn ghost", textContent: "Retry", onclick: () => loadGrantServices(box, t, g) }));
    (g?.services || []).forEach((n) => chips.append(el("label", {}, el("input", { type: "checkbox", class: "g-svc", value: n, checked: true }), n)));
    if (!box.querySelector(".g-svc-manual")) {
      chips.after(el("div", { class: "svc-manual" },
        el("span", { class: "muted small", textContent: "or type service names:" }),
        el("input", { class: "input mono g-svc-manual", placeholder: "api, worker" })));
    }
  }
}

function collectGrants() {
  const out = [];
  for (const box of $$("#grants .grant")) {
    if (!box.querySelector(".g-on").checked) continue;
    const actions = box.querySelector(".g-all").checked ? ["*"] : [...box.querySelectorAll(".g-act:checked")].map((c) => c.value);
    const typed = (box.querySelector(".g-svc-manual")?.value || "").split(/[\s,]+/).filter(Boolean);
    const services = [...new Set([...[...box.querySelectorAll(".g-svc:checked")].map((c) => c.value), ...typed])];
    const paths = [...box.querySelectorAll(".path-row")]
      .map((r) => ({ path: r.querySelector(".g-path").value.trim(), mode: r.querySelector(".g-mode").value }))
      .filter((p) => p.path);
    out.push({ target: box.dataset.target, actions, services, paths });
  }
  return out;
}

async function submitUser(e) {
  e.preventDefault();
  const f = $("#user-form");
  const edit = f.dataset.mode === "edit";
  const body = {
    name: f.name.value.trim(), password: f.password.value,
    admin: f.admin.checked, disabled: f.disabled.checked,
    grants: f.admin.checked ? [] : collectGrants(),
  };
  try {
    await api(edit ? "PUT" : "POST", edit ? `/api/admin/users/${enc(body.name)}` : "/api/admin/users", body, true);
    openUsers();
  } catch (err) {
    $("#user-form-error").textContent = err.message;
  }
}

async function deleteUser(name) {
  try {
    await api("DELETE", `/api/admin/users/${enc(name)}`);
    openUsers();
  } catch (err) {
    $("#user-form-error").textContent = err.message;
  }
}

/* =========================================================== files */

function fmtSize(n) {
  if (n < 1024) return n + " B";
  if (n < 1048576) return (n / 1024).toFixed(1) + " K";
  return (n / 1048576).toFixed(1) + " M";
}
const join = (dir, name) => (dir ? dir + "/" + name : name);
const fileURL = (kind, path) => `/api/targets/${enc(state.cur.name)}/${kind}?root=${state.root}&path=${enc(path)}`;

async function loadDir(path) {
  const t = state.cur, root = state.root;
  const list = $("#file-list");
  try {
    const entries = await (await api("GET", fileURL("files", path))).json();
    if (state.cur !== t || state.root !== root) return;
    state.cwd = path;
    renderCrumbs();
    $("#dl-folder").href = fileURL("download", path);
    const rows = [];
    if (path) rows.push(el("li", { onclick: () => loadDir(path.split("/").slice(0, -1).join("/")) }, el("span", { class: "fname", textContent: "../" })));
    for (const e of entries) {
      const p = join(path, e.name);
      rows.push(el("li", {
        class: state.file === p ? "active" : "",
        title: e.mod ? new Date(e.mod).toLocaleString() : "",
        onclick: () => (e.dir ? loadDir(p) : openFile(p)),
      }, el("span", { class: "fname", textContent: e.name + (e.dir ? "/" : "") + (e.link ? " ↪" : "") }),
         el("span", { class: "fsize", textContent: e.dir ? "" : fmtSize(e.size) }),
         el("a", { class: "fdl", href: fileURL("download", p), download: "", title: e.dir ? "Download folder (.tar.gz)" : "Download",
                   textContent: "⬇", onclick: (ev) => ev.stopPropagation() })));
    }
    if (!entries.length) rows.push(el("li", { class: "empty", textContent: "(empty)" }));
    list.replaceChildren(...rows);
  } catch (e) {
    list.replaceChildren(el("li", { class: "empty", textContent: e.message }));
  }
}

function renderCrumbs() {
  const parts = state.cwd ? state.cwd.split("/") : [];
  const c = $("#crumbs");
  c.replaceChildren(el("a", { textContent: curRoot().path, onclick: () => loadDir("") }));
  parts.forEach((p, i) => c.append(" / ", el("a", { textContent: p, onclick: () => loadDir(parts.slice(0, i + 1).join("/")) })));
}

const isDirty = () => state.file !== null && $("#editor").value !== state.original;

function setStatus(text, cls = "") { const s = $("#edit-status"); s.textContent = text; s.className = cls; }

function updateDirty() {
  const d = isDirty();
  $("#save-btn").disabled = !d && !state.isNew;
  $("#revert-btn").disabled = !d;
  $("#force-btn").hidden = true;
  if (d) setStatus("unsaved", "dirty");
  else if ($("#edit-status").className === "dirty") setStatus("");
}

function closeFile() {
  state.file = null; state.isNew = false; state.original = "";
  const ed = $("#editor"); ed.value = ""; ed.disabled = true;
  $("#edit-path").textContent = "Select a file";
  $("#dl-file").hidden = true;
  setStatus(""); updateDirty();
}

async function openFile(path, isNew = false) {
  if (!confirmDiscard()) return;
  const t = state.cur;
  const ed = $("#editor");
  const ro = curRoot()?.mode !== "rw";
  try {
    const text = isNew ? "" : await (await api("GET", fileURL("file", path))).text();
    if (state.cur !== t) return;
    state.file = path; state.isNew = isNew; state.original = text;
    ed.value = text; ed.disabled = false; ed.readOnly = ro;
    $("#edit-path").textContent = path + (isNew ? " (new)" : "");
    $("#dl-file").hidden = isNew;
    $("#dl-file").href = fileURL("download", path);
    updateDirty(); setStatus(ro ? "read-only" : "");
    $$("#file-list li").forEach((li) => li.classList.toggle("active", li.querySelector(".fname")?.textContent === path.split("/").pop()));
    if (!ro) ed.focus();
  } catch (e) {
    // too big / binary for the editor: still downloadable
    setStatus(e.message + (e.status === 400 ? " — use ⬇ to download it" : ""), "err");
    $("#dl-file").hidden = false;
    $("#dl-file").href = fileURL("download", path);
  }
}

async function save(force = false) {
  if (!state.file || curRoot()?.mode !== "rw" || (!isDirty() && !state.isNew)) return;
  const text = $("#editor").value;
  setStatus("saving…");
  try {
    const url = fileURL("file", state.file) + (state.isNew ? "&create=1" : "") + (force ? "&force=1" : "");
    const res = await (await api("PUT", url, text)).json();
    state.original = text; state.isNew = false;
    $("#edit-path").textContent = state.file;
    updateDirty();
    setStatus(res.backup ? "saved · previous version kept at " + res.backup : "saved", "saved");
    loadDir(state.cwd);
  } catch (e) {
    setStatus(e.message, "err");
    $("#force-btn").hidden = e.status !== 422; // content didn't parse: offer to save anyway
  }
}

/* =========================================================== wiring */

$("#add-target").addEventListener("click", () => openForm(null));
$("#empty-add").addEventListener("click", () => openForm(null));
$("#edit-target").addEventListener("click", () => openForm(state.cur));
$("#form-cancel").addEventListener("click", () => (state.cur ? select(state.cur) : loadTargets()));
$("#target-form").addEventListener("submit", submitTarget);
$("#target-form").type.addEventListener("change", syncFormType);

$("#nav-users").addEventListener("click", openUsers);
$("#user-new").addEventListener("click", () => openUserForm(null));
$("#user-cancel").addEventListener("click", () => show("users"));
$("#user-form").addEventListener("submit", submitUser);
$("#user-form").admin.addEventListener("change", syncAdminBox);

$("#root-select").addEventListener("change", (e) => {
  if (!confirmDiscard()) { e.target.value = state.root; return; }
  state.root = Number(e.target.value); syncRootMode(); closeFile(); loadDir("");
});
$("#clear-out").addEventListener("click", () => ($("#out").textContent = ""));
$("#refresh-files").addEventListener("click", () => state.cur?.roots?.length && loadDir(state.cwd));
$("#save-btn").addEventListener("click", () => save(false));
$("#force-btn").addEventListener("click", () => save(true));
$("#revert-btn").addEventListener("click", () => { $("#editor").value = state.original; updateDirty(); });
$("#editor").addEventListener("input", updateDirty);
$("#editor").addEventListener("keydown", (e) => {
  if ((e.ctrlKey || e.metaKey) && e.key === "s") { e.preventDefault(); save(false); }
  if (e.key === "Tab" && !e.ctrlKey && !e.altKey && !e.target.readOnly) {
    e.preventDefault();
    e.target.setRangeText("  ", e.target.selectionStart, e.target.selectionEnd, "end");
    updateDirty();
  }
});
$("#new-btn").addEventListener("click", () => {
  const name = $("#new-name").value.trim();
  if (!name || name.includes("..")) return;
  $("#new-name").value = "";
  openFile(join(state.cwd, name), true);
});
window.addEventListener("beforeunload", (e) => { if (isDirty() || state.running) e.preventDefault(); });

startApp();
