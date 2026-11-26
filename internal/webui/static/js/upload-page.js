// The page behind an upload link (/u/<token>): someone without an API key
// picks or drops files, which go one at a time to PUT /u/<token>/<name>.
// It can only add files; it never sees what else is stored.

import { announce, formatSize, h, icon, iconButton, relativeTime } from "./dom.js";
import { applyTheme } from "./theme.js";

applyTheme();

const token = location.pathname.split("/")[2] || "";
const base = `/u/${encodeURIComponent(token)}`;
const $ = (id) => document.getElementById(id);

let info = null; // from GET /u/<token>/info
const queue = [];
let busy = false;
let nextId = 1;

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
  renderOperator();
  renderLimits();
  $("drop").hidden = false;
  wire();
}

// renderOperator says who receives the files, when the server's operator
// has configured it (operator_name, privacy_url).
function renderOperator() {
  const op = info.operator;
  if (!op || (!op.name && !op.privacyUrl)) return;
  const parts = [];
  if (op.name) parts.push("Files you send go to ", h("strong", {}, op.name), ".");
  if (op.privacyUrl) {
    parts.push(parts.length ? " " : "", h("a", { href: op.privacyUrl, target: "_blank", rel: "noopener noreferrer" }, "How they handle your data"));
  }
  $("operator").replaceChildren(...parts);
  $("operator").hidden = false;
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
  // Closing the tab stops the upload in progress; ask first.
  window.addEventListener("beforeunload", (e) => {
    if (queue.some((j) => j.status === "queued" || j.status === "uploading")) {
      e.preventDefault();
      e.returnValue = ""; // older browsers need this to show the prompt
    }
  });
}

function add(files) {
  for (const file of files) {
    const job = { id: nextId++, file, status: "queued", progress: 0, message: "", xhr: null, ui: {} };
    if (file.size > info.maxFileSize) {
      Object.assign(job, { status: "failed", message: `Larger than the ${formatSize(info.maxFileSize)} limit` });
    }
    queue.push(job);
  }
  const n = files.length;
  announce(`${n} file${n === 1 ? "" : "s"} added`);
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
      job.message = `${formatSize(res.size)} sent`;
      if (info.filesLeft !== null) info.filesLeft--;
      announce(`${job.file.name} sent`);
    } catch (err) {
      if (job.status === "cancelled") continue;
      job.status = "failed";
      job.message = err.message;
      if (err.code === "UploadLimitReached") info.filesLeft = 0;
      announce(`${job.file.name} failed: ${err.message}`);
    }
    renderLimits();
    render();
  }
  busy = false;
}

// send uses XMLHttpRequest for upload progress and abort, which fetch lacks.
function send(job) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    job.xhr = xhr;
    xhr.open("PUT", `${base}/${encodeURIComponent(job.file.name)}`);
    xhr.setRequestHeader("Content-Type", job.file.type || "application/octet-stream");
    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) {
        job.progress = e.loaded / e.total;
        showProgress(job);
      }
    };
    xhr.onload = () => {
      job.xhr = null;
      let body = {};
      try { body = JSON.parse(xhr.responseText); } catch { /* not JSON */ }
      if (xhr.status >= 200 && xhr.status < 300) return resolve(body);
      const e = body.error || {};
      const err = new Error(friendly(xhr.status, e));
      err.code = e.code;
      reject(err);
    };
    xhr.onerror = () => { job.xhr = null; reject(new Error("Network error. Check your connection and try again.")); };
    xhr.onabort = () => { job.xhr = null; reject(new Error("Cancelled")); };
    xhr.send(job.file);
  });
}

function cancel(job) {
  if (job.status === "uploading") job.xhr?.abort();
  job.status = "cancelled";
  queue.splice(queue.indexOf(job), 1);
  announce(`${job.file.name} cancelled`);
  render();
}

function retry(job) {
  Object.assign(job, { status: "queued", progress: 0, message: "" });
  render();
  pump();
}

function friendly(status, e) {
  switch (e.code) {
    case "QuotaExceeded":
    case "InsufficientStorage": return "There's no room for this file. Let the person who sent the link know.";
    case "EntityTooLarge": return `Larger than the ${formatSize(info.maxFileSize)} limit`;
    case "UploadLimitReached": return "No uploads left on this link";
    case "LinkExpired": return "This link has expired";
    case "NoSuchLink": return "This link no longer works";
    case "InvalidRequest": return e.message || "This file name can't be used; rename the file and try again.";
    default: return e.message || `Upload failed (HTTP ${status})`;
  }
}

function pctText(job) {
  return job.progress >= 1 ? "Saving…" : `${Math.round(job.progress * 100)}%`;
}

// showProgress updates one row in place; rebuilding the list on every
// progress event would steal focus from its buttons.
function showProgress(job) {
  if (!job.ui.bar?.isConnected) return;
  job.ui.bar.value = job.progress;
  job.ui.pct.textContent = pctText(job);
}

// render rebuilds the list when a file changes state. Focus stays on the
// same button (matched by its label) across the rebuild.
function render() {
  const list = $("files");
  const focused = list.contains(document.activeElement) ? document.activeElement.getAttribute("aria-label") : null;
  list.replaceChildren(...queue.map(renderRow));
  if (focused) {
    const again = [...list.querySelectorAll("button")].find((b) => b.getAttribute("aria-label") === focused);
    (again || $("pick")).focus();
  }
}

function renderRow(job) {
  job.ui = {};
  const name = job.file.name;
  let sub;
  if (job.status === "queued") sub = `${formatSize(job.file.size)} · waiting`;
  else if (job.status === "uploading") sub = `${formatSize(job.file.size)} · sending`;
  else sub = job.message;
  return h("li", { class: `job ${job.status}` },
    h("div", { class: "job-text" },
      h("span", { class: "job-name", title: name }, name),
      h("span", { class: "job-sub" }, sub)),
    h("div", { class: "job-status" },
      job.status === "uploading" && (job.ui.pct = h("span", { class: "pct" }, pctText(job))),
      (job.status === "queued" || job.status === "uploading") && iconButton("close", `Cancel ${name}`, () => cancel(job)),
      job.status === "done" && h("span", { class: "ok", role: "img", "aria-label": "Sent" }, icon("check")),
      job.status === "failed" && h("span", { class: "bad", role: "img", "aria-label": "Failed" }, icon("alert")),
      job.status === "failed" && job.message !== "No uploads left on this link" &&
        iconButton("retry", `Retry ${name}`, () => retry(job))),
    job.status === "uploading" && (job.ui.bar = h("progress", { max: 1, value: job.progress, "aria-label": `Sending ${name}` })));
}

load();
