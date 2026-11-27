package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Upload links let someone without a key add files to one folder of a
// bucket through a small web page at /u/<token>. They can't read, list,
// replace or delete anything: a name that is taken is saved as
// "name (2).ext". The bucket quota applies as usual.

const (
	maxUploadLinkLabel = 200
	maxUploadLinkFiles = 1_000_000
	// maxNameTries bounds the "name (n).ext" search for a free name.
	maxNameTries = 100
)

type uploadLinkJSON struct {
	ID            string     `json:"id"`
	URL           string     `json:"url"`
	Bucket        string     `json:"bucket"`
	Prefix        string     `json:"prefix"`
	Label         string     `json:"label"`
	Expires       *time.Time `json:"expires"` // null = never
	MaxFiles      int        `json:"maxFiles"`
	Uploads       int        `json:"uploads"`
	MaxFileSize   int64      `json:"maxFileSize"` // 0 = the server's limit
	Created       time.Time  `json:"created"`
	CreatedBy     string     `json:"createdBy"`
	CreatedByName string     `json:"createdByName"`
	// Active is false when the link's creator was deleted or can no
	// longer write the bucket: the link then refuses uploads.
	Active bool `json:"active"`
}

// operatorJSON says who runs this server, for upload-link visitors.
type operatorJSON struct {
	Name       string `json:"name,omitempty"`
	PrivacyURL string `json:"privacyUrl,omitempty"`
}

func (a *api) toUploadLinkJSON(r *http.Request, l store.UploadLinkRecord) (uploadLinkJSON, error) {
	active, err := a.auth.CreatorCan(r.Context(), l.CreatedBy, auth.PermWrite, l.Bucket, "")
	if err != nil {
		return uploadLinkJSON{}, err
	}
	out := uploadLinkJSON{
		ID: l.ID, URL: a.baseURL(r) + "/u/" + l.Token, Bucket: l.Bucket, Prefix: l.Prefix, Label: l.Label,
		MaxFiles: l.MaxFiles, Uploads: l.Uploads, MaxFileSize: l.MaxFileSize, Created: l.Created,
		CreatedBy: l.CreatedBy, CreatedByName: l.CreatedByName, Active: active,
	}
	if !l.Expires.IsZero() {
		exp := l.Expires
		out.Expires = &exp
	}
	return out, nil
}

// normalizeFolder turns "invoices/2026" or "/invoices/2026/" into
// "invoices/2026/"; "" or "/" is the bucket's top level.
func normalizeFolder(p string) (string, error) {
	p = strings.Trim(p, "/")
	if p == "" {
		return "", nil
	}
	p += "/"
	if strings.Contains(p, "//") {
		return "", badRequest("the folder %q has an empty part", p)
	}
	if err := store.ValidateKey(p + "x"); err != nil {
		return "", err
	}
	return p, nil
}

func validateLabel(s string) error {
	if len(s) > maxUploadLinkLabel || !utf8.ValidString(s) || strings.ContainsFunc(s, unicode.IsControl) {
		return badRequest(`"label" must be at most %d bytes of text without control characters`, maxUploadLinkLabel)
	}
	return nil
}

// createUploadLink: anyone who may write the bucket may let others upload.
func (a *api) createUploadLink(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct {
		Bucket      string `json:"bucket"`
		Folder      string `json:"folder"`
		Label       string `json:"label"`
		ExpiresIn   int64  `json:"expiresIn"` // seconds; 0 = never
		MaxFiles    int    `json:"maxFiles"`
		MaxFileSize int64  `json:"maxFileSize"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if !p.Can(auth.PermWrite, req.Bucket, "") {
		a.fail(w, r, errAccessDenied)
		return
	}
	if err := store.ValidateBucketName(req.Bucket); err != nil {
		a.fail(w, r, err)
		return
	}
	folder, err := normalizeFolder(req.Folder)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	label := strings.TrimSpace(req.Label)
	if err := validateLabel(label); err != nil {
		a.fail(w, r, err)
		return
	}
	lifetime := time.Duration(req.ExpiresIn) * time.Second
	switch {
	case req.ExpiresIn < 0 || lifetime > maxLinkLifetime:
		a.fail(w, r, badRequest(`"expiresIn" must be 0 (never) or up to %d seconds`, int64(maxLinkLifetime/time.Second)))
		return
	case req.MaxFiles < 0 || req.MaxFiles > maxUploadLinkFiles:
		a.fail(w, r, badRequest(`"maxFiles" must be 0 (no limit) to %d`, maxUploadLinkFiles))
		return
	case req.MaxFileSize < 0 || req.MaxFileSize > a.maxObject:
		a.fail(w, r, badRequest(`"maxFileSize" must be 0 (the server's limit) to %d bytes`, a.maxObject))
		return
	}

	var tok [16]byte
	rand.Read(tok[:])
	var id [8]byte
	rand.Read(id[:])
	now := time.Now().UTC()
	rec := store.UploadLinkRecord{
		ID: hex.EncodeToString(id[:]), Token: base64.RawURLEncoding.EncodeToString(tok[:]),
		Bucket: req.Bucket, Prefix: folder, Label: label,
		MaxFiles: req.MaxFiles, MaxFileSize: req.MaxFileSize,
		Created: now, CreatedBy: p.KeyID, CreatedByName: p.KeyName,
	}
	if lifetime > 0 {
		rec.Expires = now.Add(lifetime).Truncate(time.Second)
	}
	if err := a.uplinks.CreateUploadLink(r.Context(), rec); err != nil {
		a.fail(w, r, err)
		return
	}
	out, err := a.toUploadLinkJSON(r, rec)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, out)
}

func (a *api) listUploadLinks(w http.ResponseWriter, r *http.Request) {
	bucket := r.URL.Query().Get("bucket")
	if _, ok := a.authorize(w, r, auth.Target{Bucket: bucket}, auth.PermWrite); !ok {
		return
	}
	recs, err := a.uplinks.ListUploadLinks(r.Context(), bucket)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]uploadLinkJSON, 0, len(recs))
	for _, l := range recs {
		j, err := a.toUploadLinkJSON(r, l)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		out = append(out, j)
	}
	writeJSON(w, http.StatusOK, map[string][]uploadLinkJSON{"uploadLinks": out})
}

// deleteUploadLink revokes a link: its creator or any bucket writer.
func (a *api) deleteUploadLink(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	rec, err := a.uplinks.UploadLinkByID(r.Context(), r.PathValue("id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if rec.CreatedBy != p.KeyID && !p.Can(auth.PermWrite, rec.Bucket, "") {
		a.fail(w, r, errAccessDenied)
		return
	}
	if err := a.uplinks.DeleteUploadLink(r.Context(), rec.ID); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// liveUploadLink finds the link for the request's token, refusing expired
// ones. It names the link as the actor in the access log.
func (a *api) liveUploadLink(r *http.Request) (store.UploadLinkRecord, error) {
	rec, err := a.uplinks.UploadLinkByToken(r.Context(), r.PathValue("token"))
	if err != nil {
		return rec, err
	}
	setActor(r, "uplink:"+rec.ID)
	if !rec.Expires.IsZero() && time.Now().After(rec.Expires) {
		return rec, fmt.Errorf("%w on %s", errLinkExpired, rec.Expires.Format(time.RFC3339))
	}
	// A link acts for whoever made it: it works only while they could
	// still write the bucket themselves. Otherwise it looks revoked.
	ok, err := a.auth.CreatorCan(r.Context(), rec.CreatedBy, auth.PermWrite, rec.Bucket, "")
	if err != nil {
		return rec, err
	}
	if !ok {
		return rec, fmt.Errorf("%w: its creator no longer has access", store.ErrNoSuchLink)
	}
	return rec, nil
}

// maxFileSize is the largest file a link accepts.
func (a *api) uploadLinkMaxSize(rec store.UploadLinkRecord) int64 {
	if rec.MaxFileSize > 0 {
		return min(rec.MaxFileSize, a.maxObject)
	}
	return a.maxObject
}

// uploadLinkInfo tells the upload page what it may do. It reveals nothing
// about the bucket: not its name, folder or files.
func (a *api) uploadLinkInfo(w http.ResponseWriter, r *http.Request) {
	setNoIndex(w)
	rec, err := a.liveUploadLink(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var filesLeft *int
	if rec.MaxFiles > 0 {
		n := max(rec.MaxFiles-rec.Uploads, 0)
		filesLeft = &n
	}
	var expires *time.Time
	if !rec.Expires.IsZero() {
		expires = &rec.Expires
	}
	out := map[string]any{
		"label": rec.Label, "maxFileSize": a.uploadLinkMaxSize(rec), "filesLeft": filesLeft, "expires": expires,
	}
	if a.operator != (operatorJSON{}) {
		out["operator"] = a.operator
	}
	writeJSON(w, http.StatusOK, out)
}

// uploadViaLink stores one file sent to PUT /u/{token}/{name}.
func (a *api) uploadViaLink(w http.ResponseWriter, r *http.Request) {
	setNoIndex(w)
	// As in putObject: never drain a refused body to keep the connection.
	fail := func(err error) {
		w.Header().Set("Connection", "close")
		a.fail(w, r, err)
	}
	rec, err := a.liveUploadLink(r)
	if err != nil {
		fail(err)
		return
	}
	name := r.PathValue("name")
	if err := validateFilename(name); err != nil {
		fail(err)
		return
	}
	limit := a.uploadLinkMaxSize(rec)
	if r.ContentLength > limit {
		fail(&http.MaxBytesError{Limit: limit})
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)

	// Claim a slot first, so concurrent uploads can't pass maxFiles; give
	// it back if the upload fails.
	if _, err := a.uplinks.ClaimUploadSlot(r.Context(), rec.Token); err != nil {
		fail(err)
		return
	}
	key, err := a.freeKey(r, rec.Bucket, rec.Prefix, name)
	var info store.ObjectInfo
	if err == nil {
		info, err = a.store.Put(r.Context(), rec.Bucket, key, r.Body, store.Meta{
			ContentType: r.Header.Get("Content-Type"), SizeHint: r.ContentLength, NoOverwrite: true,
		})
	}
	if err != nil {
		// Give the slot back only if nothing was stored. When the commit's
		// outcome is unknown the file may be there, and it must keep
		// counting toward maxFiles.
		if !errors.Is(err, store.ErrCommitUncertain) {
			if rerr := a.uplinks.ReleaseUploadSlot(r.Context(), rec.Token); rerr != nil {
				a.log.WarnContext(r.Context(), "could not release upload slot", "link", rec.ID, "err", rerr)
			}
		}
		fail(err)
		return
	}
	a.log.InfoContext(r.Context(), "file uploaded through upload link",
		"request_id", RequestID(r.Context()), "link", rec.ID, "bucket", rec.Bucket, "key", key, "size", info.Size)
	// Not the name it was stored under: "r (2).pdf" would tell the uploader
	// that someone else's r.pdf is in the folder, and they may not list it.
	writeJSON(w, http.StatusCreated, map[string]any{"size": info.Size})
}

// freeKey returns prefix+name, or prefix+"name (n).ext" for the first n
// not taken, or after maxNameTries a random "name (3f9a…).ext". It never
// fails because of what is already in the folder: a refusal would also
// tell the uploader about files they can't see. A race for the same name
// is caught by NoOverwrite at commit.
func (a *api) freeKey(r *http.Request, bucket, prefix, name string) (string, error) {
	stem, ext := name, path.Ext(name)
	if ext == name || ext == "" { // ".env" or "README": no extension to keep
		ext = ""
	} else {
		stem = strings.TrimSuffix(name, ext)
	}
	for n := 1; n <= maxNameTries; n++ {
		candidate := name
		if n > 1 {
			candidate = fmt.Sprintf("%s (%d)%s", stem, n, ext)
		}
		key := prefix + candidate
		_, err := a.store.Head(r.Context(), bucket, key)
		if errors.Is(err, store.ErrNoSuchKey) {
			return key, nil
		}
		if err != nil {
			return "", err
		}
	}
	var rnd [8]byte
	rand.Read(rnd[:])
	return prefix + fmt.Sprintf("%s (%s)%s", stem, hex.EncodeToString(rnd[:]), ext), nil
}

func setNoIndex(w http.ResponseWriter) {
	w.Header().Set("X-Robots-Tag", "noindex")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Cache-Control", "no-store")
}
