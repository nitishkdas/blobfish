// DOM helpers shared by every view. Every element is built with
// createElement and textContent, never by parsing HTML strings, so bucket
// names and object keys cannot inject markup.

export function h(tag, attrs = {}, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "class") el.className = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c instanceof Node ? c : String(c));
  }
  return el;
}

// Simple stroked 24×24 icons, drawn for BlobFish.
const ICONS = {
  download: ["M12 4v11", "M7 10l5 5 5-5", "M5 20h14"],
  upload: ["M12 16V5", "M7 10l5-5 5 5", "M5 20h14"],
  share: ["M10 14a4 4 0 0 0 5.66 0l3-3a4 4 0 0 0-5.66-5.66l-1 1", "M14 10a4 4 0 0 0-5.66 0l-3 3a4 4 0 0 0 5.66 5.66l1-1"],
  trash: ["M4 7h16", "M9 7V4h6v3", "M6 7l1 13h10l1-13", "M10 11v6", "M14 11v6"],
  close: ["M6 6l12 12", "M18 6L6 18"],
  chevron: ["M6 9l6 6 6-6"],
  left: ["M15 6l-6 6 6 6"],
  right: ["M9 6l6 6-6 6"],
  check: ["M5 12.5l4.5 4.5L19 7"],
  alert: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18", "M12 7.5v5.5", "M12 16.5v.01"],
  info: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18", "M12 11v5.5", "M12 7.5v.01"],
  retry: ["M4 12a8 8 0 1 0 2.35-5.65", "M4 4v4.5h4.5"],
  key: ["M8 8a4 4 0 1 0 0 8a4 4 0 1 0 0-8", "M12 12h9", "M18 12v3", "M21 12v2.5"],
  plus: ["M12 5v14", "M5 12h14"],
  folder: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"],
  folderPlus: ["M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z", "M12 10.5v5", "M9.5 13h5"],
  copy: ["M9 9h10v10H9z", "M5 15V5h10"],
  globe: ["M12 3a9 9 0 1 0 0 18a9 9 0 1 0 0-18", "M3.5 12h17", "M12 3c2.4 2.5 3.6 5.5 3.6 9s-1.2 6.5-3.6 9", "M12 3c-2.4 2.5-3.6 5.5-3.6 9s1.2 6.5 3.6 9"],
  signOut: ["M9 4H6a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h3", "M16 8l4 4-4 4", "M20 12H10"],
  file: ["M7 3h7l5 5v11a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z", "M14 3v5h5"],
  fileText: ["M7 3h7l5 5v11a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2z", "M14 3v5h5", "M9 13h6", "M9 17h4"],
  image: ["M6 4h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z", "M9 8a1.5 1.5 0 1 0 0 3a1.5 1.5 0 1 0 0-3", "M20 15l-4.5-4.5L6 20"],
  archive: ["M4 4h16v4H4z", "M5 8v10a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8", "M10 12h4"],
  film: ["M6 4h12a2 2 0 0 1 2 2v12a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2z", "M10 9l5 3-5 3z"],
  database: ["M12 3c4.4 0 8 1.3 8 3s-3.6 3-8 3s-8-1.3-8-3s3.6-3 8-3z", "M4 6v12c0 1.7 3.6 3 8 3s8-1.3 8-3V6", "M4 12c0 1.7 3.6 3 8 3s8-1.3 8-3"],
  sliders: ["M4 7h9", "M19 7h1", "M16 4.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5", "M4 17h3", "M13 17h7", "M10 14.5a2.5 2.5 0 1 0 0 5a2.5 2.5 0 1 0 0-5"],
  lock: ["M7 11h10a1 1 0 0 1 1 1v7a1 1 0 0 1-1 1H7a1 1 0 0 1-1-1v-7a1 1 0 0 1 1-1z", "M8.5 11V8a3.5 3.5 0 0 1 7 0v3"],
  inbox: ["M4 13l2.5-8h11l2.5 8", "M4 13v5a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-5", "M4 13h4.5l1 2h5l1-2H20"],
  bolt: ["M13 3L5 14h6l-1 7 8-11h-6z"],
  sun: ["M12 8a4 4 0 1 0 0 8a4 4 0 1 0 0-8", "M12 2.5v2", "M12 19.5v2", "M4.9 4.9l1.4 1.4", "M17.7 17.7l1.4 1.4", "M2.5 12h2", "M19.5 12h2", "M4.9 19.1l1.4-1.4", "M17.7 6.3l1.4-1.4"],
  moon: ["M20 14.5A8 8 0 1 1 9.5 4a6.5 6.5 0 0 0 10.5 10.5z"],
  eye: ["M2.5 12s3.5-6.5 9.5-6.5S21.5 12 21.5 12s-3.5 6.5-9.5 6.5S2.5 12 2.5 12z", "M12 9a3 3 0 1 0 0 6a3 3 0 1 0 0-6"],
  eyeOff: ["M2.5 12s3.5-6.5 9.5-6.5c1.6 0 3 .5 4.3 1.2", "M21.5 12s-3.5 6.5-9.5 6.5c-1.6 0-3-.5-4.3-1.2", "M9.9 9.9a3 3 0 0 0 4.2 4.2", "M4 4l16 16"],
  users: ["M9 11a3.5 3.5 0 1 0 0-7a3.5 3.5 0 1 0 0 7", "M2.5 20c.6-3.4 3.2-5.5 6.5-5.5s5.9 2.1 6.5 5.5", "M16 4.3a3.5 3.5 0 0 1 0 6.4", "M18 14.8c2 .7 3.2 2.5 3.5 5.2"],
  storage: ["M4 5h16v5H4z", "M4 14h16v5H4z", "M7.5 7.5h.01", "M7.5 16.5h.01"],
};

export function icon(name, size = 18) {
  const NS = "http://www.w3.org/2000/svg";
  const svg = document.createElementNS(NS, "svg");
  for (const [k, v] of Object.entries({
    viewBox: "0 0 24 24", width: size, height: size, fill: "none", stroke: "currentColor",
    "stroke-width": 1.8, "stroke-linecap": "round", "stroke-linejoin": "round", "aria-hidden": "true",
  })) svg.setAttribute(k, v);
  for (const d of ICONS[name]) {
    const p = document.createElementNS(NS, "path");
    p.setAttribute("d", d);
    svg.append(p);
  }
  return svg;
}

// iconButton is a square button whose label lives in aria-label and title.
export function iconButton(name, label, onclick, extraClass = "") {
  return h("button", { class: `icon-btn ${extraClass}`.trim(), type: "button", "aria-label": label, title: label, onclick }, icon(name));
}

// toast shows a short message. Errors go to the assertive #alerts region
// and stay until dismissed (no time limit to read them, WCAG 2.2.1); at
// most three are kept. Other messages are polite and fade after 5 s.
export function toast(message, { error = false, detail = "" } = {}) {
  const text = h("div", { class: "toast-text" }, message, detail && h("small", {}, detail));
  if (!error) {
    const el = h("div", { class: "toast" }, text);
    document.getElementById("toasts").append(el);
    setTimeout(() => el.remove(), 5000);
    return;
  }
  const alerts = document.getElementById("alerts");
  const el = h("div", { class: "toast error" }, text,
    iconButton("close", "Dismiss", () => el.remove(), "small"));
  alerts.append(el);
  while (alerts.children.length > 3) alerts.firstElementChild.remove();
}

// announce tells screen-reader users about a change they can't see happen
// (a new page, an upload finishing) without showing anything.
export function announce(message) {
  const el = document.getElementById("announce");
  if (!el) return;
  // Clear first so the same text twice is still announced.
  el.textContent = "";
  setTimeout(() => { el.textContent = message; }, 50);
}

const sizeFmt = new Intl.NumberFormat(undefined, { maximumFractionDigits: 1 });
const intFmt = new Intl.NumberFormat();
export function formatSize(n) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let i = 0;
  while (n >= 1024 && i < units.length - 1) { n /= 1024; i++; }
  return `${i === 0 ? n : sizeFmt.format(n)} ${units[i]}`;
}
export function formatInt(n) { return intFmt.format(n); }

// count(3, "file") = "3 files"; count(1, "file") = "1 file".
export function count(n, word) { return `${formatInt(n)} ${word}${n === 1 ? "" : "s"}`; }
export const dateFmt = new Intl.DateTimeFormat(undefined, { dateStyle: "medium", timeStyle: "short" });
export const dateFmtLong = new Intl.DateTimeFormat(undefined, { dateStyle: "full", timeStyle: "medium" });

const relFmt = new Intl.RelativeTimeFormat(undefined, { numeric: "auto" });
// relativeTime formats a date as "in 6 days", "2 hours ago" and so on.
export function relativeTime(date) {
  let secs = (date.getTime() - Date.now()) / 1000;
  for (const [unit, size] of [["second", 60], ["minute", 60], ["hour", 24], ["day", 30], ["month", 12], ["year", Infinity]]) {
    // Round before comparing, so 59.99 minutes reads "in 1 hour", not "in 60 minutes".
    const n = Math.round(secs);
    if (Math.abs(n) < size) return relFmt.format(n, unit);
    secs /= size;
  }
  return "";
}

export function baseName(key) { return key.slice(key.lastIndexOf("/") + 1) || key; }
export function dirOf(key) { return key.slice(0, key.lastIndexOf("/") + 1); }

export async function copyText(text, done, fallbackInput) {
  try {
    await navigator.clipboard.writeText(text);
    toast(done);
  } catch {
    // The clipboard API needs HTTPS; let the user copy by hand.
    fallbackInput?.select();
    toast("Press Ctrl+C (⌘C) to copy");
  }
}

// ---------- Dialogs ----------

// ask is the one app-styled dialog behind confirm, choice and prompt. It
// resolves {choice, value} for a button press, or null for Esc, a click on
// the backdrop, or a button whose value is "" (Cancel).
//
//   choices: [{ label, value, kind: "primary" | "danger" | "" }]
//   input:   { label, value, placeholder, validate(v) -> error string or "" }
//   focus:   value of the button to focus (defaults to Cancel when present)
export function ask({ title, body = [], choices, input = null, focus = "" }) {
  const dialog = document.getElementById("ask");
  const form = dialog.querySelector("form");
  document.getElementById("ask-title").textContent = title;
  document.getElementById("ask-body").replaceChildren(
    ...[body].flat().filter(Boolean).map((part) => (typeof part === "string" ? h("p", {}, part) : part)));

  const field = document.getElementById("ask-field");
  const inputEl = document.getElementById("ask-input");
  const error = document.getElementById("ask-error");
  error.textContent = "";
  inputEl.removeAttribute("aria-invalid");
  inputEl.setAttribute("aria-describedby", error.id);
  inputEl.oninput = () => { // a stale error would contradict the new value
    error.textContent = "";
    inputEl.removeAttribute("aria-invalid");
  };
  field.hidden = !input;
  if (input) {
    document.getElementById("ask-label").textContent = input.label;
    inputEl.value = input.value || "";
    inputEl.placeholder = input.placeholder || "";
  }

  const buttons = choices.map((c) => h("button", {
    value: c.value,
    class: c.kind === "danger" ? "danger-solid" : c.kind === "primary" ? "primary" : null,
  }, c.label));
  document.getElementById("ask-actions").replaceChildren(...buttons);

  const onSubmit = (ev) => {
    const value = ev.submitter?.value ?? "";
    if (input && value && input.validate) {
      const problem = input.validate(inputEl.value.trim());
      if (problem) {
        ev.preventDefault();
        error.textContent = problem;
        inputEl.setAttribute("aria-invalid", "true");
        inputEl.focus();
      }
    }
  };
  form.addEventListener("submit", onSubmit);
  dialog.returnValue = "";

  return new Promise((resolve) => {
    dialog.addEventListener("close", () => {
      form.removeEventListener("submit", onSubmit);
      const choice = dialog.returnValue;
      resolve(choice ? { choice, value: inputEl.value.trim() } : null);
    }, { once: true });
    dialog.showModal();
    if (input) {
      inputEl.focus();
      inputEl.select();
    } else {
      (buttons.find((b) => b.value === focus) || buttons.find((b) => b.value === "") || buttons[0]).focus();
    }
  });
}

// confirmDialog asks yes/no with Cancel focused, so a reflexive Enter is safe.
export async function confirmDialog({ title, body = [], confirmLabel, danger = true }) {
  const res = await ask({
    title, body,
    choices: [{ label: "Cancel", value: "" }, { label: confirmLabel, value: "ok", kind: danger ? "danger" : "primary" }],
  });
  return res?.choice === "ok";
}

// Clicking the dimmed backdrop (the dialog element itself, outside its
// form) closes any dialog, like Esc does.
export function wireDialogBackdrops() {
  for (const d of document.querySelectorAll("dialog")) {
    d.addEventListener("click", (e) => { if (e.target === d) d.close(); });
  }
}

// emptyState is the placeholder for a list with nothing in it: an icon,
// a short title, one line of help and an optional action.
export function emptyState({ icon: name, title, text = "", action = null }) {
  return h("div", { class: "empty-state" },
    h("span", { class: "empty-icon", "aria-hidden": "true" }, icon(name, 22)),
    h("h3", {}, title),
    text && h("p", {}, text),
    action && h("div", { class: "empty-actions" }, action));
}

// formDialog shows a form in the shared #form-dialog. onSubmit runs on
// submit; if it throws, the message shows in the dialog and it stays open.
// Resolves true once onSubmit succeeded, false if cancelled.
export function formDialog({ title, text = "", body, submitLabel, onSubmit, focus = null }) {
  const dialog = document.getElementById("form-dialog");
  const form = document.getElementById("form-dialog-form");
  const submit = document.getElementById("form-dialog-submit");
  const error = document.getElementById("form-dialog-error");
  document.getElementById("form-dialog-title").textContent = title;
  const textEl = document.getElementById("form-dialog-text");
  textEl.textContent = text;
  textEl.hidden = !text;
  document.getElementById("form-dialog-body").replaceChildren(...[body].flat().filter(Boolean));
  submit.textContent = submitLabel;
  error.textContent = "";
  form.setAttribute("aria-describedby", error.id);
  form.oninput = () => { error.textContent = ""; };
  let done = false;
  form.onsubmit = async (ev) => {
    ev.preventDefault();
    submit.disabled = true;
    try {
      await onSubmit();
      done = true;
      dialog.close();
    } catch (err) {
      error.textContent = err.message || String(err);
    } finally {
      submit.disabled = false;
    }
  };
  document.getElementById("form-dialog-cancel").onclick = () => dialog.close();
  return new Promise((resolve) => {
    dialog.addEventListener("close", () => resolve(done), { once: true });
    dialog.showModal();
    (focus || form.querySelector("input, select"))?.focus();
  });
}

// passwordInput is a password field with a show/hide toggle.
export function passwordInput(attrs = {}) {
  const input = h("input", { type: "password", autocomplete: "new-password", spellcheck: "false", ...attrs });
  const toggle = h("button", {
    type: "button", class: "icon-btn small reveal", "aria-label": "Show password", title: "Show password",
    onclick: () => {
      const show = input.type === "password";
      input.type = show ? "text" : "password";
      toggle.setAttribute("aria-label", show ? "Hide password" : "Show password");
      toggle.title = toggle.getAttribute("aria-label");
      toggle.replaceChildren(icon(show ? "eyeOff" : "eye", 16));
    },
  }, icon("eye", 16));
  return { el: h("div", { class: "password-field" }, input, toggle), input };
}

let fieldSeq = 0;
// field is a labelled form row: the label names the control (for/id) and
// the help text describes it (aria-describedby), so screen readers get a
// short name, and buttons beside the input don't join it.
export function field(label, control, help = "") {
  const input = control.matches?.("input, select, textarea") ? control : control.querySelector("input, select, textarea");
  const id = input.id || `f${++fieldSeq}`;
  input.id = id;
  const helpEl = help && h("span", { class: "field-help", id: `${id}-help` }, help);
  if (helpEl) input.setAttribute("aria-describedby", helpEl.id);
  return h("div", { class: "field" }, h("label", { for: id }, label), control, helpEl);
}

// rememberFocus notes which control inside el has focus, by what it is
// (id, or tag + label + text + href), so that after el's content is
// rebuilt, restore(newEl) can focus the matching new control, or newEl
// itself, instead of letting focus fall to <body>. Does nothing when
// focus was elsewhere.
export function rememberFocus(el) {
  const a = document.activeElement;
  if (!a || !el.contains(a)) return { restore() {} };
  const k = { tag: a.tagName, id: a.id, label: a.getAttribute("aria-label"), text: a.textContent, href: a.getAttribute("href") };
  return {
    restore(root) {
      let target = null;
      if (k.id) target = root.id === k.id ? root : root.querySelector(`#${CSS.escape(k.id)}`);
      target ||= [...root.querySelectorAll(k.tag)].find((c) =>
        c.getAttribute("aria-label") === k.label && c.textContent === k.text && c.getAttribute("href") === k.href);
      if (!target && root.tabIndex < 0 && !root.hasAttribute("tabindex")) root.setAttribute("tabindex", "-1");
      (target || root).focus({ preventScroll: true });
    },
  };
}
