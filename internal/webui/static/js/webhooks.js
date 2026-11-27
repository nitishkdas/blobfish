// The "Webhooks" dialog (bucket admins): each webhook's address, events
// and delivery health, with test and delete, and a form for a new one.

import { api, showError } from "./api.js";
import { confirmDialog, copyText, h, iconButton, relativeTime, toast } from "./dom.js";

const EVENT_NAMES = { "object.created": "added", "object.deleted": "deleted", "object.restored": "restored" };

function health(w) {
  const waiting = w.pending ? ` · ${w.pending} waiting` : "";
  const lost = w.gaveUp || w.dropped ? ` · ${w.gaveUp + w.dropped} not delivered` : "";
  const ok = w.lastSuccess ? new Date(w.lastSuccess) : null;
  const bad = w.lastFailure ? new Date(w.lastFailure) : null;
  if (bad && (!ok || bad > ok)) {
    const retry = w.retryAfter ? `, retrying ${relativeTime(new Date(w.retryAfter))}` : "";
    return { failing: true, text: `Delivery failing. Last error: ${w.lastError}${retry}${waiting}${lost}` };
  }
  if (ok) return { failing: false, text: `Last delivered ${relativeTime(ok)}${waiting}${lost}` };
  return { failing: false, text: `Nothing sent yet${waiting}${lost}` };
}

export async function openWebhooks(bucket) {
  const dialog = document.getElementById("webhooks");
  const list = document.getElementById("webhooks-list");
  const secretBox = document.getElementById("webhooks-secret");
  document.getElementById("webhooks-title").textContent = `Webhooks for “${bucket}”`;
  secretBox.hidden = true;

  let hooks = [];
  const render = () => {
    if (hooks.length === 0) {
      list.replaceChildren(h("li", { class: "muted share-none" }, "No webhooks yet."));
      return;
    }
    list.replaceChildren(...hooks.map((w) => {
      const st = health(w);
      const field = h("input", { class: "mono", readonly: true, value: w.url, "aria-label": "Webhook address" });
      return h("li", { class: "share-link" },
        h("div", { class: "share-link-row" },
          field,
          iconButton("retry", "Send a test event", () => ping(w)),
          iconButton("trash", "Delete webhook", () => remove(w), "danger")),
        h("span", { class: "share-meta" },
          `On ${w.events.map((e) => EVENT_NAMES[e] || e).join(", ")}`,
          w.allowPrivate && " · may reach private addresses",
          ` · added ${relativeTime(new Date(w.created))}`,
          w.createdByName && ` by ${w.createdByName}`),
        h("span", { class: st.failing ? "share-meta failing" : "share-meta" }, st.text));
    }));
  };
  const load = async () => {
    try {
      hooks = (await api("GET", `/v1/webhooks?bucket=${encodeURIComponent(bucket)}`)).webhooks;
    } catch (err) { showError(err); hooks = []; }
    render();
  };
  const ping = async (w) => {
    try {
      await api("POST", `/v1/webhooks/${encodeURIComponent(w.id)}/test`);
      toast("Test event queued", { detail: "It will be delivered within a few seconds." });
      setTimeout(load, 3000);
    } catch (err) { showError(err); }
  };
  const remove = async (w) => {
    const ok = await confirmDialog({
      title: "Delete this webhook?",
      body: [h("p", { class: "mono confirm-target" }, w.url), "Events still waiting for it are dropped."],
      confirmLabel: "Delete webhook",
    });
    if (!ok) return;
    try {
      await api("DELETE", `/v1/webhooks/${encodeURIComponent(w.id)}`);
      toast("Webhook deleted");
    } catch (err) { showError(err); }
    await load();
  };

  const urlEl = document.getElementById("webhooks-url");
  urlEl.value = "";
  const create = document.getElementById("webhooks-create");
  create.onclick = async () => {
    const events = [...dialog.querySelectorAll(".webhook-events input:checked")].map((c) => c.value);
    if (!urlEl.value.trim()) { urlEl.focus(); return; }
    if (events.length === 0) { toast("Choose at least one event.", { error: true }); return; }
    create.disabled = true;
    try {
      const w = await api("POST", "/v1/webhooks", { body: { bucket, url: urlEl.value.trim(), events } });
      urlEl.value = "";
      const secret = document.getElementById("webhooks-secret-value");
      secret.value = w.secret;
      document.getElementById("webhooks-secret-copy").onclick = () => copyText(w.secret, "Secret copied", secret);
      secretBox.hidden = false;
      await load();
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
