// The header shared by a bucket's pages: its name, status chips and the
// Files · Trash · Settings tabs. Tabs a key can't use are not shown.

import { formatInt, formatSize, h, icon } from "./dom.js";
import { formatHash } from "./router.js";
import { bucketInfo, can } from "./state.js";

export function renderBucketHeader(bucket, active) {
  const b = bucketInfo(bucket);
  const tabs = [
    { view: "files", label: "Files", show: can("r", bucket) },
    { view: "trash", label: "Trash", show: can("w", bucket) },
    { view: "settings", label: "Settings", show: can("w", bucket) },
  ].filter((t) => t.show);
  return h("header", { class: "bucket-head" },
    h("div", { class: "bucket-title" },
      h("h1", {}, bucket),
      b?.public && h("span", { class: "chip public-chip", title: "Files in this bucket can be read without a key" },
        icon("globe", 14), "Public"),
      h("span", { class: "spacer" }),
      usageChip(b)),
    tabs.length > 1 && h("nav", { class: "tabs", "aria-label": `${bucket} sections` },
      tabs.map((t) => h("a", {
        href: formatHash({ view: t.view, bucket, prefix: "", page: 1 }),
        "aria-current": t.view === active ? "page" : null,
      }, t.label))));
}

// usageChip shows what the bucket's files take, against its quota if it
// has one. Trash is not counted, matching the server.
export function usageChip(b) {
  if (!b) return null;
  const files = `${formatInt(b.objects)} file${b.objects === 1 ? "" : "s"}`;
  if (!b.quotaBytes) {
    return h("span", { class: "chip usage-chip", title: "Space used by this bucket's files (trash not counted)" },
      `${formatSize(b.usedBytes)} · ${files}`);
  }
  const pct = Math.min(100, Math.round((b.usedBytes / b.quotaBytes) * 100));
  return h("span", {
    class: pct >= 90 ? "chip usage-chip near-full" : "chip usage-chip",
    title: `${files}, ${pct}% of the quota (trash not counted). Uploads that don't fit are refused.`,
  }, `${formatSize(b.usedBytes)} of ${formatSize(b.quotaBytes)}`);
}
