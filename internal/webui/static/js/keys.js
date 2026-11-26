// API keys page. Only "*:admin" keys see it; the server enforces the same.

import { api, showError } from "./api.js";
import { confirmDialog, copyText, dateFmt, emptyState, formDialog, h, icon, iconButton, toast } from "./dom.js";
import { rerenderMain, state } from "./state.js";

const PERMS = [
  ["rw", "Read & write"],
  ["r", "Read only"],
  ["w", "Write only"],
  ["admin", "Admin"],
];
const PERM_LABEL = Object.fromEntries(PERMS);

export function describeScope(scope) {
  const i = scope.lastIndexOf(":");
  const bucket = scope.slice(0, i);
  const perm = scope.slice(i + 1);
  return `${bucket === "*" ? "All buckets" : bucket} · ${PERM_LABEL[perm] || perm}`;
}

export async function loadKeys() {
  try {
    state.apiKeys = (await api("GET", "/v1/keys")).keys;
  } catch (err) { showError(err); }
  rerenderMain();
}

export function renderKeysPage() {
  return h("main", { id: "main", tabindex: "-1", class: "main keys-page", "aria-label": "API keys" },
    h("header", { class: "page-head" },
      h("span", { class: "bucket-icon", "aria-hidden": "true" }, icon("key", 20)),
      h("div", { class: "page-head-text" },
        h("h1", {}, "API keys ", h("span", { class: "count" }, String(state.apiKeys.length))),
        h("p", { class: "muted" },
          "Give each app its own key, limited to the buckets it needs. A key for one bucket gets “access denied” everywhere else.")),
      h("button", { class: "primary with-icon", onclick: openCreateKey }, icon("plus", 16), "Create key")),
    renderKeyTable());
}

// scopeEditor is the "Access" rows (bucket + permission, any number) used
// by the key and user dialogs. read() returns the scopes, de-duplicated.
export function scopeEditor(initial = []) {
  const rows = h("div", { class: "scope-rows" });
  const hint = h("p", { class: "hint", role: "status" });
  const bucketOptions = () => [
    ...state.buckets.map((b) => h("option", { value: b.name }, b.name)),
    h("option", { value: "*" }, "All buckets"),
  ];
  const updateHint = () => {
    const all = [...rows.querySelectorAll(".scope-row")].some((r) =>
      r.querySelector(".scope-bucket").value === "*" && r.querySelector(".scope-perm").value === "admin");
    hint.textContent = all
      ? "All buckets + Admin is a full server admin: it can create buckets and manage keys and users."
      : "";
  };
  const addRow = (bucket, perm = "rw") => {
    const b = h("select", { class: "scope-bucket", "aria-label": "Bucket", onchange: updateHint }, bucketOptions());
    if (bucket && ![...b.options].some((o) => o.value === bucket)) b.append(h("option", { value: bucket }, bucket));
    if (bucket) b.value = bucket;
    const p = h("select", { class: "scope-perm", "aria-label": "Permission", onchange: updateHint },
      PERMS.map(([v, label]) => h("option", { value: v }, label)));
    p.value = perm;
    const row = h("div", { class: "scope-row" }, b, p,
      iconButton("close", "Remove this access", () => {
        if (rows.children.length > 1) { row.remove(); updateHint(); }
      }));
    rows.append(row);
    updateHint();
  };
  if (initial.length) {
    for (const sc of initial) {
      const i = sc.lastIndexOf(":");
      addRow(sc.slice(0, i), sc.slice(i + 1));
    }
  } else {
    addRow(state.buckets[0]?.name || "*");
  }
  const el = h("div", { class: "field" },
    h("span", {}, "Access"),
    rows,
    h("button", { type: "button", class: "link with-icon add-scope", onclick: () => addRow("*", "r") }, icon("plus", 16), "Add another bucket"),
    hint);
  const read = () => [...new Set([...rows.querySelectorAll(".scope-row")].map((r) =>
    `${r.querySelector(".scope-bucket").value}:${r.querySelector(".scope-perm").value}`))];
  return { el, read };
}

// openCreateKey asks for a name and access, then shows the new key once.
function openCreateKey() {
  const name = h("input", { required: true, maxlength: 64, placeholder: "e.g. billing app (production)", autocomplete: "off" });
  const access = scopeEditor();
  formDialog({
    title: "Create API key",
    text: "Give each app its own key, limited to the buckets it needs.",
    body: [h("label", { class: "field" }, "Name", name), access.el],
    submitLabel: "Create key",
    focus: name,
    onSubmit: async () => {
      const created = await api("POST", "/v1/keys", { body: { name: name.value.trim(), scopes: access.read() } });
      setTimeout(() => showNewKey(created)); // after this dialog has closed
      loadKeys();
    },
  });
}

function renderKeyTable() {
  if (state.apiKeys.length === 0) {
    return h("div", { class: "table-wrap" }, emptyState({
      icon: "key", title: "No keys",
      action: h("button", { class: "primary with-icon", onclick: openCreateKey }, icon("plus", 16), "Create key"),
    }));
  }
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Name"),
      h("th", { scope: "col" }, "Access"),
      h("th", { scope: "col", class: "when" }, "Created"),
      h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
    h("tbody", {}, state.apiKeys.map((k) => h("tr", {},
      h("td", { class: "name" }, k.name,
        k.id === state.me.keyId && h("span", { class: "chip you" }, "you"),
        h("span", { class: "key-id mono", title: "Key ID" }, k.id)),
      h("td", {}, h("div", { class: "chips" }, k.scopes.map((sc) =>
        h("span", { class: sc.startsWith("*:admin") ? "chip strong" : "chip" }, describeScope(sc))))),
      h("td", { class: "when" }, dateFmt.format(new Date(k.created))),
      h("td", { class: "actions" }, iconButton("trash", `Revoke ${k.name}`, () => revokeKey(k), "danger")))))));
}

async function revokeKey(k) {
  const self = k.id === state.me.keyId;
  const ok = await confirmDialog({
    title: `Revoke “${k.name}”?`,
    body: [
      h("div", { class: "chips" }, k.scopes.map((sc) => h("span", { class: "chip" }, describeScope(sc)))),
      "Apps using this key lose access immediately, and share and upload links made with it stop working. Files are not affected.",
      self && h("p", { class: "warn" }, "This is the key you're signed in with. You'll be signed out."),
    ],
    confirmLabel: "Revoke key",
  });
  if (!ok) return;
  try {
    await api("DELETE", `/v1/keys/${encodeURIComponent(k.id)}`);
    toast(`Revoked ${k.name}`);
    if (!self) await loadKeys(); // when self, the next request signs us out
    else await api("GET", "/v1/whoami").catch(() => {});
  } catch (err) {
    if (err.code === "LastAdminKey") {
      toast("This is the last admin key. Create another admin key first, then revoke this one.", { error: true });
      return;
    }
    if (err.status !== 401) showError(err);
  }
}

function showNewKey(created) {
  const dialog = document.getElementById("new-key");
  const out = document.getElementById("new-key-value");
  out.value = created.key;
  document.getElementById("new-key-name").textContent = created.name;
  document.getElementById("new-key-scopes").replaceChildren(
    ...created.scopes.map((sc) => h("span", { class: "chip" }, describeScope(sc))));
  document.getElementById("new-key-copy").onclick = () => copyText(out.value, "Key copied", out);
  // The token must not linger in the page once the dialog is closed.
  dialog.addEventListener("close", () => { out.value = ""; }, { once: true });
  dialog.showModal();
  out.select();
}
