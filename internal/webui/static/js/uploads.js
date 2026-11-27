// Uploads: picking and dropping files or whole folders, asking before
// overwriting, and the docked queue panel.

import { ApiError, objectExists, objectPath, session, showError } from "./api.js";
import { announce, ask, baseName, dirOf, formatSize, h, icon, iconButton, rememberFocus, toast } from "./dom.js";
import { can, state, ui } from "./state.js";

let nextUploadId = 1;
let pumping = false;

// ---------- Picking files ----------

// pickAndUpload opens the system picker for files, or for one folder
// (keeping its inner paths), and uploads into the folder on screen.
export function pickAndUpload({ folder }) {
  const input = h("input", { type: "file", multiple: true, hidden: true, webkitdirectory: folder || null });
  input.addEventListener("change", () => {
    const picked = [...input.files].map((file) => ({ file, path: file.webkitRelativePath || file.name }));
    input.remove();
    if (picked.length) startUploads(picked);
  });
  document.body.append(input);
  input.click();
}

export function wireDrop(el) {
  let depth = 0;
  el.addEventListener("dragenter", (e) => { e.preventDefault(); depth++; el.classList.add("dragging"); });
  el.addEventListener("dragleave", () => { if (--depth <= 0) { depth = 0; el.classList.remove("dragging"); } });
  el.addEventListener("dragover", (e) => e.preventDefault());
  el.addEventListener("drop", (e) => {
    e.preventDefault();
    depth = 0;
    el.classList.remove("dragging");
    // Entries must be taken now: the DataTransfer is emptied once this
    // handler returns, and reading folders is asynchronous.
    const entries = [...(e.dataTransfer?.items || [])]
      .filter((i) => i.kind === "file")
      .map((i) => i.webkitGetAsEntry?.())
      .filter(Boolean);
    const plainFiles = [...(e.dataTransfer?.files || [])];
    collectDropped(entries, plainFiles).then((picked) => {
      if (picked.length) startUploads(picked);
    }).catch(showError);
  });
}

// collectDropped walks dropped folders so "photos/2026/a.jpg" keeps its path.
async function collectDropped(entries, plainFiles) {
  if (entries.length === 0) return plainFiles.map((file) => ({ file, path: file.name }));
  const out = [];
  const walk = async (entry, dir) => {
    if (entry.isFile) {
      const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
      out.push({ file, path: dir + file.name });
    } else if (entry.isDirectory) {
      const reader = entry.createReader();
      // readEntries returns results in batches until it returns none.
      for (;;) {
        const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
        if (batch.length === 0) break;
        for (const child of batch) await walk(child, `${dir}${entry.name}/`);
      }
    }
  };
  for (const entry of entries) await walk(entry, "");
  return out;
}

// ---------- Before the upload: overwrite check ----------

async function startUploads(picked) {
  const bucket = state.route.bucket;
  const prefix = state.route.prefix;
  if (!bucket || !can("w", bucket)) {
    toast("You don't have permission to upload to this bucket.", { error: true });
    return;
  }
  let jobs = picked.map(({ file, path }) => ({ file, key: prefix + path }));
  jobs = await resolveConflicts(bucket, jobs);
  if (!jobs) return; // cancelled
  for (const { file, key } of jobs) {
    const job = { id: nextUploadId++, file, bucket, key, status: "queued", progress: 0, error: null, xhr: null };
    const max = state.me.maxObjectSize;
    if (max && file.size > max) {
      job.status = "failed";
      job.error = `Larger than the ${formatSize(max)} limit on this server`;
    }
    state.uploads.push(job);
  }
  state.uploadsCollapsed = false;
  renderUploadPanel();
  pumpUploads();
}

// resolveConflicts asks what to do with names that already exist:
// replace them, skip them, or keep both under a new name.
async function resolveConflicts(bucket, jobs) {
  if (!can("r", bucket)) return jobs; // a write-only key can't check; upload as asked
  const exists = await mapLimit(jobs, 6, (j) => objectExists(bucket, j.key));
  const clashes = jobs.filter((_, i) => exists[i] === true);
  if (clashes.length === 0) return jobs;

  const shown = clashes.slice(0, 5).map((j) => h("li", { class: "mono" }, j.key.slice(state.route.prefix.length)));
  if (clashes.length > 5) shown.push(h("li", { class: "muted" }, `and ${clashes.length - 5} more`));
  const res = await ask({
    title: clashes.length === 1 ? "This file already exists" : `${clashes.length} files already exist`,
    body: [
      h("ul", { class: "clash-list" }, shown),
      "Replace overwrites the existing file. Keep both saves the new file with a number added, for example “report (1).pdf”.",
    ],
    choices: [
      { label: "Cancel", value: "" },
      { label: "Skip", value: "skip" },
      { label: "Keep both", value: "both" },
      { label: "Replace", value: "replace", kind: "danger" },
    ],
    focus: "both",
  });
  if (!res) return null;
  if (res.choice === "replace") return jobs;
  if (res.choice === "skip") return jobs.filter((_, i) => exists[i] !== true);

  // Keep both: find a free "name (n).ext" for each clash, not reusing one
  // already taken by another file in this batch.
  const taken = new Set(jobs.map((j) => j.key));
  for (const j of clashes) {
    j.key = await freeName(bucket, j.key, taken);
    taken.add(j.key);
  }
  return jobs;
}

async function freeName(bucket, key, taken) {
  const dir = dirOf(key);
  const name = baseName(key);
  const dot = name.lastIndexOf(".");
  const stem = dot > 0 ? name.slice(0, dot) : name;
  const ext = dot > 0 ? name.slice(dot) : "";
  for (let n = 1; n < 1000; n++) {
    const candidate = `${dir}${stem} (${n})${ext}`;
    if (!taken.has(candidate) && (await objectExists(bucket, candidate)) === false) return candidate;
  }
  throw new Error(`No free name is available for ${name}`);
}

// mapLimit runs fn over items with at most n requests in flight.
async function mapLimit(items, n, fn) {
  const out = new Array(items.length);
  let next = 0;
  const worker = async () => {
    while (next < items.length) {
      const i = next++;
      out[i] = await fn(items[i]);
    }
  };
  await Promise.all(Array.from({ length: Math.min(n, items.length) }, worker));
  return out;
}

// ---------- Queue ----------

// pumpUploads runs queued jobs one at a time: a small server prefers a
// steady stream to a stampede of parallel multi-GB uploads.
async function pumpUploads() {
  if (pumping) return;
  pumping = true;
  try {
    let job;
    while ((job = state.uploads.find((u) => u.status === "queued"))) {
      job.status = "uploading";
      renderUploadPanel();
      try {
        await putFile(job);
        job.status = "done";
        announce(`${baseName(job.key)} uploaded`);
        job.doneAt = Date.now();
        // Fade finished rows out shortly after, then drop them. Capture the
        // id now: `job` is reassigned to the next upload by the loop.
        const doneId = job.id;
        setTimeout(() => removeUpload(doneId), 2500);
        const r = state.route;
        if (r.view === "files" && job.bucket === r.bucket && job.key.startsWith(r.prefix)) ui.reloadFiles();
      } catch (err) {
        if (job.status === "cancelled") continue;
        job.status = "failed";
        job.error = err instanceof ApiError ? err.message : err.message;
        announce(`${baseName(job.key)} failed: ${job.error}`);
      }
      renderUploadPanel();
    }
  } finally {
    pumping = false;
  }
}

function removeUpload(id) {
  const before = state.uploads.length;
  state.uploads = state.uploads.filter((u) => u.id !== id);
  if (state.uploads.length === before) return;
  if (state.uploads.length === 0 && state.finishedCount) {
    toast(state.finishedCount === 1 ? "1 file uploaded" : `${state.finishedCount} files uploaded`);
    state.finishedCount = 0;
  }
  renderUploadPanel();
}

function cancelUpload(job) {
  if (job.status === "uploading") job.xhr?.abort();
  job.status = "cancelled";
  removeUpload(job.id);
}

function retryUpload(job) {
  Object.assign(job, { status: "queued", progress: 0, error: null });
  renderUploadPanel();
  pumpUploads();
}

// stopAllUploads is used on sign-out.
export function stopAllUploads() {
  for (const job of state.uploads) job.xhr?.abort();
  state.uploads = [];
  state.finishedCount = 0;
  renderUploadPanel();
}

// XMLHttpRequest rather than fetch: it reports upload progress and aborts.
function putFile(job) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    job.xhr = xhr;
    xhr.open("PUT", objectPath(job.bucket, job.key));
    xhr.setRequestHeader("Authorization", `Bearer ${session.key}`);
    xhr.setRequestHeader("Content-Type", job.file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => {
      if (!e.lengthComputable) return;
      job.progress = e.loaded / e.total;
      showProgress(job);
    };
    xhr.onload = () => {
      job.xhr = null;
      if (xhr.status >= 200 && xhr.status < 300) {
        state.finishedCount += 1;
        resolve();
        return;
      }
      let e = {};
      try { e = JSON.parse(xhr.responseText).error || {}; } catch { /* not JSON */ }
      reject(new ApiError(xhr.status, e.code || `HTTP${xhr.status}`, e.message || "Upload failed", e.requestId));
    };
    xhr.onerror = () => { job.xhr = null; reject(new Error("Network error. Check your connection.")); };
    xhr.onabort = () => { job.xhr = null; reject(new Error("Cancelled")); };
    xhr.send(job.file);
  });
}

// ---------- Panel ----------

export function renderUploadPanel() {
  const panel = document.getElementById("uploads");
  const jobs = state.uploads;
  if (jobs.length === 0) {
    panel.hidden = true;
    panel.replaceChildren();
    reserveDockSpace(0);
    return;
  }
  const active = jobs.filter((u) => u.status === "queued" || u.status === "uploading").length;
  const failed = jobs.filter((u) => u.status === "failed").length;
  const total = jobs.length;
  // Count finished files too, even after their rows fade away, so the
  // "n of m" never jumps backwards mid-batch.
  const finished = state.finishedCount;
  let title;
  if (active) title = `Uploading ${finished + 1} of ${finished + active}`;
  else if (failed) title = failed === 1 ? "1 upload failed" : `${failed} uploads failed`;
  else title = total === 1 ? "Upload complete" : `${total} uploads complete`;

  panel.hidden = false;
  panel.classList.toggle("collapsed", state.uploadsCollapsed);
  // Rebuilt only when a job changes state (progress updates in place, see
  // showProgress), and focus stays on the same button across rebuilds.
  const focus = rememberFocus(panel);
  panel.replaceChildren(
    h("header", {},
      h("strong", {}, title),
      iconButton("chevron", state.uploadsCollapsed ? "Expand uploads" : "Collapse uploads", () => {
        state.uploadsCollapsed = !state.uploadsCollapsed;
        renderUploadPanel();
      }, "flip"),
      !active && iconButton("close", "Close", () => { state.uploads = []; state.finishedCount = 0; renderUploadPanel(); })),
    !state.uploadsCollapsed && h("ul", {}, jobs.map(renderUploadRow)));
  focus.restore(panel);
  reserveDockSpace(panel.offsetHeight);
}

// reserveDockSpace adds room under the page content while the panel is
// open, so rows behind it can still be scrolled into reach and clicked.
function reserveDockSpace(px) {
  document.documentElement.style.setProperty("--dock-space", px ? `${px + 24}px` : "0px");
}

function pctText(job) {
  return job.progress >= 1 ? "Saving…" : `${Math.floor(job.progress * 100)}%`;
}

// showProgress updates one row's bar and percentage in place: rebuilding
// the panel on every tick would steal focus from its buttons.
function showProgress(job) {
  if (!job.ui?.bar?.isConnected) return; // collapsed, or not drawn yet
  job.ui.bar.value = job.progress;
  job.ui.pct.textContent = pctText(job);
}

function renderUploadRow(job) {
  job.ui = {};
  const where = job.bucket + "/" + dirOf(job.key);
  let status;
  switch (job.status) {
    case "queued":
      status = [h("span", { class: "muted" }, "Waiting"), iconButton("close", `Cancel ${baseName(job.key)}`, () => cancelUpload(job))];
      break;
    case "uploading":
      status = [
        job.ui.pct = h("span", { class: "muted pct" }, pctText(job)),
        iconButton("close", `Cancel ${baseName(job.key)}`, () => cancelUpload(job)),
      ];
      break;
    case "done":
      status = [h("span", { class: "ok", "aria-label": "Uploaded" }, icon("check"))];
      break;
    case "failed":
      status = [
        h("span", { class: "bad", title: job.error }, icon("alert")),
        iconButton("retry", `Retry ${baseName(job.key)}`, () => retryUpload(job)),
        iconButton("close", `Dismiss ${baseName(job.key)}`, () => removeUpload(job.id)),
      ];
      break;
  }
  const li = h("li", { class: `job ${job.status}` },
    h("div", { class: "job-text" },
      h("span", { class: "job-name", title: job.key }, baseName(job.key)),
      h("span", { class: "job-sub" }, job.status === "failed" ? job.error : `${formatSize(job.file.size)} · ${where}`)),
    h("div", { class: "job-status" }, status),
    job.status === "uploading" && (job.ui.bar = h("progress", { max: 1, value: job.progress, "aria-label": `Uploading ${baseName(job.key)}` })));
  if (job.status === "done") {
    // Rows are rebuilt when other jobs change; keep the fade-out anchored to
    // when this job finished rather than restarting it. (CSSOM writes are
    // allowed under the CSP; style attributes in markup are not.)
    li.style.animationDelay = `${1900 - (Date.now() - job.doneAt)}ms`;
  }
  return li;
}

// Closing the tab kills running uploads; ask first.
window.addEventListener("beforeunload", (e) => {
  if (state.uploads.some((u) => u.status === "uploading" || u.status === "queued")) {
    e.preventDefault();
    e.returnValue = ""; // older browsers need this to show the prompt
  }
});
