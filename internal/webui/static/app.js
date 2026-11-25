// BlobFish web UI. Talks only to the /v1 API with the API key the user
// pastes in. Every element is built with createElement and textContent, never
// by parsing HTML strings, so bucket names and object keys cannot inject markup.

const KEY_STORE = "blobfish.key";
const PAGE_SIZE = 200;

const state = {
  key: null,
  buckets: [],
  bucket: null,
  prefix: "",
  folders: [],
  files: [],
  cursor: "",
  // Upload queue, shown in the docked panel. Jobs run one at a time and
  // remember their own bucket, so browsing elsewhere doesn't disturb them.
  uploads: [],
  uploadsCollapsed: false,
};
let nextUploadId = 1;

// ---------- DOM helpers ----------

function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

// Simple stroked 24×24 icons, drawn for BlobFish.
const ICONS = {
  download: ["M12 4v11", "M7 10l5 5 5-5", "M5 20h14"],
  upload: ["M12 16V5", "M7 10l5-5 5 5", "M5 20h14"],
  share: ["M10 14a4 4 0 0 0 5.66 0l3-3a4 4 0 0 0-5.66-5.66l-1 1", "M14 10a4 4 0 0 0-5.66 0l-3 3a4 4 0 0 0 5.66 5.66l1-1"],
  trash: ["M4 7h16", "M9 7V4h6v3", "M6 7l1 13h10l1-13", "M10 11v6", "M14 11v6"],
  close: ["M6 6l12 12", "M18 6L6 18"],
  chevron: ["M6 9l6 6 6-6"],
  check: ["M5 12.5l4.5 4.5L19 7"],
  alert: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18", "M12 7.5v5.5", "M12 16.5v.01"],
  retry: ["M4 12a8 8 0 1 0 2.35-5.65", "M4 4v4.5h4.5"],
};

function icon(name) {
  const NS = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 24 24", width: 18, height: 18, fill: "none", stroke: "currentColor",
    "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true",
  })) svg.setAttribute(k, v);
  for (const d of ICONS[name]) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}

// iconButton is a square button whose label lives in aria-label and title.
function iconButton(name, label, onclick, extraClass = "") {
  return h("button", { class: `icon-btn ${extraClass}`.trim(), type: "button", "aria-label": label, title: label, onclick }, icon(name));
}

function toast(message, { error = false, detail = "" } = {}) {
  const el = h("div", { class: error ? "toast error" : "toast" }, message, detail && h("small", {}, detail));
  document.getElementById("toasts").append(el);
  setTimeout(() => el.remove(), error ? 8000 : 3500);
}

function showError(err) {
  if (err instanceof ApiError) {
    toast(err.message, { error: true, detail: `${err.code}${err.requestId ? " · " + err.requestId : ""}` });
  } else {
    toast(String(err.message || err), { error: true });
  }
}

const sizeFmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
function formatSize(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${i === 0 ? n : sizeFmt.format(n)} ${units[i]}`;
}
const dateFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });

// ---------- Key storage ----------
// Storage can throw (private mode, blocked site data); the UI still works,
// the user just signs in again next time.

function loadKey() {
  try { return sessionStorage.getItem(KEY_STORE) || localStorage.getItem(KEY_STORE); } catch { return null; }
}
function saveKey(key, remember) {
  try {
    sessionStorage.setItem(KEY_STORE, key);
    if (remember) localStorage.setItem(KEY_STORE, key);
  } catch { /* in-memory only */ }
}
function forgetKey() {
  try { sessionStorage.removeItem(KEY_STORE); localStorage.removeItem(KEY_STORE); } catch { /* ignore */ }
}

// ---------- API ----------

class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(message);
    this.status = status;
    this.code = code;
    this.requestId = requestId;
  }
}

async function parseError(res) {
  let e = {};
  try { e = (await res.json()).error || {}; } catch { /* not JSON */ }
  return new ApiError(res.status, e.code || `HTTP${res.status}`, e.message || res.statusText, e.requestId);
}

async function api(method, path, { body, key = state.key } = {}) {
  const headers = { Authorization: `Bearer ${key}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  if (!res.ok) {
    const err = await parseError(res);
    if (res.status === 401 && key === state.key) signOut("Your API key was rejected. Sign in again.");
    throw err;
  }
  return res.status === 204 ? null : res.json();
}

// Object keys may contain any character, so each path segment is encoded.
// Browsers normalize "." and ".." segments before sending, so keys with
// such segments can only be reached through the API with a raw client.
function objectPath(bucket, key) {
  return `/v1/b/${encodeURIComponent(bucket)}/${key.split("/").map(encodeURIComponent).join("/")}`;
}

function presign(bucket, key, method, expiresIn) {
  return api("POST", "/v1/presign", { body: { bucket, key, method, expiresIn } });
}

// ---------- Sign in ----------

function renderLogin(message = "") {
  const input = h("input", {
    type: "password", autocomplete: "off", spellcheck: "false", placeholder: "bf_…",
    "aria-label": "API key", class: "mono", required: true,
  });
  const remember = h("input", { type: "checkbox" });
  const error = h("p", { class: "error", role: "alert" }, message);
  const submit = h("button", { class: "primary", type: "submit" }, "Sign in");

  const form = h("form", {
    onsubmit: async (ev) => {
      ev.preventDefault();
      const key = input.value.trim();
      submit.disabled = true;
      error.textContent = "";
      try {
        const res = await api("GET", "/v1/buckets", { key });
        state.key = key;
        saveKey(key, remember.checked);
        state.buckets = res.buckets;
        renderApp();
      } catch (err) {
        error.textContent = err.status === 401 ? "That key was not accepted." : err.message;
      } finally {
        submit.disabled = false;
      }
    },
  },
  h("label", { class: "field" }, "API key", input),
  h("label", { class: "check" }, remember, "Remember on this device"),
  error,
  submit);

  root().replaceChildren(h("main", { class: "login" },
    h("img", { src: "favicon.svg", alt: "" }),
    h("h1", {}, "BlobFish"),
    h("p", { class: "muted" }, "Sign in with an API key. It stays in this browser and is sent only to this server."),
    form));
  input.focus();
}

function signOut(message = "") {
  for (const job of state.uploads) job.xhr?.abort();
  state.uploads = [];
  renderUploadPanel();
  forgetKey();
  Object.assign(state, { key: null, buckets: [], bucket: null, prefix: "", folders: [], files: [], cursor: "" });
  renderLogin(message);
}

// ---------- App ----------

function root() { return document.getElementById("app"); }

function renderApp() {
  root().className = "";
  root().replaceChildren(h("div", { class: "shell" },
    h("header", { class: "top" },
      h("img", { src: "favicon.svg", alt: "" }),
      h("span", { class: "brand" }, "BlobFish"),
      h("span", { class: "spacer" }),
      h("button", { class: "link", onclick: () => signOut() }, "Sign out")),
    h("nav", { class: "side", "aria-label": "Buckets" }, renderSidebar()),
    renderMain()));
}

function renderSidebar() {
  const name = h("input", { placeholder: "new-bucket", "aria-label": "New bucket name", pattern: "[a-z0-9][a-z0-9.\\-]{1,61}[a-z0-9]" });
  return [
    h("h2", {}, "Buckets"),
    state.buckets.length === 0
      ? h("p", { class: "muted", style: null }, "No buckets yet.")
      : h("ul", {}, state.buckets.map((b) =>
        h("li", {}, h("button", {
          "aria-current": b.name === state.bucket ? "true" : "false",
          onclick: () => openBucket(b.name),
        }, b.name)))),
    h("form", {
      onsubmit: async (ev) => {
        ev.preventDefault();
        const n = name.value.trim();
        if (!n) return;
        try {
          await api("PUT", `/v1/buckets/${encodeURIComponent(n)}`);
          toast(`Created bucket ${n}`);
          await refreshBuckets();
          openBucket(n);
        } catch (err) { showError(err); }
      },
    }, name, h("button", { type: "submit" }, "Add")),
  ];
}

function renderMain() {
  const main = h("section", { class: "main", "aria-label": "Objects" });
  if (!state.bucket) {
    main.append(h("p", { class: "empty" }, state.buckets.length ? "Pick a bucket to browse its files." : "Create a bucket to get started."));
    return main;
  }
  wireDrop(main);

  const fileInput = h("input", {
    type: "file", multiple: true, hidden: true,
    onchange: () => { uploadFiles([...fileInput.files]); fileInput.value = ""; },
  });

  main.append(
    h("div", { class: "toolbar" },
      renderCrumbs(),
      fileInput,
      h("button", { class: "primary with-icon", onclick: () => fileInput.click() }, icon("upload"), "Upload"),
      h("button", { class: "link danger", onclick: deleteBucket, title: "Only empty buckets can be deleted" }, "Delete bucket")),
    renderTable());
  return main;
}

function renderCrumbs() {
  const parts = state.prefix.split("/").filter(Boolean);
  const crumbs = [h("button", { onclick: () => openPrefix("") }, state.bucket)];
  parts.forEach((p, i) => {
    const prefix = parts.slice(0, i + 1).join("/") + "/";
    crumbs.push(h("span", { class: "sep" }, "/"), h("button", { onclick: () => openPrefix(prefix) }, p));
  });
  return h("nav", { class: "crumbs", "aria-label": "Folder path" }, crumbs);
}

function renderTable() {
  if (state.folders.length === 0 && state.files.length === 0) {
    return h("p", { class: "empty" }, state.prefix ? "This folder is empty." : "No files yet. Upload or drop files here.");
  }
  const rows = [
    ...state.folders.map((p) => h("tr", {},
      h("td", { class: "name" }, h("button", { onclick: () => openPrefix(p) }, p.slice(state.prefix.length))),
      h("td", { class: "num" }, "—"),
      h("td", { class: "when" }, ""),
      h("td", { class: "actions" }))),
    ...state.files.map((o) => h("tr", {},
      h("td", { class: "name", title: o.key }, o.key.slice(state.prefix.length)),
      h("td", { class: "num" }, formatSize(o.size)),
      h("td", { class: "when" }, dateFmt.format(new Date(o.modified))),
      h("td", { class: "actions" },
        iconButton("download", `Download ${baseName(o.key)}`, () => download(o.key)),
        iconButton("share", `Share ${baseName(o.key)}`, () => share(o.key)),
        iconButton("trash", `Delete ${baseName(o.key)}`, () => deleteObject(o.key), "danger")))),
  ];
  return h("div", {},
    h("table", {},
      h("thead", {}, h("tr", {},
        h("th", { scope: "col" }, "Name"),
        h("th", { scope: "col", class: "num" }, "Size"),
        h("th", { scope: "col", class: "when" }, "Modified"),
        h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
      h("tbody", {}, rows)),
    state.cursor && h("button", { class: "more", onclick: () => loadObjects(true) }, "Load more"));
}

function rerenderMain() {
  const old = document.querySelector(".main");
  if (old) old.replaceWith(renderMain());
}

// ---------- Actions ----------

async function refreshBuckets() {
  const res = await api("GET", "/v1/buckets");
  state.buckets = res.buckets;
}

async function openBucket(name) {
  state.bucket = name;
  await openPrefix("");
  renderApp(); // updates the selected bucket in the sidebar
}

async function openPrefix(prefix) {
  state.prefix = prefix;
  await loadObjects(false);
}

async function loadObjects(append) {
  const q = new URLSearchParams({ delimiter: "/", limit: String(PAGE_SIZE) });
  if (state.prefix) q.set("prefix", state.prefix);
  if (append && state.cursor) q.set("cursor", state.cursor);
  try {
    const res = await api("GET", `/v1/b/${encodeURIComponent(state.bucket)}?${q}`);
    state.folders = append ? state.folders.concat(res.prefixes) : res.prefixes;
    state.files = append ? state.files.concat(res.objects) : res.objects;
    state.cursor = res.nextCursor || "";
  } catch (err) {
    showError(err);
    if (err.code === "NoSuchBucket") { state.bucket = null; await refreshBuckets().catch(() => {}); renderApp(); return; }
  }
  rerenderMain();
}

async function download(key) {
  try {
    // A short-lived presigned URL lets the browser stream the file itself,
    // without the API key in the link.
    const { url } = await presign(state.bucket, key, "GET", 300);
    const a = h("a", { href: url, download: baseName(key), rel: "noreferrer" });
    document.body.append(a);
    a.click();
    a.remove();
  } catch (err) { showError(err); }
}

async function share(key) {
  const dialog = document.getElementById("share");
  const ttl = document.getElementById("share-ttl");
  const out = document.getElementById("share-url");
  const make = async () => {
    out.value = "Creating link…";
    try {
      out.value = (await presign(state.bucket, key, "GET", Number(ttl.value))).url;
    } catch (err) { out.value = ""; showError(err); }
  };
  ttl.onchange = make;
  document.getElementById("share-copy").onclick = async () => {
    try {
      await navigator.clipboard.writeText(out.value);
      toast("Link copied");
    } catch {
      out.select(); // clipboard API needs HTTPS; let the user copy by hand
      toast("Press Ctrl+C (⌘C) to copy");
    }
  };
  dialog.showModal();
  await make();
}

async function deleteObject(key) {
  if (!confirm(`Delete "${key}"? This cannot be undone.`)) return;
  try {
    await api("DELETE", objectPath(state.bucket, key));
    toast("Deleted");
    await loadObjects(false);
  } catch (err) { showError(err); }
}

async function deleteBucket() {
  if (!confirm(`Delete bucket "${state.bucket}"? It must be empty.`)) return;
  try {
    await api("DELETE", `/v1/buckets/${encodeURIComponent(state.bucket)}`);
    toast(`Deleted bucket ${state.bucket}`);
    state.bucket = null;
    await refreshBuckets();
    renderApp();
  } catch (err) { showError(err); }
}

// ---------- Uploads ----------

function wireDrop(el) {
  let depth = 0;
  el.addEventListener("dragenter", (e) => { e.preventDefault(); depth++; el.classList.add("dragging"); });
  el.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; el.classList.remove("dragging"); } });
  el.addEventListener("dragover", (e) => e.preventDefault());
  el.addEventListener("drop", (e) => {
    e.preventDefault();
    depth = 0;
    el.classList.remove("dragging");
    const files = [...(e.dataTransfer?.files || [])];
    if (files.length) uploadFiles(files);
  });
}

function uploadFiles(files) {
  for (const file of files) {
    state.uploads.push({
      id: nextUploadId++, file, bucket: state.bucket, key: state.prefix + file.name,
      status: "queued", progress: 0, error: null, xhr: null,
    });
  }
  state.uploadsCollapsed = false;
  renderUploadPanel();
  pumpUploads();
}

let pumping = false;

// pumpUploads runs queued jobs one at a time: a small server prefers a
// steady stream to a stampede of parallel multi-GB uploads.
async function pumpUploads() {
  if (pumping) return;
  pumping = true;
  try {
    let job;
    while ((job = state.uploads.find((u) => u.status === "queued"))) {
      job.status = "uploading";
      renderUploadPanel();
      try {
        await putFile(job);
        job.status = "done";
        job.doneAt = Date.now();
        // Fade finished rows out shortly after, then drop them.
        setTimeout(() => removeUpload(job.id), 2500);
        if (job.bucket === state.bucket && dirOf(job.key) === state.prefix) loadObjects(false);
      } catch (err) {
        if (job.status === "cancelled") continue;
        job.status = "failed";
        job.error = err instanceof ApiError ? `${err.message} (${err.code})` : err.message;
      }
      renderUploadPanel();
    }
  } finally {
    pumping = false;
  }
}

function removeUpload(id) {
  const before = state.uploads.length;
  state.uploads = state.uploads.filter((u) => u.id !== id);
  if (state.uploads.length === before) return;
  if (state.uploads.length === 0 && state.finishedCount) {
    toast(state.finishedCount === 1 ? "1 file uploaded" : `${state.finishedCount} files uploaded`);
    state.finishedCount = 0;
  }
  renderUploadPanel();
}

function cancelUpload(job) {
  if (job.status === "uploading") job.xhr?.abort();
  job.status = "cancelled";
  removeUpload(job.id);
}

function retryUpload(job) {
  Object.assign(job, { status: "queued", progress: 0, error: null });
  renderUploadPanel();
  pumpUploads();
}

function renderUploadPanel() {
  const panel = document.getElementById("uploads");
  const jobs = state.uploads;
  if (jobs.length === 0) {
    panel.hidden = true;
    panel.replaceChildren();
    return;
  }
  const active = jobs.filter((u) => u.status === "queued" || u.status === "uploading").length;
  const failed = jobs.filter((u) => u.status === "failed").length;
  const total = jobs.length;
  // Count finished files too, even after their rows fade away, so the
  // "n of m" never jumps backwards mid-batch.
  const finished = state.finishedCount || 0;
  let title;
  if (active) title = `Uploading ${finished + 1} of ${finished + active}`;
  else if (failed) title = failed === 1 ? "1 upload failed" : `${failed} uploads failed`;
  else title = total === 1 ? "Upload complete" : `${total} uploads complete`;

  panel.hidden = false;
  panel.classList.toggle("collapsed", state.uploadsCollapsed);
  panel.replaceChildren(
    h("header", {},
      h("strong", {}, title),
      iconButton("chevron", state.uploadsCollapsed ? "Expand uploads" : "Collapse uploads", () => {
        state.uploadsCollapsed = !state.uploadsCollapsed;
        renderUploadPanel();
      }, "flip"),
      !active && iconButton("close", "Close", () => { state.uploads = []; state.finishedCount = 0; renderUploadPanel(); })),
    !state.uploadsCollapsed && h("ul", {}, jobs.map(renderUploadRow)));
}

function renderUploadRow(job) {
  const where = job.bucket + "/" + dirOf(job.key);
  let status;
  switch (job.status) {
    case "queued":
      status = [h("span", { class: "muted" }, "Waiting"), iconButton("close", `Cancel ${baseName(job.key)}`, () => cancelUpload(job))];
      break;
    case "uploading":
      status = [
        h("span", { class: "muted pct" }, job.progress >= 1 ? "Saving…" : `${Math.floor(job.progress * 100)}%`),
        iconButton("close", `Cancel ${baseName(job.key)}`, () => cancelUpload(job)),
      ];
      break;
    case "done":
      status = [h("span", { class: "ok", "aria-label": "Uploaded" }, icon("check"))];
      break;
    case "failed":
      status = [
        h("span", { class: "bad", title: job.error }, icon("alert")),
        iconButton("retry", `Retry ${baseName(job.key)}`, () => retryUpload(job)),
        iconButton("close", `Dismiss ${baseName(job.key)}`, () => removeUpload(job.id)),
      ];
      break;
  }
  const li = h("li", { class: `job ${job.status}` },
    h("div", { class: "job-text" },
      h("span", { class: "job-name", title: job.key }, baseName(job.key)),
      h("span", { class: "job-sub" }, job.status === "failed" ? job.error : `${formatSize(job.file.size)} · ${where}`)),
    h("div", { class: "job-status" }, status),
    job.status === "uploading" && h("progress", { max: 1, value: job.progress }));
  if (job.status === "done") {
    // Rows are rebuilt on every progress tick; keep the fade-out anchored to
    // when this job finished rather than restarting it. (CSSOM writes are
    // allowed under the CSP; style attributes in markup are not.)
    li.style.animationDelay = `${1900 - (Date.now() - job.doneAt)}ms`;
  }
  return li;
}

// XMLHttpRequest rather than fetch: it reports upload progress and aborts.
function putFile(job) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    job.xhr = xhr;
    xhr.open("PUT", objectPath(job.bucket, job.key));
    xhr.setRequestHeader("Authorization", `Bearer ${state.key}`);
    xhr.setRequestHeader("Content-Type", job.file.type || "application/octet-stream");
    let last = 0;
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      job.progress = e.loaded / e.total;
      const now = performance.now();
      if (now - last > 200 || job.progress >= 1) { last = now; renderUploadPanel(); }
    };
    xhr.onload = () => {
      job.xhr = null;
      if (xhr.status >= 200 && xhr.status < 300) {
        state.finishedCount = (state.finishedCount || 0) + 1;
        resolve();
        return;
      }
      let e = {};
      try { e = JSON.parse(xhr.responseText).error || {}; } catch { /* not JSON */ }
      reject(new ApiError(xhr.status, e.code || `HTTP${xhr.status}`, e.message || "Upload failed", e.requestId));
    };
    xhr.onerror = () => { job.xhr = null; reject(new Error("Network error")); };
    xhr.onabort = () => { job.xhr = null; reject(new Error("Cancelled")); };
    xhr.send(job.file);
  });
}

function baseName(key) { return key.slice(key.lastIndexOf("/") + 1) || key; }
function dirOf(key) { return key.slice(0, key.lastIndexOf("/") + 1); }

// Closing the tab kills running uploads; ask first.
window.addEventListener("beforeunload", (e) => {
  if (state.uploads.some((u) => u.status === "uploading" || u.status === "queued")) {
    e.preventDefault();
    e.returnValue = ""; // older browsers need this to show the prompt
  }
});

// ---------- Start ----------

async function start() {
  const key = loadKey();
  if (!key) { renderLogin(); return; }
  try {
    const res = await api("GET", "/v1/buckets", { key });
    state.key = key;
    state.buckets = res.buckets;
    renderApp();
  } catch (err) {
    forgetKey();
    renderLogin(err.status === 401 ? "Your saved key no longer works. Sign in again." : err.message);
  }
}

start();
