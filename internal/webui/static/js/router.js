// The URL hash holds where you are, so reload, bookmarks and the back
// button work:
//
//   #/b/<bucket>/<folder>/<sub>/?page=3&size=50
//   #/trash/<bucket>
//   #/keys
//
// Folder segments are percent-encoded one by one; keys may contain anything.

import { PAGE_SIZES } from "./state.js";

export function parseHash(hash = location.hash) {
  const raw = hash.replace(/^#/, "");
  const [path, query = ""] = raw.split("?", 2);
  const params = new URLSearchParams(query);
  const page = Math.max(1, parseInt(params.get("page"), 10) || 1);
  const sizeParam = parseInt(params.get("size"), 10);
  const size = PAGE_SIZES.includes(sizeParam) ? sizeParam : null;

  if (path === "/keys") return { view: "keys" };
  const t = path.match(/^\/trash\/([^/]+)\/?$/);
  if (t) {
    try { return { view: "trash", bucket: decodeURIComponent(t[1]) }; } catch { return { view: "home" }; }
  }
  const m = path.match(/^\/b\/([^/]+)\/?(.*)$/);
  if (m) {
    let prefix = "";
    try {
      prefix = m[2] ? m[2].split("/").map(decodeURIComponent).join("/") : "";
    } catch { prefix = ""; }
    if (prefix && !prefix.endsWith("/")) prefix += "/";
    return { view: "files", bucket: decodeURIComponent(m[1]), prefix, page, size };
  }
  return { view: "home" };
}

export function formatHash(route) {
  if (route.view === "keys") return "#/keys";
  if (route.view === "trash" && route.bucket) return `#/trash/${encodeURIComponent(route.bucket)}`;
  if (route.view !== "files" || !route.bucket) return "#/";
  const folder = route.prefix ? route.prefix.split("/").map(encodeURIComponent).join("/") : "";
  const params = new URLSearchParams();
  if (route.page > 1) params.set("page", route.page);
  if (route.size) params.set("size", route.size);
  const q = params.toString();
  return `#/b/${encodeURIComponent(route.bucket)}/${folder}${q ? "?" + q : ""}`;
}

// navigate changes the URL; the hashchange listener in app.js does the
// rest. replace avoids adding a history entry (for corrections).
export function navigate(route, { replace = false } = {}) {
  const hash = formatHash(route);
  if (hash === location.hash) {
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  } else if (replace) {
    history.replaceState(null, "", hash);
    window.dispatchEvent(new HashChangeEvent("hashchange"));
  } else {
    location.hash = hash;
  }
}
