// BlobFish web UI entry point: sign-in, the app shell and routing. Talks
// only to the /v1 API with the API key the user pastes in.

import { api, forgetKey, loadKey, saveKey, session, showError } from "./api.js";
import { ask, emptyState, formatSize, h, icon, iconButton, toast, wireDialogBackdrops } from "./dom.js";
import { reload, renderFiles, showRoute } from "./files.js";
import { describeScope, loadKeys, renderKeysPage } from "./keys.js";
import { renderTrash, showTrash } from "./trash.js";
import { renderSettings, showSettings } from "./settings.js";
import { navigate, parseHash } from "./router.js";
import { can, isServerAdmin, state, ui } from "./state.js";
import { renderUploadPanel, stopAllUploads } from "./uploads.js";
import { applyTheme, themeToggle } from "./theme.js";

applyTheme();

function root() { return document.getElementById("app"); }

// ---------- Sign in ----------

function renderLogin(message = "") {
  const input = h("input", {
    type: "password", autocomplete: "off", spellcheck: "false", placeholder: "bf_…",
    "aria-label": "API key", class: "mono", required: true,
  });
  const remember = h("input", { type: "checkbox" });
  const error = h("p", { class: "error", role: "alert" }, message);
  const submit = h("button", { class: "primary block", type: "submit" }, "Sign in");

  const form = h("form", {
    onsubmit: async (ev) => {
      ev.preventDefault();
      const key = input.value.trim();
      submit.disabled = true;
      error.textContent = "";
      try {
        await enterApp(key);
        saveKey(key, remember.checked);
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

  root().className = "";
  root().replaceChildren(h("main", { class: "login" },
    themeToggle(),
    h("div", { class: "login-card" },
      h("img", { src: "favicon.svg", alt: "" }),
      h("h1", {}, "Sign in to BlobFish"),
      h("p", { class: "muted" }, "Use an API key. It stays in this browser and is sent only to this server."),
      form),
    h("p", { class: "login-foot" }, "No key? A server admin can create one, or find the first one in ", h("code", {}, "<data_dir>/initial-admin-key"), ".")));
  input.focus();
}

// enterApp checks the key, learns what it may do, and shows the app.
async function enterApp(key) {
  const [me, buckets] = await Promise.all([
    api("GET", "/v1/whoami", { key }),
    api("GET", "/v1/buckets", { key }),
  ]);
  session.key = key;
  state.me = { keyId: me.keyId, name: me.name, scopes: me.scopes, maxObjectSize: me.maxObjectSize, trashDays: me.trashDays || 0 };
  state.buckets = buckets.buckets;
  route();
}

function signOut(message = "") {
  stopAllUploads();
  forgetKey();
  session.key = null;
  Object.assign(state, { me: { name: "", scopes: [], maxObjectSize: 0, trashDays: 0 }, buckets: [], apiKeys: [] });
  Object.assign(state.listing, { bucket: null, prefix: "", cursors: [""], page: 1, folders: [], files: [], nextCursor: "", loaded: false });
  history.replaceState(null, "", "#/");
  renderLogin(message);
}

session.onUnauthorized = () => signOut("Your API key was rejected or revoked. Sign in again.");

// ---------- Routing ----------

function route() {
  if (!session.key) return;
  const r = parseHash();
  if (r.view === "keys" && !isServerAdmin()) {
    navigate({ view: "home" }, { replace: true });
    return;
  }
  if ((r.view === "trash" || r.view === "settings") && !can("w", r.bucket)) {
    navigate({ view: "files", bucket: r.bucket, prefix: "", page: 1 }, { replace: true });
    return;
  }
  if (["files", "trash", "settings"].includes(r.view) && !state.buckets.some((b) => b.name === r.bucket)) {
    if (r.bucket) toast(`No bucket “${r.bucket}” for this key.`, { error: true });
    navigate({ view: "home" }, { replace: true });
    return;
  }
  if (r.view === "home" && state.buckets.length) {
    // Land in the first bucket rather than on an empty page.
    navigate({ view: "files", bucket: state.buckets[0].name, prefix: "", page: 1 }, { replace: true });
    return;
  }
  state.route = r;
  if (r.view === "files") showRoute(r); // resets the listing synchronously, then loads
  if (r.view === "keys") loadKeys();
  if (r.view === "trash") showTrash(r);
  if (r.view === "settings") showSettings(r);
  renderApp();
}

window.addEventListener("hashchange", route);

// ---------- Shell ----------

function renderApp() {
  root().className = "";
  root().replaceChildren(h("div", { class: "shell" },
    h("nav", { class: "side", "aria-label": "Main" }, renderSidebar()),
    renderMain()));
}

function renderSidebar() {
  const r = state.route;
  const inBucket = ["files", "trash", "settings"].includes(r.view);
  const items = state.buckets.length === 0
    ? h("p", { class: "side-empty" }, isServerAdmin() ? "No buckets yet." : "This key has no buckets it can read.")
    : h("ul", { class: "side-list" }, state.buckets.map((b) =>
      h("li", {}, h("a", {
        href: `#/b/${encodeURIComponent(b.name)}/`,
        "aria-current": inBucket && b.name === r.bucket ? "true" : "false",
        title: b.name,
      },
      icon("database", 16),
      h("span", { class: "bucket-name" }, b.name),
      b.public && h("span", { class: "public-mark", title: "Public: files can be read without a key" }, icon("globe", 13)),
      h("span", { class: "side-meta" }, formatSize(b.usedBytes || 0))))));

  return [
    h("div", { class: "brand-row" },
      h("a", { class: "brand-link", href: "#/" },
        h("img", { src: "favicon.svg", alt: "" }),
        h("span", { class: "brand" }, "BlobFish")),
      themeToggle()),
    h("div", { class: "side-scroll" },
      h("div", { class: "side-section" },
        h("div", { class: "side-head" },
          h("h2", {}, "Buckets"),
          isServerAdmin() && iconButton("plus", "New bucket", newBucket, "small")),
        items),
      isServerAdmin() && h("div", { class: "side-section" },
        h("div", { class: "side-head" }, h("h2", {}, "Server")),
        h("ul", { class: "side-list" }, h("li", {}, h("a", {
          href: "#/keys", "aria-current": r.view === "keys" ? "true" : "false",
        }, icon("key", 16), h("span", { class: "bucket-name" }, "API keys")))))),
    renderProfile(),
  ];
}

// newBucket asks for a name and creates the bucket (server admins only).
async function newBucket() {
  const res = await ask({
    title: "New bucket",
    body: [h("p", { class: "muted" }, "Lowercase letters, numbers, dots and dashes; 3 to 63 characters. The name can't be changed later.")],
    input: {
      label: "Bucket name",
      placeholder: "e.g. invoices",
      validate: (v) => (/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(v) ? "" : "Use 3–63 lowercase letters, numbers, dots or dashes, starting and ending with a letter or number."),
    },
    choices: [{ label: "Cancel", value: "" }, { label: "Create bucket", value: "ok", kind: "primary" }],
  });
  if (!res) return;
  try {
    await api("PUT", `/v1/buckets/${encodeURIComponent(res.value)}`);
    toast(`Created bucket ${res.value}`);
    state.buckets = (await api("GET", "/v1/buckets")).buckets;
    navigate({ view: "files", bucket: res.value, prefix: "", page: 1 });
  } catch (err) { showError(err); }
}

// renderProfile is the signed-in key, pinned to the bottom of the sidebar.
function renderProfile() {
  const { name, scopes } = state.me;
  let access;
  if (isServerAdmin()) access = "Admin · all buckets";
  else if (scopes.length === 1) access = describeScope(scopes[0]);
  else access = `${scopes.length} bucket permissions`;
  return h("div", { class: "profile", "aria-label": "Signed-in key" },
    h("span", { class: "avatar", "aria-hidden": "true" }, (name.trim()[0] || "?").toUpperCase()),
    h("div", { class: "profile-text", title: scopes.map(describeScope).join("\n") },
      h("span", { class: "profile-name" }, name),
      h("span", { class: "profile-access" }, access)),
    iconButton("signOut", "Sign out", () => signOut()));
}

function renderMain() {
  const r = state.route;
  if (r.view === "keys") return renderKeysPage();
  if (r.view === "files" && can("r", r.bucket)) return renderFiles();
  if (r.view === "trash" && can("w", r.bucket)) return renderTrash();
  if (r.view === "settings" && can("w", r.bucket)) return renderSettings();
  return h("section", { class: "main", "aria-label": "Files" },
    emptyState({
      icon: "database",
      title: isServerAdmin() ? "No buckets yet" : "Nothing to show",
      text: isServerAdmin() ? "A bucket holds files, with its own keys, quota and settings." : "This key can't read any bucket.",
      action: isServerAdmin() && h("button", { class: "primary with-icon", onclick: newBucket }, icon("plus"), "New bucket"),
    }));
}

ui.renderApp = renderApp;
ui.renderMain = () => {
  const old = document.querySelector(".main");
  if (old) old.replaceWith(renderMain());
  // Keep the sidebar's sizes and current bucket in step.
  const side = document.querySelector(".side");
  if (side) {
    const scroll = side.querySelector(".side-scroll")?.scrollTop || 0;
    side.replaceChildren(...renderSidebar());
    side.querySelector(".side-scroll").scrollTop = scroll;
  }
};
ui.reloadFiles = reload;

// ---------- Start ----------

async function start() {
  wireDialogBackdrops();
  renderUploadPanel();
  const key = loadKey();
  if (!key) { renderLogin(); return; }
  try {
    await enterApp(key);
  } catch (err) {
    forgetKey();
    renderLogin(err.status === 401 ? "Your saved key no longer works. Sign in again." : err.message);
  }
}

start();
