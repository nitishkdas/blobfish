// BlobFish web UI entry point: sign-in, the app shell and routing. Talks
// only to the /v1 API with the API key the user pastes in.

import { api, forgetKey, loadKey, saveKey, session, showError } from "./api.js";
import { h, icon, iconButton, toast, wireDialogBackdrops } from "./dom.js";
import { reload, renderFiles, showRoute } from "./files.js";
import { describeScope, loadKeys, renderKeysPage } from "./keys.js";
import { navigate, parseHash } from "./router.js";
import { can, isServerAdmin, state, ui } from "./state.js";
import { renderUploadPanel, stopAllUploads } from "./uploads.js";

function root() { return document.getElementById("app"); }

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
    h("img", { src: "favicon.svg", alt: "" }),
    h("h1", {}, "BlobFish"),
    h("p", { class: "muted" }, "Sign in with an API key. It stays in this browser and is sent only to this server."),
    form));
  input.focus();
}

// enterApp checks the key, learns what it may do, and shows the app.
async function enterApp(key) {
  const [me, buckets] = await Promise.all([
    api("GET", "/v1/whoami", { key }),
    api("GET", "/v1/buckets", { key }),
  ]);
  session.key = key;
  state.me = { keyId: me.keyId, name: me.name, scopes: me.scopes, maxObjectSize: me.maxObjectSize };
  state.buckets = buckets.buckets;
  route();
}

function signOut(message = "") {
  stopAllUploads();
  forgetKey();
  session.key = null;
  Object.assign(state, { me: { name: "", scopes: [], maxObjectSize: 0 }, buckets: [], apiKeys: [] });
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
  if (r.view === "files" && !state.buckets.some((b) => b.name === r.bucket)) {
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
  renderApp();
}

window.addEventListener("hashchange", route);

// ---------- Shell ----------

function renderApp() {
  root().className = "";
  root().replaceChildren(h("div", { class: "shell" },
    h("header", { class: "top" },
      h("img", { src: "favicon.svg", alt: "" }),
      h("span", { class: "brand" }, "BlobFish"),
      h("span", { class: "spacer" }),
      isServerAdmin() && h("a", {
        class: "link with-icon", href: "#/keys", "aria-current": state.route.view === "keys" ? "page" : null,
      }, icon("key"), "API keys")),
    h("nav", { class: "side", "aria-label": "Buckets" }, renderSidebar()),
    renderMain()));
}

function renderSidebar() {
  const r = state.route;
  const items = state.buckets.length === 0
    ? h("p", { class: "muted side-empty" }, isServerAdmin() ? "No buckets yet." : "This key has no buckets it can read.")
    : h("ul", {}, state.buckets.map((b) =>
      h("li", {}, h("a", {
        href: `#/b/${encodeURIComponent(b.name)}/`,
        "aria-current": r.view === "files" && b.name === r.bucket ? "true" : "false",
      }, b.name))));

  let form = null;
  if (isServerAdmin()) {
    const name = h("input", { placeholder: "new-bucket", "aria-label": "New bucket name", pattern: "[a-z0-9][a-z0-9.\\-]{1,61}[a-z0-9]" });
    form = h("form", {
      onsubmit: async (ev) => {
        ev.preventDefault();
        const n = name.value.trim();
        if (!n) return;
        try {
          await api("PUT", `/v1/buckets/${encodeURIComponent(n)}`);
          toast(`Created bucket ${n}`);
          state.buckets = (await api("GET", "/v1/buckets")).buckets;
          navigate({ view: "files", bucket: n, prefix: "", page: 1 });
        } catch (err) { showError(err); }
      },
    }, name, h("button", { type: "submit" }, "Add"));
  }
  return [
    h("div", { class: "side-scroll" }, h("h2", {}, "Buckets"), items, form),
    renderProfile(),
  ];
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
  return h("section", { class: "main", "aria-label": "Files" },
    h("p", { class: "empty" }, isServerAdmin() ? "Create a bucket to get started." : "Nothing to show for this key."));
}

ui.renderApp = renderApp;
ui.renderMain = () => {
  const old = document.querySelector(".main");
  if (old) old.replaceWith(renderMain());
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
