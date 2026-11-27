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
	"net/mail"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Errors for users and sessions.
var (
	// ErrBadCredentials: wrong name or password. Deliberately one error,
	// so a login can't tell which.
	ErrBadCredentials  = errors.New("wrong username, email or password")
	ErrTooManyAttempts = errors.New("too many failed sign-ins; wait and try again")
	ErrInvalidUserName = errors.New("invalid username")
	ErrInvalidEmail    = errors.New("invalid email")
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

	// Failed sign-ins allowed per window (see throttle.go). Per name *and*
	// network, so one attacker can't lock a person out from everywhere
	// with 10 guesses; per name overall, so a distributed attack still
	// stops (except on networks the person signed in from recently); per
	// network, so one source can't spray many names. IPv6 counts per /64.
	throttleWindow     = 15 * time.Minute
	maxFailsPerUserNet = 10
	maxFailsPerUser    = 100
	maxFailsPerNet     = 30
	maxFailsOwnPw      = 10 // wrong current password when changing it

	// defaultPasswordCost: PBKDF2-HMAC-SHA256 at OWASP's recommended
	// count. It is in the standard library and needs no memory, which
	// matters on a 512 MB server; the throttle keeps guessing slow anyway.
	// Each hash records its own count, so raising this later doesn't break
	// stored passwords.
	defaultPasswordCost = 600_000
	cheapPasswordCost   = 1000
)

// UseCheapPasswordHashForTests makes this Service hash passwords fast.
// Tests only: stored hashes record their cost, so this never weakens
// existing data, but passwords set while it is on are cheap to guess.
func (s *Service) UseCheapPasswordHashForTests() *Service {
	s.passwordCost = cheapPasswordCost
	return s
}

var userNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// NormalizeUserName lowercases and checks a user name.
func NormalizeUserName(name string) (string, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if !userNameRe.MatchString(name) {
		return "", fmt.Errorf("%w: use 2–64 lowercase letters, numbers, dots, dashes or underscores, starting with a letter or number", ErrInvalidUserName)
	}
	return name, nil
}

// MaxEmailLength is the longest address SMTP allows (RFC 5321 path limit).
const MaxEmailLength = 254

// NormalizeEmail lowercases and checks an email address: one plain
// address (no display name, no comments or quoting), with a dotted domain.
// The whole address is lowercased: mail servers that treat the local part
// as case-sensitive are vanishingly rare, and two accounts differing only
// in case would be a trap. Nothing checks that the address receives mail.
func NormalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	bad := fmt.Errorf("%w: use an address like name@example.com", ErrInvalidEmail)
	if len(email) > MaxEmailLength {
		return "", fmt.Errorf("%w: at most %d characters", ErrInvalidEmail, MaxEmailLength)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Name != "" || addr.Address != email || strings.ContainsAny(email, "\"\\ ") {
		return "", bad
	}
	local, domain, _ := strings.Cut(email, "@")
	if local == "" || len(local) > 64 || !strings.Contains(domain, ".") ||
		strings.HasPrefix(domain, ".") || strings.HasSuffix(domain, ".") || strings.Contains(domain, "..") {
		return "", bad
	}
	return email, nil
}

// CheckPassword applies the password rules: length only (NIST 800-63B);
// composition rules make passwords worse, not better. The password also
// can't be the user's name or email (pass "" for none).
func CheckPassword(name, email, pw string) error {
	n := utf8.RuneCountInString(pw)
	switch {
	case n < MinPasswordLength:
		return fmt.Errorf("%w: use at least %d characters", ErrInvalidPassword, MinPasswordLength)
	case len(pw) > MaxPasswordLength:
		return fmt.Errorf("%w: use at most %d bytes", ErrInvalidPassword, MaxPasswordLength)
	case strings.EqualFold(pw, name):
		return fmt.Errorf("%w: it can't be the username", ErrInvalidPassword)
	case email != "" && strings.EqualFold(pw, email):
		return fmt.Errorf("%w: it can't be the email address", ErrInvalidPassword)
	}
	return nil
}

// hashPassword returns "pbkdf2-sha256$<iter>$<salt>$<hash>".
func hashPassword(pw string, iter int) (string, error) {
	salt := make([]byte, 16)
	rand.Read(salt)
	key, err := pbkdf2.Key(sha256.New, pw, salt, iter, pbkdf2KeyLen)
	if err != nil {
		return "", err
	}
	enc := base64.RawStdEncoding
	return "pbkdf2-sha256$" + strconv.Itoa(iter) + "$" + enc.EncodeToString(salt) + "$" + enc.EncodeToString(key), nil
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

// hash hashes a new password at this Service's cost.
func (s *Service) hash(pw string) (string, error) { return hashPassword(pw, s.passwordCost) }

// dummyHash is checked when the user doesn't exist, so a sign-in takes
// the same time whether or not the name is real. Made at the current
// cost on each use's first need, so it always costs what a real one does.
func (s *Service) dummyHash() string {
	s.dummyMu.Lock()
	defer s.dummyMu.Unlock()
	if s.dummy == "" || s.dummyCost != s.passwordCost {
		s.dummy, _ = hashPassword("not a real password, only for timing", s.passwordCost)
		s.dummyCost = s.passwordCost
	}
	return s.dummy
}

// acquireHash waits for a password-hashing slot, or for ctx to end (the
// client gave up: don't spend CPU on it).
func (s *Service) acquireHash(ctx context.Context) error {
	select {
	case s.hashing <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
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

// LookupLogin finds the user a sign-in names: login is a user name, or an
// email if it contains "@" (names can't). id is what the attempt is
// counted under: the user's name when the user exists, so both ways in
// share one set of limits, else the normalized input. ok is false when no
// user could ever match login (the format is impossible).
func (s *Service) LookupLogin(ctx context.Context, login string) (user store.UserRecord, id string, exists, ok bool, err error) {
	if strings.Contains(login, "@") {
		email, nerr := NormalizeEmail(login)
		if nerr != nil {
			return store.UserRecord{}, "", false, false, nil
		}
		user, err = s.users.UserByEmail(ctx, email)
		id = email
	} else {
		name, nerr := NormalizeUserName(login)
		if nerr != nil {
			return store.UserRecord{}, "", false, false, nil
		}
		user, err = s.users.GetUser(ctx, name)
		id = name
	}
	if errors.Is(err, store.ErrNoSuchUser) {
		return store.UserRecord{}, id, false, true, nil
	}
	if err != nil {
		return store.UserRecord{}, "", false, true, err
	}
	return user, user.Name, true, true, nil
}

// Login checks a user name or email and a password and starts a session.
// clientIP is used only for throttling. The token is returned once and
// never stored.
func (s *Service) Login(ctx context.Context, login, pw, clientIP string) (string, store.SessionRecord, store.UserRecord, error) {
	if s.users == nil {
		return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
	}
	now := s.now()
	net := clientNet(clientIP)
	netLimit := limit{key: "net:" + net, max: maxFailsPerNet}
	user, name, exists, possible, err := s.LookupLogin(ctx, login)
	if err != nil {
		return "", store.SessionRecord{}, store.UserRecord{}, err
	}
	if !possible {
		// No user can have this name, so don't count it per name: junk
		// names (up to the body limit) would otherwise fill the table.
		if wait := s.throttle.begin(now, netLimit); wait > 0 {
			return "", store.SessionRecord{}, store.UserRecord{}, TooManyAttemptsError{Wait: wait}
		}
		return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
	}
	// The same limits apply whether or not the name exists (so the throttle
	// can't be used to find out); only where the counts are kept differs.
	userLimit := limit{key: "u:" + name, max: maxFailsPerUser, pinned: exists}
	if exists && s.throttle.trusted(name, net, now) {
		userLimit.max = 0 // counted, but doesn't block a network they use
	}
	limits := []limit{
		{key: "u:" + name + "|" + net, max: maxFailsPerUserNet, pinned: exists},
		userLimit,
		netLimit,
	}
	if wait := s.throttle.begin(now, limits...); wait > 0 {
		return "", store.SessionRecord{}, store.UserRecord{}, TooManyAttemptsError{Wait: wait}
	}
	if err := s.acquireHash(ctx); err != nil { // few hashes at once: they are CPU-heavy
		s.throttle.undo(now, limits...) // gave up before guessing
		return "", store.SessionRecord{}, store.UserRecord{}, err
	}
	var ok bool
	if exists {
		ok = passwordMatches(user.PasswordHash, pw)
	} else {
		passwordMatches(s.dummyHash(), pw)
	}
	<-s.hashing
	if !ok {
		return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
	}
	// Not a wrong guess: take it back. The per-name total keeps the others'
	// guesses; this network's count for this name starts over.
	s.throttle.undo(now, limits[1:]...)
	s.throttle.reset(limits[0])
	s.throttle.trust(name, net, now)

	var secret [32]byte
	rand.Read(secret[:])
	token := sessionPrefix + base64.RawURLEncoding.EncodeToString(secret[:])
	var id [8]byte
	rand.Read(id[:])
	sess := store.SessionRecord{
		ID: hex.EncodeToString(id[:]), Hash: hashToken(token), User: user.Name,
		Created: now.UTC(), LastSeen: now.UTC(), Expires: now.Add(SessionMax).UTC(),
	}
	// The session is stored only if the password just checked is still the
	// user's: a reset (or the user deleted and made again) while it was
	// being hashed must not let the old password in.
	if err := s.users.CreateSession(ctx, sess, user.PasswordHash); err != nil {
		if errors.Is(err, store.ErrUserChanged) || errors.Is(err, store.ErrNoSuchUser) {
			return "", store.SessionRecord{}, store.UserRecord{}, ErrBadCredentials
		}
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
		// Best effort: the janitor's purge deletes it later anyway.
		if err := s.users.DeleteSession(ctx, hash); err != nil {
			s.logSessionWriteError("delete ended session", err)
		}
		return Principal{}, ErrUnauthenticated
	}
	if now.Sub(sess.LastSeen) > touchEvery {
		// Best effort: if it can't be written (disk full), the session only
		// times out a little sooner; failing the request would block even
		// deletes, which must work on a full disk. But say so: otherwise
		// people get signed out for no visible reason.
		if err := s.users.TouchSession(ctx, hash, now.UTC()); err != nil {
			s.logSessionWriteError("record session activity", err)
		}
	}
	// The user's current scopes, so an admin's change applies at once.
	// KeyID includes the creation time so a later user with the same name
	// doesn't inherit what this one created (share links' "creator").
	p := Principal{KeyID: userKeyID(user), KeyName: user.Name, User: user.Name, SessionID: sess.ID, sessionHash: hash}
	for _, raw := range user.Scopes {
		sc, err := ParseScope(raw)
		if err != nil {
			return Principal{}, fmt.Errorf("stored user %s: %w", user.Name, err)
		}
		p.Scopes = append(p.Scopes, sc)
	}
	return p, nil
}

func userKeyID(u store.UserRecord) string {
	return "user:" + u.Name + "@" + strconv.FormatInt(u.Created.UnixNano(), 36)
}

// Logout ends the caller's own session.
func (s *Service) Logout(ctx context.Context, p Principal) error {
	if p.sessionHash == "" {
		return ErrNotSession
	}
	return s.users.DeleteSession(ctx, p.sessionHash)
}

// CheckScopes checks a user's scope list: at least one, each valid.
func CheckScopes(scopes []string) error { return checkScopes(scopes) }

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

// CreateUser adds a user with a password and scopes. email is optional
// ("" for none).
func (s *Service) CreateUser(ctx context.Context, name, email, pw string, scopes []string) (store.UserRecord, error) {
	name, err := NormalizeUserName(name)
	if err != nil {
		return store.UserRecord{}, err
	}
	if email != "" {
		if email, err = NormalizeEmail(email); err != nil {
			return store.UserRecord{}, err
		}
	}
	if err := CheckPassword(name, email, pw); err != nil {
		return store.UserRecord{}, err
	}
	if err := checkScopes(scopes); err != nil {
		return store.UserRecord{}, err
	}
	hash, err := s.hash(pw)
	if err != nil {
		return store.UserRecord{}, err
	}
	now := s.now().UTC()
	rec := store.UserRecord{Name: name, Email: email, Scopes: scopes, PasswordHash: hash, Created: now, PasswordChanged: now}
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
	user, err := s.users.GetUser(ctx, name)
	if err != nil {
		return err
	}
	if err := CheckPassword(name, user.Email, pw); err != nil {
		return err
	}
	hash, err := s.hash(pw)
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
	if err == nil {
		// A reset often means the old password leaked: networks it was
		// used from are no longer vouched for.
		s.throttle.forget(name)
	}
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
	l := limit{key: "pw:" + p.User, max: maxFailsOwnPw, pinned: true}
	if wait := s.throttle.begin(now, l); wait > 0 {
		return TooManyAttemptsError{Wait: wait}
	}
	if err := s.acquireHash(ctx); err != nil {
		s.throttle.undo(now, l)
		return err
	}
	ok := passwordMatches(user.PasswordHash, current)
	<-s.hashing
	if !ok {
		return ErrBadCredentials
	}
	s.throttle.undo(now, l)
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

// SetUserEmail sets a user's email; "" removes it. ErrEmailTaken if
// another user has it.
func (s *Service) SetUserEmail(ctx context.Context, name, email string) (store.UserRecord, error) {
	if email != "" {
		var err error
		if email, err = NormalizeEmail(email); err != nil {
			return store.UserRecord{}, err
		}
	}
	return s.users.UpdateUser(ctx, name, func(u *store.UserRecord) error {
		u.Email = email
		return nil
	}, false, "")
}

// DeleteUser removes a user and ends their sessions.
func (s *Service) DeleteUser(ctx context.Context, name string) error {
	err := s.users.DeleteUser(ctx, name)
	if err == nil {
		s.throttle.forget(name)
	}
	return err
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

// logSessionWriteError reports a failed best-effort session write, at
// most once a minute (a full disk would otherwise log on every request).
func (s *Service) logSessionWriteError(what string, err error) {
	now := s.now().UnixNano()
	last := s.lastWriteWarn.Load()
	if now-last < int64(time.Minute) || !s.lastWriteWarn.CompareAndSwap(last, now) {
		return
	}
	s.log.Warn("could not "+what+"; sessions may end early", "err", err)
}
