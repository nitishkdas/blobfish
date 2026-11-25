package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

// memKeys is an in-memory store.Keys for unit tests.
type memKeys struct {
	mu   sync.Mutex
	recs []store.KeyRecord
}

func (m *memKeys) CreateKey(_ context.Context, r store.KeyRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.recs = append(m.recs, r)
	return nil
}

func (m *memKeys) KeyByHash(_ context.Context, h string) (store.KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.recs {
		if r.Hash == h {
			return r, nil
		}
	}
	return store.KeyRecord{}, store.ErrNoSuchAPIKey
}

func (m *memKeys) ListKeys(context.Context) ([]store.KeyRecord, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.recs), nil
}

func (m *memKeys) DeleteKey(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, r := range m.recs {
		if r.ID == id {
			m.recs = slices.Delete(m.recs, i, i+1)
			return nil
		}
	}
	return store.ErrNoSuchAPIKey
}

func newSvc(t *testing.T) *Service {
	t.Helper()
	return NewService(&memKeys{}, []byte("0123456789abcdef0123456789abcdef"))
}

func TestParseScope(t *testing.T) {
	tests := []struct {
		in      string
		want    Scope
		wantErr bool
	}{
		{"invoices:r", Scope{"invoices", PermRead}, false},
		{"invoices:w", Scope{"invoices", PermWrite}, false},
		{"invoices:rw", Scope{"invoices", PermRead | PermWrite}, false},
		{"*:admin", Scope{"*", PermRead | PermWrite | PermAdmin}, false},
		{"invoices", Scope{}, true},
		{"invoices:x", Scope{}, true},
		{"Bad_Name:r", Scope{}, true},
		{":r", Scope{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := ParseScope(tt.in)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidScope) {
					t.Fatalf("err = %v, want ErrInvalidScope", err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("ParseScope(%q) = %+v, %v; want %+v", tt.in, got, err, tt.want)
			}
		})
	}
}

func TestCan(t *testing.T) {
	scopes := func(ss ...string) Principal {
		var p Principal
		for _, s := range ss {
			sc, err := ParseScope(s)
			if err != nil {
				t.Fatal(err)
			}
			p.Scopes = append(p.Scopes, sc)
		}
		return p
	}
	tests := []struct {
		name   string
		p      Principal
		perm   Perm
		bucket string
		want   bool
	}{
		{"read own bucket", scopes("inv:r"), PermRead, "inv", true},
		{"read-only cannot write", scopes("inv:r"), PermWrite, "inv", false},
		{"write-only cannot read", scopes("inv:w"), PermRead, "inv", false},
		{"rw both", scopes("inv:rw"), PermRead | PermWrite, "inv", true},
		{"other bucket", scopes("inv:rw"), PermRead, "photos", false},
		{"bucket admin deletes bucket", scopes("inv:admin"), PermAdmin, "inv", true},
		{"bucket admin is not server admin", scopes("inv:admin"), PermAdmin, "*", false},
		{"rw is not admin", scopes("*:rw"), PermAdmin, "inv", false},
		{"star rw reads anything", scopes("*:rw"), PermRead, "photos", true},
		{"server admin", scopes("*:admin"), PermAdmin, "*", true},
		{"union of scopes", scopes("a-b:r", "inv:w"), PermWrite, "inv", true},
		{"no scopes", Principal{}, PermRead, "inv", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.p.Can(tt.perm, tt.bucket, "k"); got != tt.want {
				t.Errorf("Can = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestBearerAuth(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()
	token, rec, err := s.CreateKey(ctx, "app", []string{"inv:rw"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.Hash == token || !strings.HasPrefix(token, "bf_") {
		t.Fatalf("token/hash look wrong: %q %q", token, rec.Hash)
	}

	tests := []struct {
		name    string
		header  string
		wantErr error
	}{
		{"valid", "Bearer " + token, nil},
		{"scheme case-insensitive", "bearer " + token, nil},
		{"missing", "", ErrUnauthenticated},
		{"wrong scheme", "Basic " + token, ErrUnauthenticated},
		{"unknown token", "Bearer bf_nope", ErrUnauthenticated},
		{"no prefix", "Bearer " + strings.TrimPrefix(token, "bf_"), ErrUnauthenticated},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/b/inv/k", nil)
			if tt.header != "" {
				r.Header.Set("Authorization", tt.header)
			}
			p, err := s.Authenticate(r, Target{Bucket: "inv", Key: "k"})
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err == nil && (p.KeyID != rec.ID || !p.Can(PermWrite, "inv", "k")) {
				t.Errorf("principal = %+v", p)
			}
		})
	}
}

func TestPresigned(t *testing.T) {
	s := newSvc(t)
	now := time.Unix(1_800_000_000, 0)
	s.now = func() time.Time { return now }

	getQ, _, err := s.Presign(http.MethodGet, "inv", "a/b c.pdf", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	putQ, _, err := s.Presign(http.MethodPut, "inv", "a/b c.pdf", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	tamper := func(q, k, v string) string {
		vals, _ := url.ParseQuery(q)
		vals.Set(k, v)
		return vals.Encode()
	}
	obj := Target{"inv", "a/b c.pdf"}

	tests := []struct {
		name     string
		method   string
		target   Target
		query    string
		advance  time.Duration
		wantErr  error
		wantPerm Perm
	}{
		{"get ok", http.MethodGet, obj, getQ, 0, nil, PermRead},
		{"head with get url", http.MethodHead, obj, getQ, 0, nil, PermRead},
		{"put ok", http.MethodPut, obj, putQ, 0, nil, PermWrite},
		{"get url used for put", http.MethodPut, obj, getQ, 0, ErrSignatureInvalid, 0},
		{"other key", http.MethodGet, Target{"inv", "a/b c.pdg"}, getQ, 0, ErrSignatureInvalid, 0},
		{"other bucket", http.MethodGet, Target{"inx", "a/b c.pdf"}, getQ, 0, ErrSignatureInvalid, 0},
		{"exp extended", http.MethodGet, obj, tamper(getQ, "exp", "9999999999"), 0, ErrSignatureInvalid, 0},
		{"sig garbage", http.MethodGet, obj, tamper(getQ, "sig", "00"), 0, ErrSignatureInvalid, 0},
		{"exp not a number", http.MethodGet, obj, tamper(getQ, "exp", "soon"), 0, ErrSignatureInvalid, 0},
		{"still valid at expiry", http.MethodGet, obj, getQ, time.Minute, nil, PermRead},
		{"expired", http.MethodGet, obj, getQ, time.Minute + time.Second, ErrSignatureExpired, 0},
		{"not on bucket-level routes", http.MethodGet, Target{"inv", ""}, getQ, 0, ErrUnauthenticated, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s.now = func() time.Time { return now.Add(tt.advance) }
			r := httptest.NewRequest(tt.method, "/x?"+tt.query, nil)
			p, err := s.Authenticate(r, tt.target)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("err = %v, want %v", err, tt.wantErr)
			}
			if err != nil {
				return
			}
			if !p.Can(tt.wantPerm, tt.target.Bucket, tt.target.Key) {
				t.Errorf("presigned principal lacks perm %d", tt.wantPerm)
			}
			other := PermWrite
			if tt.wantPerm == PermWrite {
				other = PermRead
			}
			if p.Can(other, tt.target.Bucket, tt.target.Key) {
				t.Errorf("presigned principal has extra perm %d", other)
			}
			if p.Can(tt.wantPerm, tt.target.Bucket, "other-key") {
				t.Error("presigned principal works on another key")
			}
		})
	}
}

func TestPresignValidation(t *testing.T) {
	s := newSvc(t)
	tests := []struct {
		name   string
		method string
		ttl    time.Duration
		want   error
	}{
		{"delete not allowed", http.MethodDelete, time.Minute, ErrInvalidPresign},
		{"zero ttl", http.MethodGet, 0, ErrInvalidPresign},
		{"too long", http.MethodGet, MaxPresignTTL + time.Second, ErrInvalidPresign},
		{"max ok", http.MethodGet, MaxPresignTTL, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := s.Presign(tt.method, "inv", "k", tt.ttl); !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestKeyLifecycle(t *testing.T) {
	s := newSvc(t)
	ctx := context.Background()

	token, err := s.EnsureAdminKey(ctx)
	if err != nil || token == "" {
		t.Fatalf("EnsureAdminKey = %q, %v", token, err)
	}
	if again, err := s.EnsureAdminKey(ctx); err != nil || again != "" {
		t.Fatalf("second EnsureAdminKey = %q, %v; want no new key", again, err)
	}
	keys, _ := s.ListKeys(ctx)
	admin := keys[0]

	if err := s.DeleteKey(ctx, admin.ID); !errors.Is(err, ErrLastAdminKey) {
		t.Fatalf("deleting only admin: %v, want ErrLastAdminKey", err)
	}
	_, second, err := s.CreateKey(ctx, "admin2", []string{"*:admin"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteKey(ctx, admin.ID); err != nil {
		t.Fatalf("deleting one of two admins: %v", err)
	}
	if err := s.DeleteKey(ctx, second.ID); !errors.Is(err, ErrLastAdminKey) {
		t.Fatalf("deleting new last admin: %v, want ErrLastAdminKey", err)
	}
	if err := s.DeleteKey(ctx, "nope"); !errors.Is(err, store.ErrNoSuchAPIKey) {
		t.Fatalf("deleting unknown: %v, want ErrNoSuchAPIKey", err)
	}

	for _, tc := range []struct {
		name   string
		scopes []string
		want   error
	}{
		{"", []string{"a-b:r"}, ErrInvalidKeyName},
		{"ok", nil, ErrInvalidScope},
		{"ok", []string{"a-b:x"}, ErrInvalidScope},
	} {
		if _, _, err := s.CreateKey(ctx, tc.name, tc.scopes); !errors.Is(err, tc.want) {
			t.Errorf("CreateKey(%q, %v) = %v, want %v", tc.name, tc.scopes, err, tc.want)
		}
	}
}
