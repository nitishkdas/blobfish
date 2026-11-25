// The file browser: one folder of one bucket, a page at a time.

import { api, bucketPath, loadPref, objectPath, presign, savePref, showError } from "./api.js";
import {
  ask, baseName, confirmDialog, copyText, dateFmt, dateFmtLong, formatInt, formatSize, h, icon, iconButton, toast,
} from "./dom.js";
import { navigate } from "./router.js";
import { PAGE_SIZES, can, rerenderMain, state, ui } from "./state.js";
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

  main.append(
    h("div", { class: "toolbar" },
      renderCrumbs(),
      h("div", { class: "toolbar-actions" },
        canWrite && h("button", { class: "with-icon", onclick: newFolder }, icon("folderPlus"), "New folder"),
        canWrite && h("button", { class: "with-icon", onclick: () => pickAndUpload({ folder: true }) }, icon("folder"), "Upload folder"),
        canWrite && h("button", { class: "primary with-icon", onclick: () => pickAndUpload({ folder: false }) }, icon("upload"), "Upload"),
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
    const a = h("a", { href: url, download: baseName(o.key), rel: "noreferrer" });
    document.body.append(a);
    a.click();
    a.remove();
  } catch (err) { showError(err); }
}

async function share(o) {
  const dialog = document.getElementById("share");
  const ttl = document.getElementById("share-ttl");
  const out = document.getElementById("share-url");
  const make = async () => {
    out.value = "Creating link…";
    try {
      out.value = (await presign(state.route.bucket, o.key, "GET", Number(ttl.value))).url;
    } catch (err) { out.value = ""; showError(err); }
  };
  ttl.onchange = make;
  document.getElementById("share-copy").onclick = () => copyText(out.value, "Link copied", out);
  dialog.showModal();
  await make();
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
  const ok = await confirmDialog({
    title: "Delete this file?",
    body: [h("p", { class: "mono confirm-target" }, o.key), "This can't be undone."],
    confirmLabel: "Delete",
  });
  if (!ok) return;
  try {
    await api("DELETE", objectPath(state.route.bucket, o.key));
    toast("Deleted");
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
    body: ["Only an empty bucket can be deleted. This can't be undone."],
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
