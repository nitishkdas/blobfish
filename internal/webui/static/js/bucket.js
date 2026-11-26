// The header shared by a bucket's pages: its name, visibility, usage and
// the Files · Trash · Settings tabs. Tabs a key can't use are not shown.

import { formatInt, formatSize, h, icon } from "./dom.js";
import { formatHash } from "./router.js";
import { bucketInfo, can } from "./state.js";

export function renderBucketHeader(bucket, active) {
  const b = bucketInfo(bucket);
  const tabs = [
    { view: "files", label: "Files", icon: "folder", show: can("r", bucket) },
    { view: "trash", label: "Trash", icon: "trash", show: can("w", bucket) },
    { view: "settings", label: "Settings", icon: "sliders", show: can("w", bucket) },
  ].filter((t) => t.show);
  return h("header", { class: "bucket-head" },
    h("div", { class: "bucket-title" },
      h("span", { class: "bucket-icon", "aria-hidden": "true" }, icon("database", 20)),
      h("div", { class: "bucket-heading" },
        h("div", { class: "bucket-name-row" },
          h("h1", {}, bucket),
          visibilityBadge(b)),
        h("p", { class: "bucket-sub" }, usageText(b))),
      quotaMeter(b)),
    tabs.length > 1 && h("nav", { class: "tabs", "aria-label": `${bucket} sections` },
      tabs.map((t) => h("a", {
        href: formatHash({ view: t.view, bucket, prefix: "", page: 1 }),
        "aria-current": t.view === active ? "page" : null,
      }, icon(t.icon, 16), t.label))));
}

export function visibilityBadge(b) {
  if (!b) return null;
  return b.public
    ? h("span", { class: "badge badge-accent public-chip", title: "Files in this bucket can be read without a key" }, icon("globe", 13), "Public")
    : h("span", { class: "badge", title: "Every request needs a key or a link" }, icon("lock", 13), "Private");
}

// usageText is what the bucket's files take. Trash is not counted,
// matching the server.
function usageText(b) {
  if (!b) return "";
  const files = `${formatInt(b.objects)} file${b.objects === 1 ? "" : "s"}`;
  return `${formatSize(b.usedBytes)} · ${files}${b.quotaBytes ? "" : " · no quota"}`;
}

// quotaMeter shows use against the quota, when the bucket has one.
function quotaMeter(b) {
  if (!b?.quotaBytes) return null;
  const pct = Math.min(100, (b.usedBytes / b.quotaBytes) * 100);
  return h("div", {
    class: pct >= 90 ? "quota-meter near-full" : "quota-meter",
    title: "Uploads that don't fit are refused. Trash is not counted.",
  },
  h("div", { class: "quota-meter-text" },
    h("span", {}, `${Math.round(pct)}% of quota`),
    h("span", { class: "muted" }, formatSize(b.quotaBytes))),
  meterBar(pct, `${Math.round(pct)}% of the quota used`));
}

// meterBar sets its width through CSSOM: the CSP forbids style attributes.
export function meterBar(pct, label) {
  const fill = h("span", {});
  fill.style.width = `${Math.max(pct, pct > 0 ? 1 : 0)}%`;
  return h("div", { class: pct >= 90 ? "usage-bar near-full" : "usage-bar", role: "img", "aria-label": label }, fill);
}
