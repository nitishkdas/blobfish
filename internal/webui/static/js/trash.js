// A bucket's trash: deleted files waiting to be deleted for good, with
// Restore and Delete forever. Shown to keys that may write the bucket.

import { api, showError } from "./api.js";
import { confirmDialog, count, dateFmtLong, emptyState, formatSize, h, icon, iconButton, relativeTime, toast } from "./dom.js";
import { renderBucketHeader } from "./bucket.js";
import { rerenderMain, state } from "./state.js";

const PAGE = 100;
const trashPath = (bucket) => `/v1/trash/${encodeURIComponent(bucket)}`;

// showTrash loads the first page of a bucket's trash, newest first.
export async function showTrash(route) {
  const T = state.trash;
  Object.assign(T, { bucket: route.bucket, items: [], nextCursor: "", loaded: false });
  try {
    const res = await api("GET", `${trashPath(route.bucket)}?limit=${PAGE}`);
    if (T.bucket !== route.bucket) return; // navigated away meanwhile
    Object.assign(T, { items: res.items, nextCursor: res.nextCursor || "" });
  } catch (err) { showError(err); }
  T.loaded = true;
  rerenderMain();
}

async function loadMore() {
  const T = state.trash;
  try {
    const q = new URLSearchParams({ limit: String(PAGE), cursor: T.nextCursor });
    const res = await api("GET", `${trashPath(T.bucket)}?${q}`);
    T.items.push(...res.items);
    T.nextCursor = res.nextCursor || "";
  } catch (err) { showError(err); }
  rerenderMain();
}

function drop(item) {
  state.trash.items = state.trash.items.filter((i) => i.id !== item.id);
  rerenderMain();
}

export function renderTrash() {
  const T = state.trash;
  const bucket = state.route.bucket;
  const days = state.me.trashDays;

  return h("main", { id: "main", tabindex: "-1", class: "main", "aria-label": "Trash" },
    renderBucketHeader(bucket, "trash"),
    h("div", { class: "toolbar" },
      h("div", { class: "callout" }, icon("info", 16), h("p", {}, days > 0
        ? `Deleted files stay here for ${count(days, "day")}, then are permanently deleted. Restoring a file brings back its share links.`
        : "Trash is turned off on this server, so deleted files are removed immediately. Files already here are kept until their scheduled deletion.")),
      h("div", { class: "toolbar-actions" },
        T.items.length > 0 && h("button", { class: "danger with-icon", onclick: emptyTrash }, icon("trash", 16), "Empty trash"))),
    h("div", { class: "table-wrap" },
      !T.loaded ? h("p", { class: "loading" }, "Loading…")
        : T.items.length === 0 ? emptyState({ icon: "trash", title: "Trash is empty", text: "Files deleted from this bucket appear here until they're permanently deleted." })
          : renderTable(),
      T.loaded && T.items.length > 0 && h("div", { class: "pager" },
        h("span", { class: "muted" }, `${T.items.length}${T.nextCursor ? "+" : ""} item${T.items.length === 1 ? "" : "s"}`),
        h("span", { class: "spacer" }),
        T.nextCursor && h("button", { class: "small", onclick: loadMore }, "Show more"))));
}

function renderTable() {
  return h("table", { class: "files-table" },
    h("thead", {}, h("tr", {},
      h("th", { scope: "col" }, "Name"),
      h("th", { scope: "col", class: "num" }, "Size"),
      h("th", { scope: "col", class: "when" }, "Deleted"),
      h("th", { scope: "col", class: "when" }, "Permanently deleted"),
      h("th", { scope: "col", class: "actions" }, h("span", { class: "visually-hidden" }, "Actions")))),
    h("tbody", {}, state.trash.items.map((it) => {
      const deleted = new Date(it.deleted);
      const purge = new Date(it.purgeAt);
      return h("tr", {},
        h("td", { class: "name", title: it.object.key },
          h("span", { class: "with-icon trash-name" }, h("span", { class: "file-icon" }, icon("file", 16)), it.object.key)),
        h("td", { class: "num" }, formatSize(it.object.size)),
        h("td", { class: "when", title: dateFmtLong.format(deleted) }, relativeTime(deleted)),
        h("td", { class: "when", title: dateFmtLong.format(purge) }, relativeTime(purge)),
        h("td", { class: "actions" },
          iconButton("retry", `Restore ${it.object.key}`, () => restore(it)),
          iconButton("trash", `Permanently delete ${it.object.key}`, () => purge1(it), "danger")));
    })));
}

async function restore(it) {
  try {
    await api("POST", `${trashPath(state.trash.bucket)}/${encodeURIComponent(it.id)}/restore`);
    toast("File restored", { detail: it.object.key });
    drop(it);
  } catch (err) { showError(err); }
}

async function purge1(it) {
  const ok = await confirmDialog({
    title: "Permanently delete this file?",
    body: [h("p", { class: "mono confirm-target" }, it.object.key), "This can't be undone."],
    confirmLabel: "Delete permanently",
  });
  if (!ok) return;
  try {
    await api("DELETE", `${trashPath(state.trash.bucket)}/${encodeURIComponent(it.id)}`);
    toast("File permanently deleted");
    drop(it);
  } catch (err) { showError(err); }
}

async function emptyTrash() {
  const bucket = state.trash.bucket;
  const ok = await confirmDialog({
    title: `Empty the trash of “${bucket}”?`,
    body: ["All files in it are permanently deleted. This can't be undone."],
    confirmLabel: "Empty trash",
  });
  if (!ok) return;
  try {
    await api("DELETE", trashPath(bucket));
    toast("Trash emptied");
    showTrash({ bucket });
  } catch (err) { showError(err); }
}
