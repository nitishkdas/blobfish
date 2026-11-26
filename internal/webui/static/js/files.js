// The file browser: one folder of one bucket, a page at a time.

import { api, bucketPath, loadPref, objectPath, presign, savePref, showError } from "./api.js";
import {
  ask, baseName, confirmDialog, emptyState, copyText, dateFmt, dateFmtLong, formatInt, formatSize, h, icon, iconButton,
  relativeTime, toast,
} from "./dom.js";
import { navigate } from "./router.js";
import { PAGE_SIZES, bucketInfo, can, rerenderMain, state } from "./state.js";
import { pickAndUpload, wireDrop } from "./uploads.js";
import { openUploadLinks } from "./uplinks.js";
import { renderBucketHeader } from "./bucket.js";

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

  main.append(
    renderBucketHeader(r.bucket, "files"),
    h("div", { class: "toolbar" },
      renderCrumbs(),
      h("div", { class: "toolbar-actions" },
        canWrite && h("button", {
          class: "with-icon ghost", onclick: () => openUploadLinks(r.bucket, r.prefix),
          title: "Let people without a key send files into this folder",
        }, icon("inbox", 16), "Upload links"),
        canWrite && h("span", { class: "toolbar-sep", "aria-hidden": "true" }),
        canWrite && h("button", { class: "with-icon", onclick: newFolder }, icon("folderPlus", 16), "New folder"),
        canWrite && h("button", { class: "with-icon", onclick: () => pickAndUpload({ folder: true }) }, icon("folder", 16), "Upload folder"),
        canWrite && h("button", { class: "primary with-icon", onclick: () => pickAndUpload({ folder: false }) }, icon("upload", 16), "Upload"))),
    L.loaded ? renderTable(canWrite) : h("div", { class: "table-wrap" }, h("p", { class: "loading" }, "Loading…")));
  return main;
}

function renderCrumbs() {
  const r = state.route;
  const parts = r.prefix.split("/").filter(Boolean);
  const crumbs = [h("button", { class: "with-icon", onclick: () => go({ prefix: "", page: 1 }), "aria-current": parts.length ? null : "page" },
    icon("database", 15), r.bucket)];
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
    const upload = canWrite && h("button", { class: "primary with-icon", onclick: () => pickAndUpload({ folder: false }) }, icon("upload", 16), "Upload files");
    return h("div", { class: "table-wrap" }, r.prefix
      ? emptyState({
        icon: "folder", title: "This folder is empty",
        text: canWrite ? "Folders exist only while they hold files: upload or drop something here and it will stay." : "",
        action: upload,
      })
      : emptyState({
        icon: "upload", title: "No files yet",
        text: canWrite ? "Upload files or drag them onto this page." : "Files added to this bucket will show up here.",
        action: upload,
      }));
  }
  const rows = [
    ...L.folders.map((p) => h("tr", {},
      h("td", { class: "name" }, h("button", { class: "with-icon", onclick: () => go({ prefix: p, page: 1 }) },
        h("span", { class: "file-icon folder" }, icon("folder", 16)), p.slice(r.prefix.length, -1) || p.slice(r.prefix.length))),
      h("td", { class: "num" }, "—"),
      h("td", { class: "when" }, ""),
      h("td", { class: "actions" }))),
    ...L.files.map((o) => {
      const name = o.key.slice(r.prefix.length);
      return h("tr", {},
        h("td", { class: "name", title: o.key },
          h("button", { class: "with-icon file-link", onclick: () => openFile(o), title: `Open ${name} in a new tab` },
            h("span", { class: "file-icon" }, icon(fileIconName(o), 16)), name)),
        h("td", { class: "num" }, formatSize(o.size)),
        h("td", { class: "when" }, dateFmt.format(new Date(o.modified))),
        h("td", { class: "actions" },
          iconButton("info", `Details for ${name}`, () => showDetails(o)),
          iconButton("download", `Download ${name}`, () => download(o)),
          iconButton("share", `Share ${name}`, () => share(o)),
          canWrite && iconButton("trash", `Delete ${name}`, () => deleteObject(o), "danger")));
    }),
  ];
  return h("div", { class: "table-wrap" }, h("table", { class: "files-table" },
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Name"),
      h("th", { scope: "col", class: "num" }, "Size"),
      h("th", { scope: "col", class: "when" }, "Modified"),
      h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
    h("tbody", {}, rows)),
  renderPager());
}

// fileIconName picks an icon from the content type or the extension.
function fileIconName(o) {
  const t = (o.contentType || "").toLowerCase();
  const ext = o.key.slice(o.key.lastIndexOf(".") + 1).toLowerCase();
  if (t.startsWith("image/")) return "image";
  if (t.startsWith("video/")) return "film";
  if (/zip|tar|gzip|compressed|x-7z|x-rar/.test(t) || ["zip", "gz", "tgz", "tar", "7z", "rar", "zst", "xz"].includes(ext)) return "archive";
  if (t.startsWith("text/") || t === "application/pdf" || t.includes("json") || t.includes("xml") ||
    ["pdf", "txt", "md", "csv", "json", "doc", "docx", "odt", "rtf"].includes(ext)) return "fileText";
  return "file";
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
    h("span", { class: "muted" }, hasPrev || hasNext
      ? `Items ${formatInt(first)}–${formatInt(first + count - 1)}`
      : `${formatInt(count)} item${count === 1 ? "" : "s"}`),
    h("span", { class: "spacer" }),
    h("label", { class: "pager-size muted" }, "Per page", sizeSelect),
    (hasPrev || hasNext) && h("div", { class: "pager-nav" },
      h("button", { class: "with-icon small", disabled: !hasPrev, onclick: () => go({ page: L.page - 1 }) }, icon("left", 16), "Prev"),
      h("span", { class: "pager-page", "aria-current": "page" }, `Page ${L.page}`),
      h("button", { class: "with-icon small", disabled: !hasNext, onclick: () => go({ page: L.page + 1 }) }, "Next", icon("right", 16))));
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

