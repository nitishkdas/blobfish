// BlobFish web UI entry point: sign-in, the app shell and routing. Talks
// only to the /v1 API, with a session token (user sign-in) or an API key.

import { api, forgetKey, loadKey, saveKey, session, showError } from "./api.js";
import { ask, emptyState, formatSize, h, icon, iconButton, rememberFocus, toast, wireDialogBackdrops } from "./dom.js";
import { reload, renderFiles, showRoute } from "./files.js";
import { describeScope, loadKeys, renderKeysPage } from "./keys.js";
import { changeOwnPassword, loadUsers, renderUsersPage } from "./users.js";
import { renderTrash, showTrash } from "./trash.js";
import { renderSettings, showSettings } from "./settings.js";
import { navigate, parseHash } from "./router.js";
import { can, isServerAdmin, state, ui } from "./state.js";
import { renderUploadPanel, stopAllUploads } from "./uploads.js";
import { applyTheme, themeToggle } from "./theme.js";

applyTheme();

function root() { return document.getElementById("app"); }

// ---------- Sign in ----------

// renderLogin shows the sign-in card: user name or email + password, or (for admins
// before any user exists, and for scripts' keys) an API key.
function renderLogin(message = "", mode = "user") {
  const remember = h("input", { type: "checkbox" });
  const error = h("p", { class: "error", role: "alert" }, message);
  const submit = h("button", { class: "primary block", type: "submit" }, "Sign in");
  let fields, first, attempt;

  if (mode === "user") {
    const name = h("input", { autocomplete: "username", spellcheck: "false", autocapitalize: "none", required: true });
    const pw = h("input", { type: "password", autocomplete: "current-password", required: true });
    fields = [h("label", { class: "field" }, "Username or email", name), h("label", { class: "field" }, "Password", pw)];
    first = name;
    attempt = async () => {
      let res;
      try {
        res = await api("POST", "/v1/login", { key: "", body: { username: name.value.trim(), password: pw.value } });
      } catch (err) {
        if (err.code === "InvalidCredentials") throw new Error("The username, email or password is incorrect.");
        if (err.code === "TooManyAttempts") throw new Error("Too many failed sign-in attempts. Wait a few minutes and try again.");
        throw err;
      }
      await enterApp(res.token);
      saveKey(res.token, remember.checked);
    };
  } else {
    const key = h("input", {
      type: "password", autocomplete: "off", spellcheck: "false", placeholder: "bf_…", class: "mono", required: true,
    });
    fields = [h("label", { class: "field" }, "API key", key)];
    first = key;
    attempt = async () => {
      const k = key.value.trim();
      try {
        await enterApp(k);
      } catch (err) {
        if (err.status === 401) throw new Error("This API key isn't valid or has been revoked.");
        throw err;
      }
      saveKey(k, remember.checked);
    };
  }

  const form = h("form", {
    onsubmit: async (ev) => {
      ev.preventDefault();
      submit.disabled = true;
      error.textContent = "";
      try {
        await attempt();
      } catch (err) {
        error.textContent = err.message;
      } finally {
        submit.disabled = false;
      }
    },
  }, fields, h("label", { class: "check" }, remember, "Keep me signed in on this device"), error, submit);

  const other = mode === "user"
    ? h("button", { type: "button", class: "link login-switch", onclick: () => renderLogin("", "key") }, "Sign in with an API key instead")
    : h("button", { type: "button", class: "link login-switch", onclick: () => renderLogin("", "user") }, "Sign in with a username instead");

  root().className = "";
  root().replaceChildren(h("main", { class: "login" },
    themeToggle(),
    h("div", { class: "login-card" },
      h("img", { src: "favicon.svg", alt: "" }),
      h("h1", {}, "Sign in to BlobFish"),
      h("p", { class: "muted" }, mode === "user"
        ? "Use the username or email and password your administrator gave you."
        : "Paste an API key. It stays in this browser and is sent only to this server."),
      form,
      other),
    mode === "key" && h("p", { class: "login-foot" }, "First time? The first admin key is in ", h("code", {}, "<data_dir>/initial-admin-key"), ".")));
  first.focus();
}

// enterApp checks the key, learns what it may do, and shows the app.
async function enterApp(key) {
  const [me, buckets] = await Promise.all([
    api("GET", "/v1/whoami", { key }),
    api("GET", "/v1/buckets", { key }),
  ]);
  session.key = key;
  state.me = {
    keyId: me.keyId, name: me.name, user: me.user || "", scopes: me.scopes,
    maxObjectSize: me.maxObjectSize, trashDays: me.trashDays || 0, minQuotaBytes: me.minQuotaBytes || 0,
  };
  state.buckets = buckets.buckets;
  route();
}

function signOut(message = "") {
  stopAllUploads();
  // End the session on the server too (best effort; a key has none).
  if (session.key?.startsWith("bfs_")) api("POST", "/v1/logout").catch(() => {});
  forgetKey();
  session.key = null;
  Object.assign(state, { me: { name: "", user: "", scopes: [], maxObjectSize: 0, trashDays: 0 }, buckets: [], apiKeys: [], users: [] });
  Object.assign(state.listing, { bucket: null, prefix: "", cursors: [""], page: 1, folders: [], files: [], nextCursor: "", loaded: false });
  history.replaceState(null, "", "#/");
  renderLogin(message);
}

session.onUnauthorized = () => signOut(state.me.user
  ? "Your session has ended. Sign in again."
  : "Your API key is no longer valid. Sign in again.");

// ---------- Routing ----------

function route() {
  if (!session.key) return;
  const r = parseHash();
  if ((r.view === "keys" || r.view === "users") && !isServerAdmin()) {
    navigate({ view: "home" }, { replace: true });
    return;
  }
  if ((r.view === "trash" || r.view === "settings") && !can("w", r.bucket)) {
    navigate({ view: "files", bucket: r.bucket, prefix: "", page: 1 }, { replace: true });
    return;
  }
  if (["files", "trash", "settings"].includes(r.view) && !state.buckets.some((b) => b.name === r.bucket)) {
    if (r.bucket) toast(`Bucket “${r.bucket}” doesn't exist or you don't have access to it.`, { error: true });
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
  if (r.view === "users") loadUsers();
  if (r.view === "trash") showTrash(r);
  if (r.view === "settings") showSettings(r);
  renderApp();
  // A new page: say where we are (tab title, screen readers) and start
  // keyboard focus at its content rather than back at the top of the page.
  document.title = pageTitle(r);
  document.getElementById("main")?.focus({ preventScroll: true });
}

function pageTitle(r) {
  const where = {
    files: () => `${r.bucket}${r.prefix ? "/" + r.prefix.slice(0, -1) : ""}`,
    trash: () => `Trash · ${r.bucket}`,
    settings: () => `Settings · ${r.bucket}`,
    keys: () => "API keys",
    users: () => "Users",
  }[r.view];
  return where ? `${where()} · BlobFish` : "BlobFish";
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
    ? h("p", { class: "side-empty" }, isServerAdmin() ? "No buckets yet." : "You don't have access to any buckets.")
    : h("ul", { class: "side-list" }, state.buckets.map((b) =>
      h("li", {}, h("a", {
        href: `#/b/${encodeURIComponent(b.name)}/`,
        "aria-current": inBucket && b.name === r.bucket ? "true" : "false",
        title: b.name,
      },
      icon("database", 16),
      h("span", { class: "bucket-name" }, b.name),
      b.public && h("span", { class: "public-mark", title: "Public: anyone with a public link can download files" }, icon("globe", 13)),
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
        h("ul", { class: "side-list" },
          h("li", {}, h("a", {
            href: "#/users", "aria-current": r.view === "users" ? "true" : "false",
          }, icon("users", 16), h("span", { class: "bucket-name" }, "Users"))),
          h("li", {}, h("a", {
            href: "#/keys", "aria-current": r.view === "keys" ? "true" : "false",
          }, icon("key", 16), h("span", { class: "bucket-name" }, "API keys")))))),
    renderProfile(),
  ];
}

// newBucket asks for a name and creates the bucket (server admins only).
async function newBucket() {
  const res = await ask({
    title: "New bucket",
    body: [h("p", { class: "muted" }, "3–63 lowercase letters, numbers, dots or dashes. The name can't be changed later.")],
    input: {
      label: "Bucket name",
      placeholder: "invoices",
      validate: (v) => (/^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$/.test(v) ? "" : "Use 3–63 lowercase letters, numbers, dots or dashes, starting and ending with a letter or number."),
    },
    choices: [{ label: "Cancel", value: "" }, { label: "Create bucket", value: "ok", kind: "primary" }],
  });
  if (!res) return;
  try {
    await api("PUT", `/v1/buckets/${encodeURIComponent(res.value)}`);
    toast(`Bucket “${res.value}” created`);
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
  const user = state.me.user;
  return h("div", { class: "profile", "aria-label": user ? "Signed-in user" : "Signed-in key" },
    h("span", { class: "avatar", "aria-hidden": "true" }, (name.trim()[0] || "?").toUpperCase()),
    h("div", { class: "profile-text", title: scopes.map(describeScope).join("\n") },
      h("span", { class: "profile-name" }, name),
      h("span", { class: "profile-access" }, user ? access : `API key · ${access}`)),
    user && iconButton("lock", "Change password", changeOwnPassword),
    iconButton("signOut", "Sign out", () => signOut()));
}

function renderMain() {
  const r = state.route;
  if (r.view === "keys") return renderKeysPage();
  if (r.view === "users") return renderUsersPage();
  if (r.view === "files" && can("r", r.bucket)) return renderFiles();
  if (r.view === "trash" && can("w", r.bucket)) return renderTrash();
  if (r.view === "settings" && can("w", r.bucket)) return renderSettings();
  return h("main", { id: "main", tabindex: "-1", class: "main", "aria-label": "Files" },
    emptyState({
      icon: "database",
      title: isServerAdmin() ? "No buckets yet" : "No buckets available",
      text: isServerAdmin() ? "A bucket holds files, with its own keys, quota and settings." : "Ask an administrator for access to a bucket.",
      action: isServerAdmin() && h("button", { class: "primary with-icon", onclick: newBucket }, icon("plus"), "New bucket"),
    }));
}

ui.renderApp = renderApp;
ui.renderMain = () => {
  const old = document.querySelector(".main");
  if (old) {
    const focus = rememberFocus(old);
    const fresh = renderMain();
    old.replaceWith(fresh);
    focus.restore(fresh);
  }
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
    renderLogin(err.status === 401 ? "Your saved sign-in is no longer valid. Sign in again." : err.message);
  }
}

start();
