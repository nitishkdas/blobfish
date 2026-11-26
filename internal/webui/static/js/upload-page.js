// The page behind an upload link (/u/<token>): someone without an API key
// picks or drops files, which go one at a time to PUT /u/<token>/<name>.
// It can only add files; it never sees what else is stored.

import { formatSize, h, icon, relativeTime } from "./dom.js";

const token = location.pathname.split("/")[2] || "";
const base = `/u/${encodeURIComponent(token)}`;
const $ = (id) => document.getElementById(id);

let info = null; // from GET /u/<token>/info
const queue = [];
let busy = false;

async function load() {
  try {
    const res = await fetch(`${base}/info`);
    const body = await res.json().catch(() => ({}));
    if (!res.ok) {
      showClosed(res.status === 410
        ? "This upload link has expired. Ask the person who sent it for a new one."
        : "This upload link doesn't work. It may have been revoked or mistyped.");
      return;
    }
    info = body;
  } catch {
    showClosed("Can't reach the server. Check your connection and reload the page.");
    return;
  }
  if (info.label) {
    $("title").textContent = info.label;
    document.title = `${info.label} · Upload files`;
  }
  renderLimits();
  $("drop").hidden = false;
  wire();
}

function showClosed(message) {
  $("limits").textContent = message;
  $("limits").className = "error";
  $("drop").hidden = true;
}

function renderLimits() {
  const parts = [`Up to ${formatSize(info.maxFileSize)} per file`];
  if (info.filesLeft !== null) parts.push(`${info.filesLeft} file${info.filesLeft === 1 ? "" : "s"} left`);
  if (info.expires) parts.push(`link expires ${relativeTime(new Date(info.expires))}`);
  $("limits").textContent = parts.join(" · ");
  if (info.filesLeft === 0) showClosed("This link has taken all the files it accepts. Thank you!");
}

function wire() {
  $("pick").addEventListener("change", (e) => {
    add([...e.target.files]);
    e.target.value = "";
  });
  const zone = $("drop");
  zone.addEventListener("dragover", (e) => { e.preventDefault(); zone.classList.add("over"); });
  zone.addEventListener("dragleave", () => zone.classList.remove("over"));
  zone.addEventListener("drop", (e) => {
    e.preventDefault();
    zone.classList.remove("over");
    add([...e.dataTransfer.files]);
  });
}

function add(files) {
  for (const file of files) {
    const job = { file, status: "queued", progress: 0, message: "" };
    if (file.size > info.maxFileSize) {
      Object.assign(job, { status: "failed", message: `Larger than the ${formatSize(info.maxFileSize)} limit` });
    }
    queue.push(job);
  }
  render();
  pump();
}

async function pump() {
  if (busy) return;
  busy = true;
  let job;
  while ((job = queue.find((j) => j.status === "queued"))) {
    if (info.filesLeft === 0) {
      Object.assign(job, { status: "failed", message: "No uploads left on this link" });
      continue;
    }
    job.status = "uploading";
    render();
    try {
      const res = await send(job);
      job.status = "done";
      job.message = res.name !== job.file.name ? `Saved as “${res.name}”` : `${formatSize(res.size)} sent`;
      if (info.filesLeft !== null) info.filesLeft--;
    } catch (err) {
      job.status = "failed";
      job.message = err.message;
      if (err.code === "UploadLimitReached") info.filesLeft = 0;
    }
    renderLimits();
    render();
  }
  busy = false;
}

// send uses XMLHttpRequest for upload progress, which fetch lacks.
function send(job) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open("PUT", `${base}/${encodeURIComponent(job.file.name)}`);
    xhr.setRequestHeader("Content-Type", job.file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        job.progress = e.loaded / e.total;
        render();
      }
    };
    xhr.onload = () => {
      let body = {};
      try { body = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      if (xhr.status >= 200 && xhr.status < 300) return resolve(body);
      const e = body.error || {};
      const err = new Error(friendly(xhr.status, e));
      err.code = e.code;
      reject(err);
    };
    xhr.onerror = () => reject(new Error("Network error. Check your connection and try again."));
    xhr.send(job.file);
  });
}

function friendly(status, e) {
  switch (e.code) {
    case "QuotaExceeded":
    case "InsufficientStorage": return "There's no room for this file. Let the person who sent the link know.";
    case "EntityTooLarge": return `Larger than the ${formatSize(info.maxFileSize)} limit`;
    case "UploadLimitReached": return "No uploads left on this link";
    case "LinkExpired": return "This link has expired";
    case "NoSuchLink": return "This link was revoked";
    case "InvalidRequest": return e.message || "This file name can't be used; rename the file and try again.";
    default: return e.message || `Upload failed (HTTP ${status})`;
  }
}

function render() {
  $("files").replaceChildren(...queue.map((job) => h("li", { class: `job ${job.status}` },
    h("div", { class: "job-text" },
      h("span", { class: "job-name", title: job.file.name }, job.file.name),
      h("span", { class: "job-sub" },
        job.status === "queued" ? `${formatSize(job.file.size)} · waiting`
          : job.status === "uploading" ? `${formatSize(job.file.size)} · sending`
            : job.message)),
    h("div", { class: "job-status" },
      job.status === "uploading" && h("span", { class: "pct" }, `${Math.round(job.progress * 100)}%`),
      job.status === "done" && h("span", { class: "ok", title: "Sent" }, icon("check")),
      job.status === "failed" && h("span", { class: "bad", title: job.message }, icon("alert"))),
    job.status === "uploading" && h("progress", { max: 1, value: job.progress }))));
}

load();
