// Talking to the /v1 API, and keeping the API key for this browser.

import { toast } from "./dom.js";

const KEY_STORE = "blobfish.key";

// session.key is the key in use. onUnauthorized runs when the server
// rejects it (revoked, or the data dir was reset).
export const session = { key: null, onUnauthorized: () => {} };

// Messages for codes where the UI can say more than the API's own message
// (which is written for any client). Everything else shows the server's
// message, which is a full sentence.
const MESSAGES = {
  AccessDenied: "You don't have permission to do this.",
  Unauthorized: "Your session has ended. Sign in again.",
  NoSuchBucket: "This bucket doesn't exist or was deleted.",
  NoSuchKey: "This file doesn't exist or was deleted.",
  QuotaExceeded: "This bucket is full: the upload would exceed its quota.",
  EntityTooLarge: "The file is larger than this server allows.",
  CommitUncertain: "The upload may or may not have been saved. Reload the list to check before trying again.",
  IncompleteBody: "The upload was interrupted. Nothing was saved.",
  RequestTimeout: "The upload stalled and was stopped. Try again.",
  InternalError: "Something went wrong on the server. Try again, or give your administrator the request ID.",
  DataCorrupt: "This file failed its integrity check. Ask your administrator to run blobfish verify.",
  NetworkError: "Can't reach the server. Check your connection and try again.",
};

export class ApiError extends Error {
  constructor(status, code, message, requestId) {
    super(MESSAGES[code] || message);
    this.status = status;
    this.code = code;
    this.serverMessage = message;
    this.requestId = requestId;
  }
}

async function parseError(res) {
  let e = {};
  try { e = (await res.json()).error || {}; } catch { /* not JSON */ }
  return new ApiError(res.status, e.code || `HTTP${res.status}`, e.message || res.statusText, e.requestId);
}

export async function api(method, path, { body, key = session.key } = {}) {
  const headers = {};
  if (key) headers.Authorization = `Bearer ${key}`; // none for /v1/login
  if (body !== undefined) headers["Content-Type"] = "application/json";
  let res;
  try {
    res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  } catch {
    throw new ApiError(0, "NetworkError", "Network error");
  }
  if (!res.ok) {
    const err = await parseError(res);
    // Only a rejected key/session signs out: a wrong password (when
    // changing it) is 401 InvalidCredentials and must not.
    if (res.status === 401 && err.code === "Unauthorized" && key && key === session.key) session.onUnauthorized();
    throw err;
  }
  // 204, or any answer without a body, has nothing to parse.
  const text = res.status === 204 ? "" : await res.text();
  return text ? JSON.parse(text) : null;
}

// objectExists reports true, false, or null when it can't tell (for
// example a write-only key may not read).
export async function objectExists(bucket, key) {
  try {
    const res = await fetch(objectPath(bucket, key), { method: "HEAD", headers: { Authorization: `Bearer ${session.key}` } });
    if (res.status === 200) return true;
    if (res.status === 404) return false;
    return null;
  } catch {
    return null;
  }
}

// Object keys may contain any character, so each path segment is encoded.
// Browsers normalize "." and ".." segments before sending, so keys with
// such segments can only be reached through the API with a raw client.
export function objectPath(bucket, key) {
  return `/v1/b/${encodeURIComponent(bucket)}/${key.split("/").map(encodeURIComponent).join("/")}`;
}

export function bucketPath(bucket) {
  return `/v1/b/${encodeURIComponent(bucket)}`;
}

export function presign(bucket, key, method, expiresIn) {
  return api("POST", "/v1/presign", { body: { bucket, key, method, expiresIn } });
}

export function showError(err) {
  if (err instanceof ApiError) {
    toast(err.message, { error: true, detail: err.requestId ? `Request ID ${err.requestId}` : "" });
  } else {
    toast(String(err.message || err), { error: true });
  }
}

// ---------- Key storage ----------
// Storage can throw (private mode, blocked site data); the UI still works,
// the user just signs in again next time.

export function loadKey() {
  try { return sessionStorage.getItem(KEY_STORE) || localStorage.getItem(KEY_STORE); } catch { return null; }
}
export function saveKey(key, remember) {
  try {
    sessionStorage.setItem(KEY_STORE, key);
    if (remember) localStorage.setItem(KEY_STORE, key);
  } catch { /* in-memory only */ }
}
export function forgetKey() {
  try { sessionStorage.removeItem(KEY_STORE); localStorage.removeItem(KEY_STORE); } catch { /* ignore */ }
}

// Small per-browser preferences (not secrets).
export function loadPref(name, fallback) {
  try { return localStorage.getItem(`blobfish.pref.${name}`) ?? fallback; } catch { return fallback; }
}
export function savePref(name, value) {
  try { localStorage.setItem(`blobfish.pref.${name}`, value); } catch { /* ignore */ }
}
