package server

import (
	"cmp"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// maxLinkLifetime caps an expiring share link; 0 means "never expires".
const maxLinkLifetime = 10 * 365 * 24 * time.Hour

type linkJSON struct {
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	Bucket        string     `json:"bucket"`
	Key           string     `json:"key"`
	Created       time.Time  `json:"created"`
	Expires       *time.Time `json:"expires"` // null = never
	CreatedBy     string     `json:"createdBy"`
	CreatedByName string     `json:"createdByName"`
	// Filename is the name the file is served and saved under.
	Filename string `json:"filename"`
	// Active is false when the link's creator was deleted or can no
	// longer read the file: the link then answers 404 until access returns.
	Active bool `json:"active"`
}

func (a *api) toLinkJSON(r *http.Request, l store.LinkRecord) (linkJSON, error) {
	active, err := a.auth.CreatorCan(r.Context(), l.CreatedBy, auth.PermRead, l.Bucket, l.Key)
	if err != nil {
		return linkJSON{}, err
	}
	name := linkFilename(l)
	out := linkJSON{
		ID: l.ID, Bucket: l.Bucket, Key: l.Key, Created: l.Created,
		CreatedBy: l.CreatedBy, CreatedByName: l.CreatedByName, Filename: name, Active: active,
		// The trailing file name is cosmetic: it makes links readable and
		// gives downloads a sensible default name.
		URL: a.baseURL(r) + "/s/" + l.Token + "/" + url.PathEscape(name),
	}
	if !l.Expires.IsZero() {
		exp := l.Expires
		out.Expires = &exp
	}
	return out, nil
}

// createLink makes a share link to one object. Anyone who can read the
// object may share it, the same rule as for presigned GET URLs.
func (a *api) createLink(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct {
		Bucket    string `json:"bucket"`
		Key       string `json:"key"`
		ExpiresIn int64  `json:"expiresIn"` // seconds; 0 = never expires
		// Filename, if set, replaces the last part of the key as the name
		// the file is shown and saved under.
		Filename string `json:"filename"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if !p.Can(auth.PermRead, req.Bucket, req.Key) {
		a.fail(w, r, errAccessDenied)
		return
	}
	if err := store.ValidateBucketName(req.Bucket); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := store.ValidateKey(req.Key); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.Filename != "" {
		if err := validateFilename(req.Filename); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	lifetime := time.Duration(req.ExpiresIn) * time.Second
	if req.ExpiresIn < 0 || lifetime > maxLinkLifetime {
		a.fail(w, r, badRequest(`"expiresIn" must be 0 (never) or up to %d seconds`, int64(maxLinkLifetime/time.Second)))
		return
	}

	var tok [16]byte
	rand.Read(tok[:])
	var id [8]byte
	rand.Read(id[:])
	now := time.Now().UTC()
	rec := store.LinkRecord{
		ID: hex.EncodeToString(id[:]), Token: base64.RawURLEncoding.EncodeToString(tok[:]),
		Bucket: req.Bucket, Key: req.Key, Created: now,
		CreatedBy: p.KeyID, CreatedByName: p.KeyName, Filename: req.Filename,
	}
	if lifetime > 0 {
		rec.Expires = now.Add(lifetime).Truncate(time.Second)
	}
	if err := a.links.CreateLink(r.Context(), rec); err != nil {
		a.fail(w, r, err)
		return
	}
	out, err := a.toLinkJSON(r, rec)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

// listLinks lists links for one object or one bucket (read access needed),
// or for everything (server admins only).
func (a *api) listLinks(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	bucket, key := q.Get("bucket"), q.Get("key")
	t := auth.Target{Bucket: bucket}
	perm := auth.PermRead
	if bucket == "" {
		t, perm = serverWide, auth.PermAdmin
	}
	if _, ok := a.authorize(w, r, t, perm); !ok {
		return
	}
	recs, err := a.links.ListLinks(r.Context(), bucket, key)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]linkJSON, 0, len(recs))
	for _, l := range recs {
		j, err := a.toLinkJSON(r, l)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string][]linkJSON{"links": out})
}

// deleteLink revokes a link. Allowed for the key that created it, or any
// key that can write the bucket.
func (a *api) deleteLink(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rec, err := a.links.LinkByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if rec.CreatedBy != p.KeyID && !p.Can(auth.PermWrite, rec.Bucket, "") {
		a.fail(w, r, errAccessDenied)
		return
	}
	if err := a.links.DeleteLink(r.Context(), rec.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveLink is the public side of a share link: no key, just the token.
func (a *api) serveLink(w http.ResponseWriter, r *http.Request) {
	rec, err := a.links.LinkByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if a.cors(w, r, rec.Bucket) {
		return
	}
	setActor(r, "link:"+rec.ID)
	if !rec.Expires.IsZero() && time.Now().After(rec.Expires) {
		a.fail(w, r, fmt.Errorf("%w on %s", errLinkExpired, rec.Expires.Format(time.RFC3339)))
		return
	}
	// A link acts for whoever made it: it works only while they could
	// still read the file themselves. Otherwise it looks revoked.
	if ok, err := a.auth.CreatorCan(r.Context(), rec.CreatedBy, auth.PermRead, rec.Bucket, rec.Key); err != nil || !ok {
		a.fail(w, r, cmp.Or(err, fmt.Errorf("%w: its creator no longer has access", store.ErrNoSuchLink)))
		return
	}
	disposition, err := contentDisposition(r, linkFilename(rec))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.serveObject(w, r, auth.Target{Bucket: rec.Bucket, Key: rec.Key}, func(h http.Header) {
		h.Set("Content-Disposition", disposition)
		// Revalidate every time (cheap, via ETag) so a revoked link stops
		// working at once; never stored by shared caches.
		h.Set("Cache-Control", "private, no-cache")
		h.Set("X-Robots-Tag", "noindex")
		h.Set("Referrer-Policy", "no-referrer")
	})
}

// linkFilename is the name a link serves its file under: the one chosen
// when the link was made, else the last part of the key.
func linkFilename(l store.LinkRecord) string {
	if l.Filename != "" {
		return l.Filename
	}
	return defaultFilename(l.Key)
}

// updateBucket changes bucket settings: visibility and browser access
// (bucket admin), and the quota (server admin only: a bucket admin who
// could raise their own quota would make it meaningless).
func (a *api) updateBucket(w http.ResponseWriter, r *http.Request) {
	bucket := r.PathValue("bucket")
	p, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermAdmin)
	if !ok {
		return
	}
	var req struct {
		Public      *bool     `json:"public"`
		CORSOrigins *[]string `json:"corsOrigins"`
		QuotaBytes  *int64    `json:"quotaBytes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.Public == nil && req.CORSOrigins == nil && req.QuotaBytes == nil {
		a.fail(w, r, badRequest(`nothing to change: send {"public": true|false}, {"corsOrigins": [...]} and/or {"quotaBytes": n}`))
		return
	}
	if req.QuotaBytes != nil && !p.Can(auth.PermAdmin, serverWide.Bucket, "") {
		a.fail(w, r, errAccessDenied)
		return
	}
	var info store.BucketInfo
	var err error
	if req.QuotaBytes != nil {
		if q := *req.QuotaBytes; q > 0 && q < a.minQuota {
			a.fail(w, r, fmt.Errorf("%w: the smallest quota is %d MB (%d bytes); use 0 for no quota",
				store.ErrInvalidQuota, a.minQuota>>20, a.minQuota))
			return
		}
		if info, err = a.buckets.SetBucketQuota(r.Context(), bucket, *req.QuotaBytes); err != nil {
			a.fail(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "bucket quota changed",
			"request_id", RequestID(r.Context()), "bucket", bucket, "quota_bytes", info.QuotaBytes)
	}
	if req.CORSOrigins != nil {
		if info, err = a.buckets.SetBucketCORS(r.Context(), bucket, *req.CORSOrigins); err != nil {
			a.fail(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "bucket browser access changed",
			"request_id", RequestID(r.Context()), "bucket", bucket, "origins", info.CORSOrigins)
	}
	if req.Public != nil {
		if info, err = a.buckets.SetBucketPublic(r.Context(), bucket, *req.Public); err != nil {
			a.fail(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "bucket visibility changed",
			"request_id", RequestID(r.Context()), "bucket", bucket, "public", info.Public)
	}
	writeJSON(w, http.StatusOK, toBucketJSON(info))
}
