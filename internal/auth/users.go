package auth

// Users sign in with a name and password and get a session token
// ("bfs_..."), which the web UI sends as a Bearer token exactly like an API
// key. No cookies: a token in a header can't be sent by another site's page
// (no CSRF), and CORS "*" stays harmless.

import (
	"context"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Errors for users and sessions.
var (
	// ErrBadCredentials: wrong name or password. Deliberately one error,
	// so a login can't tell which.
	ErrBadCredentials  = errors.New("wrong user name or password")
	ErrTooManyAttempts = errors.New("too many failed sign-ins; wait and try again")
	ErrInvalidUserName = errors.New("invalid user name")
	ErrInvalidPassword = errors.New("invalid password")
	ErrNotSession      = errors.New("this request is not signed in as a user")
)

// Session and password rules.
const (
	SessionIdle       = 12 * time.Hour     // signed out after this long unused
	SessionMax        = 7 * 24 * time.Hour // and after this long at most
	MinPasswordLength = 10
	MaxPasswordLength = 256
	sessionPrefix     = "bfs_"
	// touchEvery limits LastSeen writes (each is an fsync) to one per
	// session per interval; the idle timeout is that much less precise.
	touchEvery = 5 * time.Minute

	pbkdf2KeyLen = 32

	// Failed sign-ins allowed per window, per user name and per client IP.
	throttleWindow  = 15 * time.Minute
	maxFailsPerUser = 10
	maxFailsPerIP   = 30
)

// pbkdf2Iterations: PBKDF2-HMAC-SHA256 at OWASP's recommended count. It is
// in the standard library and needs no memory, which matters on a 512 MB
// server; the throttle keeps guessing slow anyway. Each hash records its
// own count, so raising this later doesn't break stored passwords. A
// variable only so tests can lower it.
var pbkdf2Iterations = 600_000

// UseCheapPasswordHashForTests makes password hashing fast. Tests only:
// stored hashes record their cost, so this never weakens real data, but
// passwords set while it is on are cheap to guess.
func UseCheapPasswordHashForTests() { pbkdf2Iterations = 1000 }

var userNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// NormalizeUserName lowercases and checks a user name.
func NormalizeUserName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !userNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: use 2-64 lowercase letters, digits, dots, dashes or underscores, starting with a letter or digit", ErrInvalidUserName)
	}
	return name, nil
}

// CheckPassword applies the password rules: length only (NIST 800-63B);
// composition rules make passwords worse, not better.
func CheckPassword(name, pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLength:
		return fmt.Errorf("%w: use at least %d characters", ErrInvalidPassword, MinPasswordLength)
	case len(pw) > MaxPasswordLength:
		return fmt.Errorf("%w: use at most %d bytes", ErrInvalidPassword, MaxPasswordLength)
	case strings.EqualFold(pw, name):
		return fmt.Errorf("%w: it can't be the user name", ErrInvalidPassword)
	}
	return nil
}

// hashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>".
func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, pbkdf2Iterations, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return "pbkdf2-sha256$" + strconv.Itoa(pbkdf2Iterations) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
}

// passwordMatches checks pw against a stored hash in constant time.
func passwordMatches(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 {
		return false
	}
	enc := base64.RawStdEncoding
	salt, err1 := enc.DecodeString(parts[2])
	want, err2 := enc.DecodeString(parts[3])
	if err1 != nil || err2 != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, pw, salt, iter, len(want))
	return err == nil && subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is checked when the user doesn't exist, so a sign-in takes
// the same time whether or not the name is real.
var dummyHash = sync.OnceValue(func() string {
	h, _ := hashPassword("not a real password, only for timing")
	return h
})

// throttle counts failed sign-ins per user name and per client IP.
type throttle struct {
	mu    sync.Mutex
	fails map[string]*failCount
}

type failCount struct {
	n     int
	first time.Time
}

// blocked reports how long key must wait, or 0.
func (t *throttle) blocked(key string, limit int, now time.Time) time.Duration {
	t.mu.Lock()
	defer t.mu.Unlock()
	f := t.fails[key]
	if f == nil || now.Sub(f.first) >= throttleWindow {
		return 0
	}
	if f.n >= limit {
		return f.first.Add(throttleWindow).Sub(now)
	}
	return 0
}

func (t *throttle) fail(key string, now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fails == nil {
		t.fails = map[string]*failCount{}
	}
	if len(t.fails) > 10_000 { // bound memory under a spray of names/IPs
		for k, f := range t.fails {
			if now.Sub(f.first) >= throttleWindow {
				delete(t.fails, k)
			}
		}
	}
	f := t.fails[key]
	if f == nil || now.Sub(f.first) >= throttleWindow {
		t.fails[key] = &failCount{n: 1, first: now}
		return
	}
	f.n++
}

func (t *throttle) reset(key string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.fails, key)
}

// TooManyAttemptsError carries how long to wait (for Retry-After).
type TooManyAttemptsError struct{ Wait time.Duration }

func (e TooManyAttemptsError) Error() string { return ErrTooManyAttempts.Error() }
func (e TooManyAttemptsError) Unwrap() error { return ErrTooManyAttempts }

// UseUsers enables user sign-in, with users and sessions kept in u.
func (s *Service) UseUsers(u store.Users) *Service {
	s.users = u
	return s
}

// Login checks a name and password and starts a session. clientIP is used
// only for throttling. The token is returned once and never stored.
func (s *Service) Login(ctx context.Context, name, pw, clientIP string) (string, store.SessionRecord, store.UserRecord, error) {
	if s.users == nil {
		return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
	}
	now := s.now()
	name = strings.ToLower(strings.TrimSpace(name))
	userKey, ipKey := "u:"+name, "ip:"+clientIP
	if wait := max(s.throttle.blocked(userKey, maxFailsPerUser, now), s.throttle.blocked(ipKey, maxFailsPerIP, now)); wait > 0 {
		return "", store.SessionRecord{}, store.UserRecord{}, TooManyAttemptsError{Wait: wait}
	}
	s.hashing <- struct{}{} // at most a few hashes at once: they are CPU-heavy
	user, err := s.users.GetUser(ctx, name)
	ok := false
	switch {
	case err == nil:
		ok = passwordMatches(user.PasswordHash, pw)
	case errors.Is(err, store.ErrNoSuchUser):
		passwordMatches(dummyHash(), pw)
	default:
		<-s.hashing
		return "", store.SessionRecord{}, store.UserRecord{}, err
	}
	<-s.hashing
	if !ok {
		s.throttle.fail(userKey, now)
		s.throttle.fail(ipKey, now)
		return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
	}
	s.throttle.reset(userKey)

	var secret [32]byte
	rand.Read(secret[:])
	token := sessionPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	var id [8]byte
	rand.Read(id[:])
	sess := store.SessionRecord{
		ID: hex.EncodeToString(id[:]), Hash: hashToken(token), User: user.Name,
		Created: now.UTC(), LastSeen: now.UTC(), Expires: now.Add(SessionMax).UTC(),
	}
	if err := s.users.CreateSession(ctx, sess); err != nil {
		return "", store.SessionRecord{}, store.UserRecord{}, fmt.Errorf("start session: %w", err)
	}
	user.LastLogin = sess.Created
	return token, sess, user, nil
}

// sessionExpired reports whether a session has ended at now.
func sessionExpired(sess store.SessionRecord, now time.Time) bool {
	return now.After(sess.Expires) || now.Sub(sess.LastSeen) > SessionIdle
}

func (s *Service) authenticateSession(ctx context.Context, token string) (Principal, error) {
	if s.users == nil {
		return Principal{}, ErrUnauthenticated
	}
	hash := hashToken(token)
	sess, user, err := s.users.SessionByHash(ctx, hash)
	if errors.Is(err, store.ErrNoSuchSession) || errors.Is(err, store.ErrNoSuchUser) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, fmt.Errorf("look up session: %w", err)
	}
	now := s.now()
	if sessionExpired(sess, now) {
		s.users.DeleteSession(ctx, hash)
		return Principal{}, ErrUnauthenticated
	}
	if now.Sub(sess.LastSeen) > touchEvery {
		if err := s.users.TouchSession(ctx, hash, now.UTC()); err != nil {
			return Principal{}, fmt.Errorf("refresh session: %w", err)
		}
	}
	// The user's current scopes, so an admin's change applies at once.
	p := Principal{KeyID: "user:" + user.Name, KeyName: user.Name, User: user.Name, SessionID: sess.ID, sessionHash: hash}
	for _, raw := range user.Scopes {
		sc, err := ParseScope(raw)
		if err != nil {
			return Principal{}, fmt.Errorf("stored user %s: %w", user.Name, err)
		}
		p.Scopes = append(p.Scopes, sc)
	}
	return p, nil
}

// Logout ends the caller's own session.
func (s *Service) Logout(ctx context.Context, p Principal) error {
	if p.sessionHash == "" {
		return ErrNotSession
	}
	return s.users.DeleteSession(ctx, p.sessionHash)
}

func checkScopes(scopes []string) error {
	if len(scopes) == 0 {
		return fmt.Errorf("%w: at least one scope is required", ErrInvalidScope)
	}
	for _, sc := range scopes {
		if _, err := ParseScope(sc); err != nil {
			return err
		}
	}
	return nil
}

// CreateUser adds a user with a password and scopes.
func (s *Service) CreateUser(ctx context.Context, name, pw string, scopes []string) (store.UserRecord, error) {
	name, err := NormalizeUserName(name)
	if err != nil {
		return store.UserRecord{}, err
	}
	if err := CheckPassword(name, pw); err != nil {
		return store.UserRecord{}, err
	}
	if err := checkScopes(scopes); err != nil {
		return store.UserRecord{}, err
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return store.UserRecord{}, err
	}
	now := s.now().UTC()
	rec := store.UserRecord{Name: name, Scopes: scopes, PasswordHash: hash, Created: now, PasswordChanged: now}
	if err := s.users.CreateUser(ctx, rec); err != nil {
		return store.UserRecord{}, err
	}
	return rec, nil
}

// GetUser returns one user (hash included; callers must not expose it).
func (s *Service) GetUser(ctx context.Context, name string) (store.UserRecord, error) {
	return s.users.GetUser(ctx, name)
}

// ListUsers returns all users (hashes included; callers must not expose them).
func (s *Service) ListUsers(ctx context.Context) ([]store.UserRecord, error) {
	return s.users.ListUsers(ctx)
}

// SetPassword sets a user's password and signs them out everywhere except
// keepSession (the caller's own session when changing their own password).
func (s *Service) SetPassword(ctx context.Context, name, pw string, keep Principal) error {
	if err := CheckPassword(name, pw); err != nil {
		return err
	}
	hash, err := hashPassword(pw)
	if err != nil {
		return err
	}
	keepHash := ""
	if keep.User == name {
		keepHash = keep.sessionHash
	}
	_, err = s.users.UpdateUser(ctx, name, func(u *store.UserRecord) error {
		u.PasswordHash = hash
		u.PasswordChanged = s.now().UTC()
		return nil
	}, true, keepHash)
	return err
}

// ChangeOwnPassword lets a signed-in user change their password, given the
// current one. Their other sessions end; this one stays.
func (s *Service) ChangeOwnPassword(ctx context.Context, p Principal, current, next string) error {
	if p.sessionHash == "" {
		return ErrNotSession
	}
	user, err := s.users.GetUser(ctx, p.User)
	if err != nil {
		return err
	}
	now := s.now()
	key := "u:" + p.User
	if wait := s.throttle.blocked(key, maxFailsPerUser, now); wait > 0 {
		return TooManyAttemptsError{Wait: wait}
	}
	s.hashing <- struct{}{}
	ok := passwordMatches(user.PasswordHash, current)
	<-s.hashing
	if !ok {
		s.throttle.fail(key, now)
		return ErrBadCredentials
	}
	return s.SetPassword(ctx, p.User, next, p)
}

// SetUserScopes changes what a user may do; it applies to their open
// sessions at once.
func (s *Service) SetUserScopes(ctx context.Context, name string, scopes []string) (store.UserRecord, error) {
	if err := checkScopes(scopes); err != nil {
		return store.UserRecord{}, err
	}
	return s.users.UpdateUser(ctx, name, func(u *store.UserRecord) error {
		u.Scopes = scopes
		return nil
	}, false, "")
}

// DeleteUser removes a user and ends their sessions.
func (s *Service) DeleteUser(ctx context.Context, name string) error {
	return s.users.DeleteUser(ctx, name)
}

// SignOutUser ends all of a user's sessions.
func (s *Service) SignOutUser(ctx context.Context, name string) (int, error) {
	if _, err := s.users.GetUser(ctx, name); err != nil {
		return 0, err
	}
	return s.users.DeleteUserSessions(ctx, name)
}

// PurgeExpiredSessions deletes ended sessions (the janitor calls it).
func (s *Service) PurgeExpiredSessions(ctx context.Context) (int, error) {
	if s.users == nil {
		return 0, nil
	}
	now := s.now()
	return s.users.PurgeSessions(ctx, func(sess store.SessionRecord) bool { return sessionExpired(sess, now) })
}
