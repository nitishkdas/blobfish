// A bucket's trash: deleted files waiting to be deleted for good, with
// Restore and Delete forever. Shown to keys that may write the bucket.

import { api, showError } from "./api.js";
import { confirmDialog, dateFmtLong, emptyState, formatSize, h, icon, iconButton, relativeTime, toast } from "./dom.js";
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
        ? `Deleted files stay here for ${days} day${days === 1 ? "" : "s"}, then are deleted for good. Share links work again when a file is restored.`
        : "Trash is turned off on this server, so deleted files are removed at once. Files deleted before that stay here until their time is up.")),
      h("div", { class: "toolbar-actions" },
        T.items.length > 0 && h("button", { class: "danger with-icon", onclick: emptyTrash }, icon("trash", 16), "Empty trash"))),
    h("div", { class: "table-wrap" },
      !T.loaded ? h("p", { class: "loading" }, "Loading…")
        : T.items.length === 0 ? emptyState({ icon: "trash", title: "Trash is empty", text: "Files you delete from this bucket show up here until they are purged." })
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
      h("th", { scope: "col", class: "when" }, "Gone for good"),
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
          iconButton("trash", `Delete ${it.object.key} forever`, () => purge1(it), "danger")));
    })));
}

async function restore(it) {
  try {
    await api("POST", `${trashPath(state.trash.bucket)}/${encodeURIComponent(it.id)}/restore`);
    toast("Restored", { detail: it.object.key });
    drop(it);
  } catch (err) { showError(err); }
}

async function purge1(it) {
  const ok = await confirmDialog({
    title: "Delete this file forever?",
    body: [h("p", { class: "mono confirm-target" }, it.object.key), "It can't be restored after this."],
    confirmLabel: "Delete forever",
  });
  if (!ok) return;
  try {
    await api("DELETE", `${trashPath(state.trash.bucket)}/${encodeURIComponent(it.id)}`);
    toast("Deleted forever");
    drop(it);
  } catch (err) { showError(err); }
}

async function emptyTrash() {
  const bucket = state.trash.bucket;
  const ok = await confirmDialog({
    title: `Empty the trash of “${bucket}”?`,
    body: ["Every file in it is deleted for good. This can't be undone."],
    confirmLabel: "Empty trash",
  });
  if (!ok) return;
  try {
    await api("DELETE", trashPath(bucket));
    toast("Trash emptied");
    showTrash({ bucket });
  } catch (err) { showError(err); }
}
