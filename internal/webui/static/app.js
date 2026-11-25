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
  uploads: [],
};

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
      h("button", { class: "primary", onclick: () => fileInput.click() }, "Upload"),
      h("button", { class: "link danger", onclick: deleteBucket, title: "Only empty buckets can be deleted" }, "Delete bucket")),
    renderUploads(),
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

function renderUploads() {
  if (state.uploads.length === 0) return h("div", { class: "uploads", hidden: true });
  return h("div", { class: "uploads" }, state.uploads.map((u) =>
    h("div", { class: u.failed ? "upload failed" : "upload" },
      h("span", { class: "name", title: u.key }, u.key),
      h("progress", { max: 1, value: u.progress }),
      h("span", { class: "state muted" }, u.failed ? "Failed" : u.progress >= 1 ? "Done" : `${Math.round(u.progress * 100)}%`))));
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
        h("button", { class: "link", onclick: () => download(o.key) }, "Download"),
        h("button", { class: "link", onclick: () => share(o.key) }, "Share"),
        h("button", { class: "link danger", onclick: () => deleteObject(o.key) }, "Delete")))),
  ];
  return h("div", {},
    h("table", {},
      h("thead", {}, h("tr", {},
        h("th", { scope: "col" }, "Name"),
        h("th", { scope: "col", class: "num" }, "Size"),
        h("th", { scope: "col", class: "when" }, "Modified"),
        h("th", { scope: "col" }, h("span", { class: "muted" }, "")))),
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
  state.uploads = [];
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
    const a = h("a", { href: url, download: key.split("/").pop(), rel: "noreferrer" });
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

async function uploadFiles(files) {
  const bucket = state.bucket;
  const jobs = files.map((f) => ({ file: f, key: state.prefix + f.name, progress: 0, failed: false }));
  state.uploads = jobs.concat(state.uploads.filter((u) => !u.failed && u.progress < 1));
  rerenderMain();
  // One at a time: a small server prefers a steady stream to a stampede.
  for (const job of jobs) {
    try {
      await putFile(bucket, job);
    } catch (err) {
      job.failed = true;
      showError(err);
    }
    if (state.bucket === bucket) rerenderMain();
  }
  if (state.bucket === bucket) await loadObjects(false);
}

// XMLHttpRequest rather than fetch: it reports upload progress.
function putFile(bucket, job) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", objectPath(bucket, job.key));
    xhr.setRequestHeader("Authorization", `Bearer ${state.key}`);
    xhr.setRequestHeader("Content-Type", job.file.type || "application/octet-stream");
    let last = 0;
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      job.progress = e.loaded / e.total;
      const now = performance.now();
      if (now - last > 150 && state.bucket === bucket) { last = now; rerenderMain(); }
    };
    xhr.onload = () => {
      if (xhr.status >= 200 && xhr.status < 300) { job.progress = 1; resolve(); return; }
      let e = {};
      try { e = JSON.parse(xhr.responseText).error || {}; } catch { /* not JSON */ }
      reject(new ApiError(xhr.status, e.code || `HTTP${xhr.status}`, e.message || "Upload failed", e.requestId));
    };
    xhr.onerror = () => reject(new Error(`Network error uploading ${job.key}`));
    xhr.send(job.file);
  });
}

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
