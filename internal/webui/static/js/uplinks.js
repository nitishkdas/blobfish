// The "Upload links" dialog: a bucket's upload links (copy, revoke) and a
// form for a new one into the folder on screen.

import { api, showError } from "./api.js";
import { confirmDialog, copyText, formatSize, h, iconButton, relativeTime, toast } from "./dom.js";
import { can, state } from "./state.js";

const MB = 1024 * 1024;

export async function openUploadLinks(bucket, folder) {
  const dialog = document.getElementById("uplinks");
  const list = document.getElementById("uplinks-list");
  document.getElementById("uplinks-title").textContent = `Upload links for “${bucket}”`;
  const folderEl = document.getElementById("uplinks-folder");
  folderEl.value = folder;

  let links = [];
  const render = () => {
    if (links.length === 0) {
      list.replaceChildren(h("li", { class: "muted share-none" }, "No upload links yet."));
      return;
    }
    list.replaceChildren(...links.map((l) => {
      const field = h("input", { class: "mono", readonly: true, value: l.url, "aria-label": "Upload link" });
      const expires = l.expires ? new Date(l.expires) : null;
      const expired = expires && expires < new Date();
      const full = l.maxFiles > 0 && l.uploads >= l.maxFiles;
      const mayRevoke = l.createdBy === state.me.keyId || can("w", bucket);
      const meta = [
        l.label && `“${l.label}”`,
        `into ${l.prefix ? l.prefix : "the top level"}`,
        l.maxFiles ? `${l.uploads} of ${l.maxFiles} files` : `${l.uploads} file${l.uploads === 1 ? "" : "s"} so far`,
        l.maxFileSize && `max ${formatSize(l.maxFileSize)} each`,
        expires ? (expired ? `expired ${relativeTime(expires)}` : `expires ${relativeTime(expires)}`) : "never expires",
      ].filter(Boolean).join(" · ");
      return h("li", { class: expired || full || !l.active ? "share-link expired" : "share-link" },
        h("div", { class: "share-link-row" },
          field,
          iconButton("copy", "Copy link", () => copyText(l.url, "Link copied", field)),
          mayRevoke && iconButton("trash", "Revoke link", () => revoke(l), "danger")),
        !l.active && h("span", { class: "share-meta warn-text" },
          `Not working: ${l.createdByName || "its creator"} was removed or can no longer write to this bucket. It works again if their access returns.`),
        h("span", { class: "share-meta" }, meta));
    }));
  };
  const load = async () => {
    try {
      links = (await api("GET", `/v1/uploadlinks?bucket=${encodeURIComponent(bucket)}`)).uploadLinks;
    } catch (err) { showError(err); links = []; }
    render();
  };
  const revoke = async (l) => {
    const ok = await confirmDialog({
      title: "Revoke this upload link?",
      body: [h("p", { class: "mono confirm-target" }, l.url), "Nobody can upload with it any more. Files already sent stay."],
      confirmLabel: "Revoke link",
    });
    if (!ok) return;
    try {
      await api("DELETE", `/v1/uploadlinks/${encodeURIComponent(l.id)}`);
      toast("Upload link revoked");
    } catch (err) { showError(err); }
    await load();
  };

  const labelEl = document.getElementById("uplinks-label");
  const filesEl = document.getElementById("uplinks-max-files");
  const sizeEl = document.getElementById("uplinks-max-size");
  labelEl.value = filesEl.value = sizeEl.value = "";
  const create = document.getElementById("uplinks-create");
  create.onclick = async () => {
    const body = {
      bucket, folder: folderEl.value.trim(), label: labelEl.value.trim(),
      expiresIn: Number(document.getElementById("uplinks-ttl").value),
      maxFiles: Math.max(0, Math.floor(Number(filesEl.value) || 0)),
      maxFileSize: Math.max(0, Math.round((Number(sizeEl.value) || 0) * MB)),
    };
    if (state.me.maxObjectSize && body.maxFileSize > state.me.maxObjectSize) {
      toast(`Max size per file can be at most ${formatSize(state.me.maxObjectSize)}, the server's limit.`, { error: true });
      return;
    }
    create.disabled = true;
    try {
      const l = await api("POST", "/v1/uploadlinks", { body });
      labelEl.value = filesEl.value = sizeEl.value = "";
      await load();
      await copyText(l.url, "Upload link created and copied");
    } catch (err) {
      showError(err);
    } finally {
      create.disabled = false;
    }
  };

  list.replaceChildren(h("li", { class: "muted share-none" }, "Loading…"));
  dialog.showModal();
  await load();
}
