// The file browser: one folder of one bucket, a page at a time.

import { api, bucketPath, loadPref, objectPath, presign, savePref, showError } from "./api.js";
import {
  ask, baseName, confirmDialog, copyText, dateFmt, dateFmtLong, formatInt, formatSize, h, icon, iconButton,
  relativeTime, toast,
} from "./dom.js";
import { navigate } from "./router.js";
import { PAGE_SIZES, bucketInfo, can, isServerAdmin, rerenderMain, state, ui } from "./state.js";
import { pickAndUpload, wireDrop } from "./uploads.js";

// ---------- Loading ----------

function pageSize(route) {
  return route.size || Number(loadPref("pageSize", 100)) || 100;
}

async function fetchPage(bucket, prefix, size, cursor) {
  const q = new URLSearchParams({ delimiter: "/", limit: String(size) });
  if (prefix) q.set("prefix", prefix);
  if (cursor) q.set("cursor", cursor);
  return api("GET", `${bucketPath(bucket)}?${q}`);
}

// showRoute loads the page the route asks for. With a fresh listing (new
// folder, a reload, a bookmark to page 4) it walks pages 1..n-1 first to
// learn their cursors; the API pages by cursor, not by number.
export async function showRoute(route) {
  const L = state.listing;
  const size = pageSize(route);
  const same = L.bucket === route.bucket && L.prefix === route.prefix && L.size === size;
  if (!same) {
    Object.assign(L, {
      bucket: route.bucket, prefix: route.prefix, size,
      cursors: [""], page: 1, folders: [], files: [], nextCursor: "", loaded: false,
    });
  }
  let page = route.page;
  try {
    // Learn cursors up to the wanted page, if we don't have them yet.
    while (L.cursors.length < page) {
      const res = await fetchPage(L.bucket, L.prefix, size, L.cursors[L.cursors.length - 1]);
      if (!res.nextCursor) break;
      L.cursors.push(res.nextCursor);
    }
    if (page > L.cursors.length) {
      page = L.cursors.length; // the folder has fewer pages than the link said
      navigate({ ...route, page }, { replace: true });
      return;
    }
    const res = await fetchPage(L.bucket, L.prefix, size, L.cursors[page - 1]);
    if (page > 1 && res.objects.length === 0 && res.prefixes.length === 0) {
      navigate({ ...route, page: page - 1 }, { replace: true }); // emptied by deletes
      return;
    }
    Object.assign(L, { page, folders: res.prefixes, files: res.objects, nextCursor: res.nextCursor || "", loaded: true });
    // Fresh usage numbers for the toolbar (uploads, deletes and restores
    // change them). Not worth an error toast if this one fails.
    try { state.buckets = (await api("GET", "/v1/buckets")).buckets; } catch { /* keep the old numbers */ }
    if (res.nextCursor) L.cursors[page] = res.nextCursor;
    L.cursors.length = page + (res.nextCursor ? 1 : 0);
  } catch (err) {
    showError(err);
    if (err.code === "NoSuchBucket") {
      navigate({ view: "home" }, { replace: true });
      return;
    }
  }
  rerenderMain();
}

// reload refreshes the current page in place (after an upload or delete).
export function reload() {
  const r = state.route;
  if (r.view === "files") showRoute(r);
}

function go(changes) {
  navigate({ ...state.route, ...changes });
}

// ---------- Rendering ----------

export function renderFiles() {
  const r = state.route;
  const L = state.listing;
  const main = h("section", { class: "main", "aria-label": "Files" });
  const canWrite = can("w", r.bucket);
  if (canWrite) wireDrop(main);

  const isPublic = bucketInfo(r.bucket)?.public;
  main.append(
    h("div", { class: "toolbar" },
      renderCrumbs(),
      isPublic && h("span", { class: "chip public-chip", title: "Files in this bucket can be read without a key" },
        icon("globe", 14), "Public"),
      corsChip(r.bucket),
      usageChip(r.bucket),
      h("div", { class: "toolbar-actions" },
        canWrite && !r.prefix && h("button", {
          class: "link with-icon", title: "Deleted files you can still restore",
          onclick: () => navigate({ view: "trash", bucket: r.bucket }),
        }, icon("trash"), "Trash"),
        canWrite && h("button", { class: "with-icon", onclick: newFolder }, icon("folderPlus"), "New folder"),
        canWrite && h("button", { class: "with-icon", onclick: () => pickAndUpload({ folder: true }) }, icon("folder"), "Upload folder"),
        canWrite && h("button", { class: "primary with-icon", onclick: () => pickAndUpload({ folder: false }) }, icon("upload"), "Upload"),
        can("admin", r.bucket) && !r.prefix && h("button", {
          class: "link", onclick: () => setVisibility(!isPublic),
        }, isPublic ? "Make private" : "Make public"),
        can("admin", r.bucket) && !r.prefix && h("button", {
          class: "link", onclick: () => editBrowserAccess(r.bucket),
          title: "Websites whose pages may upload to and download from this bucket directly",
        }, "Browser access"),
        isServerAdmin() && !r.prefix && h("button", {
          class: "link", onclick: () => editQuota(r.bucket),
          title: "Limit how much this bucket may store",
        }, "Quota"),
        can("admin", r.bucket) && !r.prefix && h("button", {
          class: "link danger", onclick: deleteBucket, title: "Only empty buckets can be deleted",
        }, "Delete bucket"))),
    L.loaded ? renderTable(canWrite) : h("p", { class: "empty" }, "Loading…"),
    L.loaded && renderPager());
  return main;
}

function renderCrumbs() {
  const r = state.route;
  const parts = r.prefix.split("/").filter(Boolean);
  const crumbs = [h("button", { onclick: () => go({ prefix: "", page: 1 }) }, r.bucket)];
  parts.forEach((p, i) => {
    const prefix = parts.slice(0, i + 1).join("/") + "/";
    const last = i === parts.length - 1;
    crumbs.push(h("span", { class: "sep" }, "/"),
      h("button", { onclick: () => go({ prefix, page: 1 }), "aria-current": last ? "page" : null }, p));
  });
  return h("nav", { class: "crumbs", "aria-label": "Folder path" }, crumbs);
}

function renderTable(canWrite) {
  const r = state.route;
  const L = state.listing;
  if (L.folders.length === 0 && L.files.length === 0) {
    if (r.prefix) {
      return h("div", { class: "empty" },
        h("p", {}, "This folder is empty."),
        canWrite && h("p", { class: "muted" }, "Folders exist only while they hold files: upload or drop something here and it will stay."));
    }
    return h("p", { class: "empty" }, canWrite ? "No files yet. Upload or drop files here." : "No files yet.");
  }
  const rows = [
    ...L.folders.map((p) => h("tr", {},
      h("td", { class: "name" }, h("button", { class: "with-icon", onclick: () => go({ prefix: p, page: 1 }) },
        icon("folder"), p.slice(r.prefix.length))),
      h("td", { class: "num" }, "—"),
      h("td", { class: "when" }, ""),
      h("td", { class: "actions" }))),
    ...L.files.map((o) => {
      const name = o.key.slice(r.prefix.length);
      return h("tr", {},
        h("td", { class: "name", title: o.key },
          h("button", { class: "with-icon file-link", onclick: () => openFile(o), title: `Open ${name} in a new tab` }, icon("file"), name)),
        h("td", { class: "num" }, formatSize(o.size)),
        h("td", { class: "when" }, dateFmt.format(new Date(o.modified))),
        h("td", { class: "actions" },
          iconButton("info", `Details for ${name}`, () => showDetails(o)),
          iconButton("download", `Download ${name}`, () => download(o)),
          iconButton("share", `Share ${name}`, () => share(o)),
          canWrite && iconButton("trash", `Delete ${name}`, () => deleteObject(o), "danger")));
    }),
  ];
  return h("table", {},
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Name"),
      h("th", { scope: "col", class: "num" }, "Size"),
      h("th", { scope: "col", class: "when" }, "Modified"),
      h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
    h("tbody", {}, rows));
}

function renderPager() {
  const L = state.listing;
  const count = L.folders.length + L.files.length;
  const first = (L.page - 1) * L.size + 1;
  const hasPrev = L.page > 1;
  const hasNext = Boolean(L.nextCursor);
  const sizeSelect = h("select", {
    "aria-label": "Items per page",
    onchange: (e) => {
      savePref("pageSize", e.target.value);
      go({ size: Number(e.target.value), page: 1 });
    },
  }, PAGE_SIZES.map((n) => h("option", { value: n }, String(n))));
  sizeSelect.value = String(L.size);

  return h("nav", { class: "pager", "aria-label": "Pages" },
    h("span", { class: "muted" }, count ? `Items ${formatInt(first)}–${formatInt(first + count - 1)}` : ""),
    h("span", { class: "spacer" }),
    h("label", { class: "pager-size muted" }, "Per page", sizeSelect),
    (hasPrev || hasNext) && h("div", { class: "pager-nav" },
      h("button", { class: "with-icon", disabled: !hasPrev, onclick: () => go({ page: L.page - 1 }) }, icon("left"), "Prev"),
      h("span", { class: "pager-page", "aria-current": "page" }, `Page ${L.page}`),
      h("button", { class: "with-icon", disabled: !hasNext, onclick: () => go({ page: L.page + 1 }) }, "Next", icon("right"))));
}

// ---------- Actions ----------

// openFile shows a file in a new tab through a short-lived presigned URL,
// so images, PDFs and video display in the browser. The tab is opened
// before the request so popup blockers see it as part of the click.
async function openFile(o) {
  const tab = window.open("", "_blank");
  try {
    const { url } = await presign(state.route.bucket, o.key, "GET", 300);
    if (!tab) {
      toast("Your browser blocked the new tab. Allow pop-ups for this site, or use Download.", { error: true });
      return;
    }
    tab.opener = null;
    tab.location.href = url;
  } catch (err) {
    tab?.close();
    showError(err);
  }
}

async function download(o) {
  try {
    // A short-lived presigned URL lets the browser stream the file itself,
    // without the API key in the link.
    const { url } = await presign(state.route.bucket, o.key, "GET", 300);
    // ?download makes the server send Content-Disposition: attachment, so
    // the browser saves the file even where it ignores the download attribute.
    const a = h("a", { href: `${url}&download`, download: baseName(o.key), rel: "noreferrer" });
    document.body.append(a);
    a.click();
    a.remove();
  } catch (err) { showError(err); }
}

function publicURL(key) {
  return location.origin + objectPath(state.route.bucket, key);
}

// share opens the share dialog: the public URL when the bucket is public,
// the file's share links (copy any time, revoke), and a form for new ones.
async function share(o) {
  const bucket = state.route.bucket;
  const dialog = document.getElementById("share");
  const list = document.getElementById("share-links");
  document.getElementById("share-title").textContent = `Share “${baseName(o.key)}”`;

  const pub = document.getElementById("share-public");
  pub.hidden = !bucketInfo(bucket)?.public;
  if (!pub.hidden) {
    const field = document.getElementById("share-public-url");
    field.value = publicURL(o.key);
    document.getElementById("share-public-copy").onclick = () => copyText(field.value, "Public link copied", field);
  }

  let links = [];
  const render = () => {
    if (links.length === 0) {
      list.replaceChildren(h("li", { class: "muted share-none" }, "No share links yet."));
      return;
    }
    list.replaceChildren(...links.map((l) => {
      const field = h("input", { class: "mono", readonly: true, value: l.url, "aria-label": "Share link" });
      const expires = l.expires ? new Date(l.expires) : null;
      const expired = expires && expires < new Date();
      const mayRevoke = l.createdBy === state.me.keyId || can("w", bucket);
      return h("li", { class: expired ? "share-link expired" : "share-link" },
        h("div", { class: "share-link-row" },
          field,
          iconButton("copy", "Copy link", () => copyText(l.url, "Link copied", field)),
          mayRevoke && iconButton("trash", "Revoke link", () => revoke(l), "danger")),
        h("span", { class: "share-meta" },
          l.filename !== baseName(o.key) && `Saved as “${l.filename}” · `,
          expires ? (expired ? `Expired ${relativeTime(expires)}` : `Expires ${relativeTime(expires)}`) : "Never expires",
          ` · created ${relativeTime(new Date(l.created))}`,
          l.createdByName && ` by ${l.createdByName}`));
    }));
  };
  const load = async () => {
    try {
      const q = new URLSearchParams({ bucket, key: o.key });
      links = (await api("GET", `/v1/links?${q}`)).links;
    } catch (err) { showError(err); links = []; }
    render();
  };
  const revoke = async (l) => {
    const ok = await confirmDialog({
      title: "Revoke this link?",
      body: [h("p", { class: "mono confirm-target" }, l.url), "Anyone using it loses access immediately."],
      confirmLabel: "Revoke link",
    });
    if (!ok) return;
    try {
      await api("DELETE", `/v1/links/${encodeURIComponent(l.id)}`);
      toast("Link revoked");
    } catch (err) { showError(err); }
    await load();
  };

  // The name the file opens and saves under; left as is, the link uses the
  // file's own name.
  const nameField = document.getElementById("share-name");
  nameField.value = baseName(o.key);
  const create = document.getElementById("share-create");
  create.onclick = async () => {
    const ttl = Number(document.getElementById("share-ttl").value);
    const name = nameField.value.trim();
    const body = { bucket, key: o.key, expiresIn: ttl };
    if (name && name !== baseName(o.key)) body.filename = name;
    create.disabled = true;
    try {
      const l = await api("POST", "/v1/links", { body });
      await load();
      await copyText(l.url, ttl === 0 ? "Permanent link created and copied" : "Link created and copied");
    } catch (err) {
      showError(err);
    } finally {
      create.disabled = false;
    }
  };

  list.replaceChildren(h("li", { class: "muted share-none" }, "Loading…"));
  dialog.showModal();
  await load();
}

// usageChip shows what the bucket's files take, against its quota if it
// has one. Trash is not counted, matching the server.
function usageChip(bucket) {
  const b = bucketInfo(bucket);
  if (!b) return null;
  const files = `${formatInt(b.objects)} file${b.objects === 1 ? "" : "s"}`;
  if (!b.quotaBytes) {
    return h("span", { class: "chip usage-chip", title: "Space used by this bucket's files (trash not counted)" },
      `${formatSize(b.usedBytes)} · ${files}`);
  }
  const pct = Math.min(100, Math.round((b.usedBytes / b.quotaBytes) * 100));
  return h("span", {
    class: pct >= 90 ? "chip usage-chip near-full" : "chip usage-chip",
    title: `${files}, ${pct}% of the quota (trash not counted). Uploads that don't fit are refused.`,
  }, `${formatSize(b.usedBytes)} of ${formatSize(b.quotaBytes)}`);
}

// editQuota lets a server admin cap the bucket's size, in GB.
async function editQuota(bucket) {
  const GB = 1024 ** 3;
  const current = bucketInfo(bucket)?.quotaBytes || 0;
  const res = await ask({
    title: `Quota for “${bucket}”`,
    body: [h("p", { class: "muted" },
      "Uploads that would take the bucket past this size are refused. Files in trash don't count. Leave empty for no limit.")],
    input: {
      label: "Maximum size in GB",
      value: current ? String(+(current / GB).toFixed(3)) : "",
      placeholder: "no limit",
      validate: (v) => (v === "" || (Number.isFinite(Number(v)) && Number(v) >= 0) ? "" : "Enter a number of GB, or leave empty."),
    },
    choices: [{ label: "Cancel", value: "" }, { label: "Save", value: "ok", kind: "primary" }],
  });
  if (!res) return;
  const quotaBytes = res.value === "" ? 0 : Math.round(Number(res.value) * GB);
  try {
    await api("PATCH", `/v1/buckets/${encodeURIComponent(bucket)}`, { body: { quotaBytes } });
    state.buckets = (await api("GET", "/v1/buckets")).buckets;
    toast(quotaBytes ? `Quota set to ${formatSize(quotaBytes)}` : "Quota removed");
    rerenderMain();
  } catch (err) { showError(err); }
}

function corsChip(bucket) {
  const origins = bucketInfo(bucket)?.corsOrigins || [];
  if (origins.length === 0) return null;
  const label = origins.includes("*") ? "Browser: any site" : `Browser: ${origins.length} site${origins.length === 1 ? "" : "s"}`;
  return h("span", { class: "chip cors-chip", title: `Web pages on these sites may call this bucket:\n${origins.join("\n")}` },
    icon("globe", 14), label);
}

// originOf returns the exact origin a browser would send for what the user
// typed, or an error message. "*" means any site.
function originOf(input) {
  const v = input.trim().toLowerCase();
  if (v === "*") return { origin: "*" };
  let u;
  try { u = new URL(v); } catch { return { error: "Enter a full address like https://app.example.com." }; }
  if (u.protocol !== "https:" && u.protocol !== "http:") return { error: "Only http:// and https:// sites can be added." };
  if (v.replace(/\/$/, "") !== u.origin || u.username || u.search || u.hash) {
    return { error: `Use just the site, without a path: ${u.origin}` };
  }
  return { origin: u.origin };
}

// editBrowserAccess edits which websites' pages may call this bucket from a
// browser, e.g. to upload straight to a presigned URL from your app.
async function editBrowserAccess(bucket) {
  const dialog = document.getElementById("cors");
  const list = document.getElementById("cors-list");
  const input = document.getElementById("cors-input");
  const error = document.getElementById("cors-error");
  const warn = document.getElementById("cors-warn");
  let origins = [...(bucketInfo(bucket)?.corsOrigins || [])];

  document.getElementById("cors-title").textContent = `Browser access to “${bucket}”`;
  const render = () => {
    list.replaceChildren(...(origins.length
      ? origins.map((o) => h("li", {},
        h("span", { class: "mono" }, o === "*" ? "* (any website)" : o),
        iconButton("close", `Remove ${o}`, () => { origins = origins.filter((x) => x !== o); render(); })))
      : [h("li", { class: "muted" }, "No websites: pages on other sites can't call this bucket.")]));
    warn.hidden = !origins.includes("*");
  };
  const add = () => {
    const { origin, error: problem } = originOf(input.value);
    error.textContent = problem || "";
    if (!origin) return;
    if (!origins.includes(origin)) origins = [...origins, origin].sort();
    input.value = "";
    render();
    input.focus();
  };
  document.getElementById("cors-add").onclick = add;
  input.onkeydown = (e) => { if (e.key === "Enter") { e.preventDefault(); add(); } };
  document.getElementById("cors-save").onclick = async () => {
    if (input.value.trim()) { add(); if (error.textContent) return; } // typed but not added yet
    try {
      await api("PATCH", `/v1/buckets/${encodeURIComponent(bucket)}`, { body: { corsOrigins: origins } });
      state.buckets = (await api("GET", "/v1/buckets")).buckets;
      dialog.close();
      toast(origins.length ? `Browser access saved for ${bucket}` : `Browser access turned off for ${bucket}`);
      ui.renderApp();
    } catch (err) { showError(err); }
  };
  input.value = "";
  error.textContent = "";
  render();
  dialog.showModal();
  input.focus();
}

async function setVisibility(makePublic) {
  const bucket = state.route.bucket;
  const ok = await confirmDialog(makePublic ? {
    title: `Make “${bucket}” public?`,
    body: [
      "Anyone who has, or guesses, a file's address can download it without a key. Listing, uploading and deleting still need a key.",
      "Use this for website or app assets, not for private documents.",
    ],
    confirmLabel: "Make public",
  } : {
    title: `Make “${bucket}” private?`,
    body: ["Public addresses to its files stop working immediately. Share links keep working."],
    confirmLabel: "Make private",
    danger: false,
  });
  if (!ok) return;
  try {
    await api("PATCH", `/v1/buckets/${encodeURIComponent(bucket)}`, { body: { public: makePublic } });
    state.buckets = (await api("GET", "/v1/buckets")).buckets;
    toast(makePublic ? `${bucket} is now public` : `${bucket} is now private`);
    ui.renderApp();
  } catch (err) { showError(err); }
}

function showDetails(o) {
  const dialog = document.getElementById("details");
  const row = (label, ...value) => [h("dt", {}, label), h("dd", {}, ...value)];
  const meta = Object.entries(o.meta || {});
  document.getElementById("details-title").textContent = baseName(o.key);
  document.getElementById("details-body").replaceChildren(h("dl", { class: "details" },
    row("Key", h("span", { class: "mono break" }, o.key)),
    row("Size", `${formatSize(o.size)} `, h("span", { class: "muted" }, `(${formatInt(o.size)} bytes)`)),
    row("Type", h("span", { class: "mono" }, o.contentType)),
    row("Modified", dateFmtLong.format(new Date(o.modified))),
    bucketInfo(state.route.bucket)?.public && row("Public URL",
      h("span", { class: "mono break" }, publicURL(o.key)),
      iconButton("copy", "Copy public URL", () => copyText(publicURL(o.key), "Public URL copied"), "inline-copy")),
    row("SHA-256",
      h("span", { class: "mono break" }, o.etag),
      iconButton("copy", "Copy SHA-256", () => copyText(o.etag, "Checksum copied"), "inline-copy")),
    row("Metadata", meta.length
      ? h("table", { class: "meta-table" }, h("tbody", {}, meta.map(([k, v]) =>
        h("tr", {}, h("th", { scope: "row", class: "mono" }, `X-Meta-${k}`), h("td", { class: "mono break" }, v)))))
      : h("span", { class: "muted" }, "None"))));
  const actions = document.getElementById("details-actions");
  actions.replaceChildren(
    h("button", { type: "button", class: "with-icon", onclick: () => { dialog.close(); download(o); } }, icon("download"), "Download"),
    h("button", { type: "button", class: "with-icon", onclick: () => { dialog.close(); share(o); } }, icon("share"), "Share"),
    h("button", { value: "close", class: "primary" }, "Close"));
  dialog.showModal();
}

async function deleteObject(o) {
  const days = state.me.trashDays;
  const ok = await confirmDialog({
    title: days > 0 ? "Move this file to trash?" : "Delete this file?",
    body: [h("p", { class: "mono confirm-target" }, o.key), days > 0
      ? `You can restore it from the bucket's trash for ${days} day${days === 1 ? "" : "s"}. Its share links stop working until then.`
      : "This can't be undone."],
    confirmLabel: days > 0 ? "Move to trash" : "Delete",
  });
  if (!ok) return;
  try {
    await api("DELETE", objectPath(state.route.bucket, o.key));
    toast(days > 0 ? "Moved to trash" : "Deleted");
    reload();
  } catch (err) { showError(err); }
}

// Folders are prefixes: a "new folder" is a place to upload into. It
// appears in its parent's listing once it holds a file.
async function newFolder() {
  const res = await ask({
    title: "New folder",
    body: [h("p", { class: "muted" }, "Folders in BlobFish are part of file names, so the folder is kept once you upload something into it.")],
    input: {
      label: "Folder name",
      placeholder: "e.g. invoices or invoices/2026",
      validate: (v) => {
        const name = v.replace(/^\/+|\/+$/g, "");
        if (!name) return "Enter a name.";
        const parts = name.split("/");
        if (parts.some((p) => p === "")) return "Folder names can't contain empty parts like “a//b”.";
        if (parts.some((p) => p === "." || p === "..")) return "“.” and “..” can't be used as folder names.";
        return "";
      },
    },
    choices: [{ label: "Cancel", value: "" }, { label: "Create", value: "ok", kind: "primary" }],
  });
  if (!res) return;
  const name = res.value.replace(/^\/+|\/+$/g, "");
  go({ prefix: state.route.prefix + name + "/", page: 1 });
}

async function deleteBucket() {
  const bucket = state.route.bucket;
  const ok = await confirmDialog({
    title: `Delete bucket “${bucket}”?`,
    body: ["Only an empty bucket can be deleted, with an empty trash. This can't be undone."],
    confirmLabel: "Delete bucket",
  });
  if (!ok) return;
  try {
    await api("DELETE", `/v1/buckets/${encodeURIComponent(bucket)}`);
    toast(`Deleted bucket ${bucket}`);
    state.buckets = (await api("GET", "/v1/buckets")).buckets;
    navigate({ view: "home" });
    ui.renderApp();
  } catch (err) { showError(err); }
}
