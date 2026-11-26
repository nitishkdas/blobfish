// Users page (server admins): people who sign in with a name and password.
// API keys stay for apps; users are for people using this web UI.

import { api, showError } from "./api.js";
import {
  confirmDialog, dateFmt, dateFmtLong, emptyState, field, formDialog, h, icon, iconButton, passwordInput, relativeTime, toast,
} from "./dom.js";
import { describeScope, scopeEditor } from "./keys.js";
import { rerenderMain, state } from "./state.js";

const MIN_PASSWORD = 10;

export async function loadUsers() {
  try {
    state.users = (await api("GET", "/v1/users")).users;
  } catch (err) { showError(err); }
  rerenderMain();
}

export function renderUsersPage() {
  return h("section", { class: "main users-page", "aria-label": "Users" },
    h("header", { class: "page-head" },
      h("span", { class: "bucket-icon", "aria-hidden": "true" }, icon("users", 20)),
      h("div", { class: "page-head-text" },
        h("h1", {}, "Users ", h("span", { class: "count" }, String(state.users.length))),
        h("p", { class: "muted" },
          "People who sign in to this web UI with a name and password. Apps should use API keys instead.")),
      h("button", { class: "primary with-icon", onclick: () => openUserDialog(null) }, icon("plus", 16), "Add user")),
    renderTable());
}

function renderTable() {
  if (state.users.length === 0) {
    return h("div", { class: "table-wrap" }, emptyState({
      icon: "users", title: "No users yet",
      text: "Add a user for each person, with access to just the buckets they need.",
      action: h("button", { class: "primary with-icon", onclick: () => openUserDialog(null) }, icon("plus", 16), "Add user"),
    }));
  }
  return h("div", { class: "table-wrap" }, h("table", {},
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Name"),
      h("th", { scope: "col" }, "Access"),
      h("th", { scope: "col", class: "when" }, "Last sign-in"),
      h("th", { scope: "col", class: "when" }, "Created"),
      h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
    h("tbody", {}, state.users.map((u) => h("tr", {},
      h("td", { class: "name" }, u.name, u.name === state.me.user && h("span", { class: "chip you" }, "you")),
      h("td", {}, h("div", { class: "chips" }, u.scopes.map((sc) =>
        h("span", { class: sc.startsWith("*:admin") ? "chip strong" : "chip" }, describeScope(sc))))),
      h("td", { class: "when", title: u.lastLogin ? dateFmtLong.format(new Date(u.lastLogin)) : "" },
        u.lastLogin ? relativeTime(new Date(u.lastLogin)) : h("span", { class: "muted" }, "Never")),
      h("td", { class: "when" }, dateFmt.format(new Date(u.created))),
      h("td", { class: "actions" },
        iconButton("sliders", `Edit access for ${u.name}`, () => openUserDialog(u)),
        iconButton("lock", `Reset password for ${u.name}`, () => resetPassword(u)),
        iconButton("signOut", `Sign ${u.name} out everywhere`, () => signOutEverywhere(u)),
        iconButton("trash", `Delete ${u.name}`, () => deleteUser(u), "danger")))))));
}

// openUserDialog adds a user (u null) or edits an existing user's access.
function openUserDialog(u) {
  const access = scopeEditor(u ? u.scopes : []);
  if (u) {
    formDialog({
      title: `Access for “${u.name}”`,
      text: "Changes apply at once, also to their open sessions.",
      body: [access.el],
      submitLabel: "Save",
      onSubmit: async () => {
        await api("PATCH", `/v1/users/${encodeURIComponent(u.name)}`, { body: { scopes: access.read() } });
        toast(`Access for ${u.name} updated`);
        loadUsers();
      },
    });
    return;
  }
  const name = h("input", { required: true, maxlength: 64, autocomplete: "off", spellcheck: "false", placeholder: "e.g. priya" });
  const pw = passwordInput({ required: true, minlength: MIN_PASSWORD, maxlength: 256 });
  formDialog({
    title: "Add user",
    text: "Share the password with them safely; they can change it after signing in.",
    body: [
      field("User name", name, "Lowercase letters, digits, dots, dashes or underscores."),
      field("Password", pw.el, `At least ${MIN_PASSWORD} characters. A few random words work well.`),
      access.el,
    ],
    submitLabel: "Add user",
    focus: name,
    onSubmit: async () => {
      const created = await api("POST", "/v1/users", {
        body: { name: name.value.trim(), password: pw.input.value, scopes: access.read() },
      });
      toast(`Added ${created.name}`);
      loadUsers();
    },
  });
}

function resetPassword(u) {
  const pw = passwordInput({ required: true, minlength: MIN_PASSWORD, maxlength: 256 });
  formDialog({
    title: `New password for “${u.name}”`,
    text: "They are signed out everywhere and sign in again with this password.",
    body: [field("New password", pw.el, `At least ${MIN_PASSWORD} characters.`)],
    submitLabel: "Set password",
    onSubmit: async () => {
      await api("PATCH", `/v1/users/${encodeURIComponent(u.name)}`, { body: { password: pw.input.value } });
      toast(`Password for ${u.name} changed`);
      loadUsers();
    },
  });
}

async function signOutEverywhere(u) {
  const ok = await confirmDialog({
    title: `Sign “${u.name}” out everywhere?`,
    body: ["Every browser signed in as this user has to sign in again. Their password doesn't change."],
    confirmLabel: "Sign out everywhere",
  });
  if (!ok) return;
  try {
    const { sessionsEnded } = await api("DELETE", `/v1/users/${encodeURIComponent(u.name)}/sessions`);
    toast(sessionsEnded ? `Ended ${sessionsEnded} session${sessionsEnded === 1 ? "" : "s"}` : `${u.name} wasn't signed in anywhere`);
  } catch (err) { showError(err); }
}

async function deleteUser(u) {
  const self = u.name === state.me.user;
  const ok = await confirmDialog({
    title: `Delete user “${u.name}”?`,
    body: [
      "They are signed out at once and can't sign in again. Files, links and keys are not affected.",
      self && h("p", { class: "warn" }, "This is you. You'll be signed out."),
    ],
    confirmLabel: "Delete user",
  });
  if (!ok) return;
  try {
    await api("DELETE", `/v1/users/${encodeURIComponent(u.name)}`);
    toast(`Deleted ${u.name}`);
    if (!self) loadUsers();
    else await api("GET", "/v1/whoami").catch(() => {}); // 401 signs us out
  } catch (err) {
    if (err.status !== 401) showError(err);
  }
}

// changeOwnPassword: the signed-in user changes their own password. Their
// other sessions end; this one stays.
export function changeOwnPassword() {
  const current = passwordInput({ required: true, autocomplete: "current-password" });
  const next = passwordInput({ required: true, minlength: MIN_PASSWORD, maxlength: 256 });
  const again = passwordInput({ required: true, minlength: MIN_PASSWORD, maxlength: 256 });
  formDialog({
    title: "Change your password",
    text: "You stay signed in here; other browsers signed in as you are signed out.",
    body: [
      field("Current password", current.el),
      field("New password", next.el, `At least ${MIN_PASSWORD} characters.`),
      field("New password again", again.el),
    ],
    submitLabel: "Change password",
    onSubmit: async () => {
      if (next.input.value !== again.input.value) throw new Error("The new passwords don't match.");
      try {
        await api("POST", "/v1/password", { body: { currentPassword: current.input.value, newPassword: next.input.value } });
      } catch (err) {
        if (err.code === "InvalidCredentials") throw new Error("The current password is wrong.");
        throw err;
      }
      toast("Password changed");
    },
  });
}
