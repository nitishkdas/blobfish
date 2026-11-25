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

export function toast(message, { error = false, detail = "" } = {}) {
  const el = h("div", { class: error ? "toast error" : "toast" }, message, detail && h("small", {}, detail));
  document.getElementById("toasts").append(el);
  setTimeout(() => el.remove(), error ? 8000 : 3500);
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
