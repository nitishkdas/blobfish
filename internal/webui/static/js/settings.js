// A bucket's Settings page: one card per setting, each shown only to keys
// allowed to change it (the server checks every request anyway).

import { api, showError } from "./api.js";
import { ask, confirmDialog, formatInt, formatSize, h, relativeTime, toast } from "./dom.js";
import { meterBar, renderBucketHeader, visibilityBadge } from "./bucket.js";
import { navigate } from "./router.js";
import { bucketInfo, can, isServerAdmin, rerenderMain, state, ui } from "./state.js";
import { openUploadLinks } from "./uplinks.js";
import { openWebhooks } from "./webhooks.js";

// showSettings loads what the cards list (upload links, webhooks).
export async function showSettings(route) {
  const S = state.settings;
  const bucket = route.bucket;
  Object.assign(S, { bucket, uploadLinks: null, webhooks: null });
  try { state.buckets = (await api("GET", "/v1/buckets")).buckets; } catch { /* keep the old numbers */ }
  const jobs = [];
  if (can("w", bucket)) {
    jobs.push(api("GET", `/v1/uploadlinks?bucket=${encodeURIComponent(bucket)}`)
      .then((r) => { S.uploadLinks = r.uploadLinks; }).catch((err) => { showError(err); S.uploadLinks = []; }));
  }
  if (can("admin", bucket)) {
    jobs.push(api("GET", `/v1/webhooks?bucket=${encodeURIComponent(bucket)}`)
      .then((r) => { S.webhooks = r.webhooks; }).catch((err) => { showError(err); S.webhooks = []; }));
  }
  await Promise.all(jobs);
  if (S.bucket === bucket && state.route.view === "settings") rerenderMain();
}

// whenClosed refreshes the page once a dialog opened from it closes.
function whenClosed(id) {
  document.getElementById(id).addEventListener("close", () => {
    if (state.route.view === "settings") showSettings(state.route);
  }, { once: true });
}

// row is one setting: what it is (left), its current state (middle) and
// the button that changes it (right).
function row({ title, description, status, actions = [] }) {
  return h("section", { class: "setting-row" },
    h("div", { class: "setting-label" },
      h("h3", {}, title),
      description && h("p", {}, description)),
    h("div", { class: "setting-value" }, status),
    h("div", { class: "setting-actions" }, actions.filter(Boolean)));
}

function group(title, rows, { danger = false } = {}) {
  rows = rows.filter(Boolean);
  if (rows.length === 0) return null;
  return h("div", { class: "settings-group" },
    h("h2", { class: "group-title" }, title),
    h("div", { class: danger ? "panel danger-panel" : "panel" }, rows));
}

export function renderSettings() {
  const bucket = state.route.bucket;
  const b = bucketInfo(bucket) || {};
  const S = state.settings;
  const admin = can("admin", bucket);
  const loading = h("span", { class: "muted" }, "Loading…");

  const groups = [
    group("Storage and access", [
      storageRow(b),
      admin && row({
        title: "Visibility",
        description: "Public buckets let anyone download a file if they have its address. Listing, uploading and deleting always need a key.",
        status: h("div", { class: "value-line" }, visibilityBadge(b),
          h("span", { class: "muted" }, b.public ? "Anyone with a file's address can open it" : "Every request needs a key or a link")),
        actions: [h("button", { onclick: () => setVisibility(bucket, !b.public) }, b.public ? "Make private" : "Make public")],
      }),
      admin && row({
        title: "Browser access",
        description: "Websites whose pages may upload to and download from this bucket directly, for example with presigned URLs.",
        status: (b.corsOrigins || []).length === 0
          ? h("span", { class: "muted" }, "No websites")
          : h("ul", { class: "value-list" }, b.corsOrigins.map((o) => h("li", { class: "mono" }, o === "*" ? "* (any website)" : o))),
        actions: [h("button", { onclick: () => { editBrowserAccess(bucket); whenClosed("cors"); } }, "Edit")],
      }),
    ]),
    group("Integrations", [
      can("w", bucket) && row({
        title: "Upload links",
        description: "Let people without a key send files into a folder. They can't see, replace or delete anything.",
        status: S.uploadLinks === null ? loading : uploadLinksSummary(S.uploadLinks),
        actions: [h("button", { onclick: () => { openUploadLinks(bucket, ""); whenClosed("uplinks"); } }, "Manage")],
      }),
      admin && row({
        title: "Webhooks",
        description: "Signed notifications to your apps when files are added, deleted or restored.",
        status: S.webhooks === null ? loading : webhooksSummary(S.webhooks),
        actions: [h("button", { onclick: () => { openWebhooks(bucket); whenClosed("webhooks"); } }, "Manage")],
      }),
    ]),
    admin && group("Danger zone", [row({
      title: "Delete bucket",
      description: "Only an empty bucket can be deleted: delete or move its files, then empty its trash. This can't be undone.",
      status: null,
      actions: [h("button", { class: "danger-solid", onclick: () => deleteBucket(bucket) }, "Delete bucket")],
    })], { danger: true }),
  ];

  return h("section", { class: "main", "aria-label": "Bucket settings" },
    renderBucketHeader(bucket, "settings"),
    h("div", { class: "settings" }, groups.filter(Boolean)));
}

function storageRow(b) {
  const pct = b.quotaBytes ? Math.min(100, (b.usedBytes / b.quotaBytes) * 100) : 0;
  return row({
    title: "Storage",
    description: "Space taken by this bucket's files. Files in trash don't count.",
    status: h("div", { class: "usage" },
      h("div", { class: "usage-numbers" },
        h("strong", {}, formatSize(b.usedBytes || 0)),
        h("span", { class: "muted" }, b.quotaBytes ? ` of ${formatSize(b.quotaBytes)}` : " used · no quota"),
        h("span", { class: "muted" }, ` · ${formatInt(b.objects || 0)} file${b.objects === 1 ? "" : "s"}`)),
      b.quotaBytes > 0 && meterBar(pct, `${Math.round(pct)}% of the quota used`)),
    actions: [isServerAdmin() && h("button", { onclick: () => editQuota(b.name) }, b.quotaBytes ? "Change quota" : "Set quota")],
  });
}

function uploadLinksSummary(links) {
  const now = new Date();
  const live = links.filter((l) => (!l.expires || new Date(l.expires) > now) && !(l.maxFiles && l.uploads >= l.maxFiles));
  if (links.length === 0) return h("span", { class: "muted" }, "No upload links");
  return h("ul", { class: "value-list" }, live.slice(0, 5).map((l) => h("li", {},
    h("span", { class: "dot ok", "aria-hidden": "true" }),
    h("span", { class: "value-main" }, l.label || (l.prefix ? l.prefix : "Top level")),
    h("span", { class: "muted" }, `${l.uploads}${l.maxFiles ? ` of ${l.maxFiles}` : ""} file${l.uploads === 1 && !l.maxFiles ? "" : "s"}`,
      l.expires ? ` · expires ${relativeTime(new Date(l.expires))}` : " · never expires"))),
  live.length > 5 && h("li", { class: "muted" }, `and ${live.length - 5} more`),
  live.length < links.length && h("li", { class: "muted" },
    h("span", { class: "dot", "aria-hidden": "true" }), `${links.length - live.length} expired or full`));
}

function webhooksSummary(hooks) {
  if (hooks.length === 0) return h("span", { class: "muted" }, "No webhooks");
  return h("ul", { class: "value-list" }, hooks.map((w) => {
    const failing = w.lastFailure && (!w.lastSuccess || new Date(w.lastFailure) > new Date(w.lastSuccess));
    return h("li", {},
      h("span", { class: failing ? "dot bad" : w.lastSuccess ? "dot ok" : "dot", "aria-hidden": "true" }),
      h("span", { class: "mono value-main" }, w.url),
      failing
        ? h("span", { class: "card-bad" }, `failing${w.pending ? ` · ${w.pending} waiting` : ""}`)
        : h("span", { class: "muted" }, w.lastSuccess ? `last delivered ${relativeTime(new Date(w.lastSuccess))}` : "nothing sent yet"),
      failing && h("span", { class: "value-error", title: w.lastError }, w.lastError));
  }));
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
    ui.renderApp();
  } catch (err) { showError(err); }
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

async function setVisibility(bucket, makePublic) {
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

async function deleteBucket(bucket) {
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
