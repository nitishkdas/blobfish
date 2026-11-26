// Light/dark theme switch. Without a saved choice the UI follows the
// system setting (CSS color-scheme: light dark); a click saves the
// opposite of what is showing, as data-theme on <html>.

import { h, icon } from "./dom.js";

const PREF = "blobfish.pref.theme";
const media = matchMedia("(prefers-color-scheme: dark)");

function saved() {
  try { return localStorage.getItem(PREF); } catch { return null; }
}

// applyTheme sets the saved choice on <html>; call it once at start.
export function applyTheme() {
  const t = saved();
  if (t === "light" || t === "dark") document.documentElement.dataset.theme = t;
}

function current() {
  return document.documentElement.dataset.theme || (media.matches ? "dark" : "light");
}

// themeToggle is the sun/moon button. It shows the mode a click switches to.
export function themeToggle() {
  const btn = h("button", { type: "button", class: "icon-btn theme-toggle" });
  const paint = () => {
    const next = current() === "dark" ? "light" : "dark";
    const label = `Switch to ${next} mode`;
    btn.setAttribute("aria-label", label);
    btn.title = label;
    btn.replaceChildren(icon(next === "dark" ? "moon" : "sun"));
  };
  btn.addEventListener("click", () => {
    const next = current() === "dark" ? "light" : "dark";
    // Switch instantly: without this every button fades its colours.
    const root = document.documentElement;
    root.classList.add("theme-switching");
    root.dataset.theme = next;
    requestAnimationFrame(() => requestAnimationFrame(() => root.classList.remove("theme-switching")));
    try { localStorage.setItem(PREF, next); } catch { /* private mode: this page only */ }
    for (const b of document.querySelectorAll(".theme-toggle")) b.dispatchEvent(new Event("themechange"));
  });
  btn.addEventListener("themechange", paint);
  paint();
  return btn;
}
