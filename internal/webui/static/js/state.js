// Shared UI state, permission checks and re-render hooks.

export const PAGE_SIZES = [50, 100, 200];

export const state = {
  // Who is signed in, from GET /v1/whoami.
  me: { name: "", scopes: [], maxObjectSize: 0, trashDays: 0 },
  buckets: [],

  // Current route (see router.js).
  route: { view: "home", bucket: null, prefix: "", page: 1 },

  // The file listing on screen. cursors[i] fetches page i+1, so Prev is
  // instant and Next appends the server's nextCursor.
  listing: {
    bucket: null, prefix: "", size: 100,
    cursors: [""], page: 1,
    folders: [], files: [], nextCursor: "",
    loaded: false,
  },

  apiKeys: [],

  // The trash on screen (trash.js), newest first.
  trash: { bucket: null, items: [], nextCursor: "", loaded: false },

  // The Settings page's lists (settings.js); null while loading.
  settings: { bucket: null, uploadLinks: null, webhooks: null },

  // Upload queue, shown in the docked panel. Jobs run one at a time and
  // remember their own bucket, so browsing elsewhere doesn't disturb them.
  uploads: [],
  uploadsCollapsed: false,
  finishedCount: 0,
};

// ---------- Permissions ----------
// Mirrors auth.Principal.Can on the server: a scope "bucket:perm" grants
// perm on that bucket, "*:perm" on every bucket, and server-wide actions
// use bucket "*", which only "*:" scopes match. The UI uses this only to
// hide buttons; the server still checks every request.

const PERM_BITS = { r: 1, w: 2, rw: 3, admin: 7 };

export function can(perm, bucket) {
  const need = PERM_BITS[perm];
  return state.me.scopes.some((s) => {
    const i = s.lastIndexOf(":");
    const b = s.slice(0, i);
    const have = PERM_BITS[s.slice(i + 1)] || 0;
    return (have & need) === need && (b === "*" || b === bucket);
  });
}

export const isServerAdmin = () => can("admin", "*");

export const bucketInfo = (name) => state.buckets.find((b) => b.name === name);

// ---------- Re-rendering ----------
// Views register their renderers here so modules can ask for a redraw
// without importing each other.

export const ui = {
  renderApp: () => {},
  renderMain: () => {},
  reloadFiles: () => {},
};

export function rerenderMain() {
  ui.renderMain();
}
