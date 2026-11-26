// Package auth authenticates /v1 requests (Bearer API keys and presigned
// URLs) and decides what a caller may do. It persists keys only through
// store.Keys.
//
// Scopes have the form "<bucket>:<perm>", where bucket is a bucket name or
// "*" for every bucket, and perm is r, w, rw or admin (admin includes rw).
// Server-wide operations (create bucket, manage keys) need "*:admin".
package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Errors returned by this package. Frontends map them to API codes.
var (
	ErrUnauthenticated  = errors.New("missing or invalid API key or session")
	ErrSignatureInvalid = errors.New("presigned URL signature is invalid")
	ErrSignatureExpired = errors.New("presigned URL has expired")
	ErrInvalidScope     = errors.New("invalid scope")
	ErrInvalidKeyName   = errors.New("invalid key name")
	ErrInvalidPresign   = errors.New("invalid presign request")
	ErrLastAdminKey     = errors.New("cannot delete the last *:admin key")
)

// Presign limits.
const (
	MaxPresignTTL     = 7 * 24 * time.Hour
	DefaultPresignTTL = time.Hour
)

const tokenPrefix = "bf_"

// Perm is a set of permissions.
type Perm uint8

const (
	PermRead Perm = 1 << iota
	PermWrite
	PermAdmin
)

// Scope grants Perm on one bucket, or on all buckets when Bucket is "*".
type Scope struct {
	Bucket string
	Perm   Perm
}

// ParseScope parses "bucket:perm".
func ParseScope(s string) (Scope, error) {
	bucket, p, ok := strings.Cut(s, ":")
	if !ok {
		return Scope{}, fmt.Errorf("%w: %q, want bucket:perm", ErrInvalidScope, s)
	}
	if bucket != "*" {
		if err := store.ValidateBucketName(bucket); err != nil {
			return Scope{}, fmt.Errorf("%w: %q: %v", ErrInvalidScope, s, err)
		}
	}
	var perm Perm
	switch p {
	case "r":
		perm = PermRead
	case "w":
		perm = PermWrite
	case "rw":
		perm = PermRead | PermWrite
	case "admin":
		perm = PermRead | PermWrite | PermAdmin
	default:
		return Scope{}, fmt.Errorf("%w: %q, perm must be r, w, rw or admin", ErrInvalidScope, s)
	}
	return Scope{Bucket: bucket, Perm: perm}, nil
}

// String formats s the way ParseScope reads it, e.g. "invoices:rw".
func (s Scope) String() string {
	var p string
	switch {
	case s.Perm&PermAdmin != 0:
		p = "admin"
	case s.Perm == PermRead|PermWrite:
		p = "rw"
	case s.Perm == PermRead:
		p = "r"
	case s.Perm == PermWrite:
		p = "w"
	default:
		p = fmt.Sprintf("perm(%d)", s.Perm)
	}
	return s.Bucket + ":" + p
}

// Target is what a request acts on. Bucket is "*" for server-wide
// operations; Key is empty for bucket-level ones.
type Target struct {
	Bucket, Key string
}

// Principal is an authenticated caller.
type Principal struct {
	// KeyID and KeyName identify the API key; both are empty for a
	// presigned URL. For a signed-in user they are "user:<name>" and the
	// name, so things a user creates (links) stay theirs across sessions.
	KeyID   string
	KeyName string
	// User and SessionID are set for a signed-in user (session token).
	User      string
	SessionID string
	Scopes    []Scope
	// sessionHash identifies the caller's own session, for logout.
	sessionHash string
	// presigned limits the principal to exactly one object.
	presigned *Target
}

// Can reports whether p holds perm on bucket (and key, for presigned URLs).
// Use bucket "*" for server-wide operations: only "*:..." scopes match it.
func (p Principal) Can(perm Perm, bucket, key string) bool {
	if p.presigned != nil && (bucket != p.presigned.Bucket || key != p.presigned.Key) {
		return false
	}
	for _, s := range p.Scopes {
		if s.Perm&perm == perm && (s.Bucket == "*" || s.Bucket == bucket) {
			return true
		}
	}
	return false
}

// Authenticator identifies the caller of a request. It is an interface so a
// SigV4 implementation can be added for the optional S3 module.
type Authenticator interface {
	Authenticate(r *http.Request, t Target) (Principal, error)
}

// Service is the native Authenticator plus API key management and URL
// presigning.
type Service struct {
	keys     store.Keys
	users    store.Users // nil: user sign-in is off
	secret   []byte
	now      func() time.Time
	throttle throttle
	hashing  chan struct{} // limits concurrent password hashing
}

var _ Authenticator = (*Service)(nil)

// NewService returns a Service that stores keys in keys and signs URLs with
// secret.
func NewService(keys store.Keys, secret []byte) *Service {
	return &Service{keys: keys, secret: secret, now: time.Now, hashing: make(chan struct{}, 2)}
}

// Authenticate implements Authenticator. A request carries either an
// "Authorization: Bearer <key>" header or, for object requests only,
// presigned "exp" and "sig" query parameters.
func (s *Service) Authenticate(r *http.Request, t Target) (Principal, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		scheme, token, ok := strings.Cut(h, " ")
		if !ok || !strings.EqualFold(scheme, "Bearer") {
			return Principal{}, ErrUnauthenticated
		}
		return s.authenticateToken(r.Context(), strings.TrimSpace(token))
	}
	q := r.URL.Query()
	if q.Has("sig") && t.Key != "" {
		return s.authenticatePresigned(r.Method, t, q.Get("exp"), q.Get("sig"))
	}
	return Principal{}, ErrUnauthenticated
}

func (s *Service) authenticateToken(ctx context.Context, token string) (Principal, error) {
	if strings.HasPrefix(token, sessionPrefix) {
		return s.authenticateSession(ctx, token)
	}
	if !strings.HasPrefix(token, tokenPrefix) {
		return Principal{}, ErrUnauthenticated
	}
	rec, err := s.keys.KeyByHash(ctx, hashToken(token))
	if errors.Is(err, store.ErrNoSuchAPIKey) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("look up API key: %w", err)
	}
	p := Principal{KeyID: rec.ID, KeyName: rec.Name}
	for _, raw := range rec.Scopes {
		sc, err := ParseScope(raw)
		if err != nil {
			return Principal{}, fmt.Errorf("stored key %s: %w", rec.ID, err)
		}
		p.Scopes = append(p.Scopes, sc)
	}
	return p, nil
}

func (s *Service) authenticatePresigned(method string, t Target, expStr, sig string) (Principal, error) {
	if method == http.MethodHead {
		method = http.MethodGet // a GET URL may also be used for HEAD
	}
	exp, err := strconv.ParseInt(expStr, 10, 64)
	if err != nil {
		return Principal{}, ErrSignatureInvalid
	}
	// Check the signature before the expiry, so a tampered exp reports
	// "invalid" rather than leaking anything about timing.
	want := s.sign(method, t.Bucket, t.Key, exp)
	if !hmac.Equal([]byte(sig), []byte(want)) {
		return Principal{}, ErrSignatureInvalid
	}
	if s.now().Unix() > exp {
		return Principal{}, ErrSignatureExpired
	}
	perm := PermRead
	if method == http.MethodPut {
		perm = PermWrite
	}
	return Principal{
		Scopes:    []Scope{{Bucket: t.Bucket, Perm: perm}},
		presigned: &t,
	}, nil
}

// sign computes HMAC-SHA256(secret, method NUL bucket NUL key NUL exp).
// NUL separators make the message unambiguous: bucket names and keys
// cannot contain NUL, so no two different requests sign the same bytes.
func (s *Service) sign(method, bucket, key string, exp int64) string {
	m := hmac.New(sha256.New, s.secret)
	for _, part := range []string{method, bucket, key, strconv.FormatInt(exp, 10)} {
		m.Write([]byte(part))
		m.Write([]byte{0})
	}
	return hex.EncodeToString(m.Sum(nil))
}

// Presign returns the query string ("exp=...&sig=...") that authorizes
// method (GET or PUT) on bucket/key until the returned expiry time.
func (s *Service) Presign(method, bucket, key string, ttl time.Duration) (string, time.Time, error) {
	if method != http.MethodGet && method != http.MethodPut {
		return "", time.Time{}, fmt.Errorf("%w: method must be GET or PUT", ErrInvalidPresign)
	}
	if ttl <= 0 || ttl > MaxPresignTTL {
		return "", time.Time{}, fmt.Errorf("%w: expiry must be 1s to %s", ErrInvalidPresign, MaxPresignTTL)
	}
	if err := store.ValidateBucketName(bucket); err != nil {
		return "", time.Time{}, err
	}
	if err := store.ValidateKey(key); err != nil {
		return "", time.Time{}, err
	}
	exp := s.now().Add(ttl).Truncate(time.Second)
	q := url.Values{
		"exp": {strconv.FormatInt(exp.Unix(), 10)},
		"sig": {s.sign(method, bucket, key, exp.Unix())},
	}
	return q.Encode(), exp, nil
}

// CreateKey makes a new API key and returns its token. The token is not
// stored and cannot be recovered later.
func (s *Service) CreateKey(ctx context.Context, name string, scopes []string) (string, store.KeyRecord, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return "", store.KeyRecord{}, fmt.Errorf("%w: must be 1-64 characters", ErrInvalidKeyName)
	}
	if len(scopes) == 0 {
		return "", store.KeyRecord{}, fmt.Errorf("%w: at least one scope is required", ErrInvalidScope)
	}
	for _, sc := range scopes {
		if _, err := ParseScope(sc); err != nil {
			return "", store.KeyRecord{}, err
		}
	}

	var secret [32]byte
	rand.Read(secret[:])
	token := tokenPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	var id [8]byte
	rand.Read(id[:])

	rec := store.KeyRecord{
		ID:      hex.EncodeToString(id[:]),
		Name:    name,
		Scopes:  scopes,
		Created: s.now().UTC(),
		Hash:    hashToken(token),
	}
	if err := s.keys.CreateKey(ctx, rec); err != nil {
		return "", store.KeyRecord{}, fmt.Errorf("create API key: %w", err)
	}
	return token, rec, nil
}

// ListKeys returns all keys (hashes included; callers must not expose them).
func (s *Service) ListKeys(ctx context.Context) ([]store.KeyRecord, error) {
	return s.keys.ListKeys(ctx)
}

// DeleteKey deletes a key, refusing to remove the last "*:admin" key so the
// server cannot be locked out through the API.
func (s *Service) DeleteKey(ctx context.Context, id string) error {
	keys, err := s.keys.ListKeys(ctx)
	if err != nil {
		return err
	}
	admins, targetIsAdmin := 0, false
	for _, k := range keys {
		if isGlobalAdmin(k) {
			admins++
			if k.ID == id {
				targetIsAdmin = true
			}
		}
	}
	if targetIsAdmin && admins == 1 {
		return ErrLastAdminKey
	}
	return s.keys.DeleteKey(ctx, id)
}

// EnsureAdminKey creates a "*:admin" key if no keys exist at all, returning
// its token. It returns "" when keys already exist.
func (s *Service) EnsureAdminKey(ctx context.Context) (string, error) {
	keys, err := s.keys.ListKeys(ctx)
	if err != nil {
		return "", err
	}
	if len(keys) > 0 {
		return "", nil
	}
	token, _, err := s.CreateKey(ctx, "bootstrap-admin", []string{"*:admin"})
	return token, err
}

func isGlobalAdmin(k store.KeyRecord) bool {
	for _, raw := range k.Scopes {
		if sc, err := ParseScope(raw); err == nil && sc.Bucket == "*" && sc.Perm&PermAdmin != 0 {
			return true
		}
	}
	return false
}

func hashToken(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}
