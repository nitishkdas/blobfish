package server

import (
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
	"github.com/nitishkdas/blobfish/internal/webhook"
)

// Webhooks are set per bucket by bucket admins. The signing secret is
// shown once, in the create response. Webhooks made by server admins may
// target private addresses (an app on the same machine); others only
// public ones, enforced again at connect time by the dispatcher.

const maxWebhookURL = 2048

type webhookJSON struct {
	ID            string     `json:"id"`
	Bucket        string     `json:"bucket"`
	URL           string     `json:"url"`
	Events        []string   `json:"events"`
	AllowPrivate  bool       `json:"allowPrivate"`
	Secret        string     `json:"secret,omitempty"` // only when just created
	Created       time.Time  `json:"created"`
	CreatedBy     string     `json:"createdBy"`
	CreatedByName string     `json:"createdByName"`
	Pending       int        `json:"pending"`
	Dropped       int        `json:"dropped"`
	GaveUp        int        `json:"gaveUp"`
	LastSuccess   *time.Time `json:"lastSuccess"`
	LastFailure   *time.Time `json:"lastFailure"`
	LastError     string     `json:"lastError"`
	RetryAfter    *time.Time `json:"retryAfter"`
}

func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

func toWebhookJSON(w store.WebhookRecord) webhookJSON {
	return webhookJSON{
		ID: w.ID, Bucket: w.Bucket, URL: w.URL, Events: w.Events, AllowPrivate: w.AllowPrivate,
		Created: w.Created, CreatedBy: w.CreatedBy, CreatedByName: w.CreatedByName,
		Pending: w.Pending, Dropped: w.Dropped, GaveUp: w.GaveUp,
		LastSuccess: timePtr(w.LastSuccess), LastFailure: timePtr(w.LastFailure), LastError: w.LastError,
		RetryAfter: timePtr(w.RetryAfter),
	}
}

// checkWebhookURL accepts absolute http(s) URLs without credentials. For
// webhooks that may only reach public addresses it also refuses hosts that
// are obviously not public; hostnames are checked again after DNS.
func checkWebhookURL(raw string, allowPrivate bool) error {
	u, err := url.Parse(raw)
	switch {
	case len(raw) > maxWebhookURL:
		return badRequest("url is longer than %d bytes", maxWebhookURL)
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "":
		return badRequest("url must be an http:// or https:// address without user name or password, e.g. https://app.example.com/hooks/blobfish")
	}
	if allowPrivate {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return badRequest("only server admins may send webhooks to this machine (%s)", host)
	}
	if ip, err := netip.ParseAddr(host); err == nil && !webhook.IsPublic(ip) {
		return badRequest("only server admins may send webhooks to private or local addresses (%s)", host)
	}
	return nil
}

func (a *api) createWebhook(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Bucket string   `json:"bucket"`
		URL    string   `json:"url"`
		Events []string `json:"events"` // empty = all
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	p, ok := a.authorize(w, r, auth.Target{Bucket: req.Bucket}, auth.PermAdmin)
	if !ok {
		return
	}
	if err := store.ValidateBucketName(req.Bucket); err != nil {
		a.fail(w, r, err)
		return
	}
	events := store.EventTypes
	if len(req.Events) > 0 {
		events = nil
		for _, ev := range req.Events {
			if !slices.Contains(store.EventTypes, ev) {
				a.fail(w, r, badRequest("unknown event %q; known: %s", ev, strings.Join(store.EventTypes, ", ")))
				return
			}
			if !slices.Contains(events, ev) {
				events = append(events, ev)
			}
		}
	}
	allowPrivate := p.Can(auth.PermAdmin, serverWide.Bucket, "")
	if err := checkWebhookURL(req.URL, allowPrivate); err != nil {
		a.fail(w, r, err)
		return
	}
	var id [8]byte
	rand.Read(id[:])
	var secret [32]byte
	rand.Read(secret[:])
	rec := store.WebhookRecord{
		ID: hex.EncodeToString(id[:]), Bucket: req.Bucket, URL: req.URL, Events: events,
		Secret: "whsec_" + hex.EncodeToString(secret[:]), AllowPrivate: allowPrivate,
		Created: time.Now().UTC(), CreatedBy: p.KeyID, CreatedByName: p.KeyName,
	}
	if err := a.webhooks.CreateWebhook(r.Context(), rec); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "webhook created",
		"request_id", RequestID(r.Context()), "bucket", rec.Bucket, "webhook", rec.ID, "host", webhookHost(rec.URL))
	out := toWebhookJSON(rec)
	out.Secret = rec.Secret
	writeJSON(w, http.StatusCreated, out)
}

func (a *api) listWebhooks(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermAdmin); !ok {
		return
	}
	recs, err := a.webhooks.ListWebhooks(r.Context(), bucket)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]webhookJSON, 0, len(recs))
	for _, rec := range recs {
		out = append(out, toWebhookJSON(rec))
	}
	writeJSON(w, http.StatusOK, map[string][]webhookJSON{"webhooks": out})
}

// webhookForAdmin loads the webhook in the path and checks the caller
// administers its bucket. A 404 for others would reveal less, but the
// IDs are random and webhooks belong to buckets the caller can name.
func (a *api) webhookForAdmin(w http.ResponseWriter, r *http.Request) (store.WebhookRecord, bool) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return store.WebhookRecord{}, false
	}
	rec, err := a.webhooks.WebhookByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return rec, false
	}
	if !p.Can(auth.PermAdmin, rec.Bucket, "") {
		a.fail(w, r, errAccessDenied)
		return rec, false
	}
	return rec, true
}

func (a *api) deleteWebhook(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.webhookForAdmin(w, r)
	if !ok {
		return
	}
	if err := a.webhooks.DeleteWebhook(r.Context(), rec.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "webhook deleted",
		"request_id", RequestID(r.Context()), "bucket", rec.Bucket, "webhook", rec.ID)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) testWebhook(w http.ResponseWriter, r *http.Request) {
	rec, ok := a.webhookForAdmin(w, r)
	if !ok {
		return
	}
	if err := a.webhooks.QueueTestEvent(r.Context(), rec.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "queued"})
}

// webhookHost is what the log may say about a webhook's address: its
// scheme and host. The path and query often carry a secret (Slack, Discord
// and many CI hooks put a token there).
func webhookHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(unparseable)"
	}
	return u.Scheme + "://" + u.Host
}
