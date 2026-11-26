// API keys page. Only "*:admin" keys see it; the server enforces the same.

import { api, showError } from "./api.js";
import { confirmDialog, copyText, dateFmt, emptyState, h, icon, iconButton, toast } from "./dom.js";
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
  return h("section", { class: "main keys-page", "aria-label": "API keys" },
    h("header", { class: "page-head" },
      h("span", { class: "bucket-icon", "aria-hidden": "true" }, icon("key", 20)),
      h("div", {},
        h("h1", {}, "API keys"),
        h("p", { class: "muted" },
          "Give each app its own key, limited to the buckets it needs. A key for one bucket gets “access denied” everywhere else."))),
    h("div", { class: "settings-group" },
      h("h2", { class: "group-title" }, "New key"),
      renderKeyForm()),
    h("div", { class: "settings-group" },
      h("h2", { class: "group-title" }, "Keys ", h("span", { class: "count" }, String(state.apiKeys.length))),
      renderKeyTable()));
}

function renderKeyForm() {
  const name = h("input", { required: true, maxlength: 64, placeholder: "e.g. app1 production", "aria-label": "Key name" });
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
      ? "All buckets + Admin is a full admin key: it can create buckets and manage keys. Apps almost never need this."
      : "";
  };
  const addRow = (bucket) => {
    const b = h("select", { class: "scope-bucket", "aria-label": "Bucket", onchange: updateHint }, bucketOptions());
    if (bucket) b.value = bucket;
    const p = h("select", { class: "scope-perm", "aria-label": "Permission", onchange: updateHint },
      PERMS.map(([v, label]) => h("option", { value: v }, label)));
    const row = h("div", { class: "scope-row" }, b, p,
      iconButton("close", "Remove this access", () => {
        if (rows.children.length > 1) { row.remove(); updateHint(); }
      }));
    rows.append(row);
    updateHint();
  };
  addRow(state.route.bucket || state.buckets[0]?.name || "*");

  const submit = h("button", { class: "primary", type: "submit" }, "Create key");
  return h("form", {
    class: "panel key-form",
    onsubmit: async (ev) => {
      ev.preventDefault();
      const scopes = [...new Set([...rows.querySelectorAll(".scope-row")].map((r) =>
        `${r.querySelector(".scope-bucket").value}:${r.querySelector(".scope-perm").value}`))];
      submit.disabled = true;
      try {
        const created = await api("POST", "/v1/keys", { body: { name: name.value.trim(), scopes } });
        showNewKey(created);
        await loadKeys();
      } catch (err) {
        showError(err);
      } finally {
        submit.disabled = false;
      }
    },
  },
  h("label", { class: "field" }, "Name", name),
  h("div", { class: "field" }, h("span", {}, "Access"), rows,
    h("button", { type: "button", class: "link with-icon add-scope", onclick: () => addRow("*") }, icon("plus", 16), "Add another bucket")),
  hint,
  h("div", { class: "row" }, submit));
}

function renderKeyTable() {
  if (state.apiKeys.length === 0) return h("div", { class: "table-wrap" }, emptyState({ icon: "key", title: "No keys" }));
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
      "Apps using this key lose access immediately.",
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
