package auth

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
)

func init() { pbkdf2Iterations = 1000 } // keep tests fast; the format is the same

// newUserService returns a Service backed by a real engine, with a fake clock.
func newUserService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := NewService(eng, eng.PresignSecret()).UseUsers(eng)
	s.now = func() time.Time { return clock }
	return s, &clock
}

func authAs(t *testing.T, s *Service, token string) (Principal, error) {
	t.Helper()
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+token)
	return s.Authenticate(r, Target{})
}

func TestPasswordHash(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Errorf("hash format = %q", h)
	}
	if !passwordMatches(h, "correct horse battery") || passwordMatches(h, "correct horse batterY") {
		t.Error("password check is wrong")
	}
	h2, _ := hashPassword("correct horse battery")
	if h == h2 {
		t.Error("same hash twice: salt missing")
	}
	// A hash made with another iteration count still verifies.
	old := pbkdf2Iterations
	pbkdf2Iterations = 2000
	h3, _ := hashPassword("pw-made-with-2000")
	pbkdf2Iterations = old
	if !passwordMatches(h3, "pw-made-with-2000") {
		t.Error("hash with a different iteration count no longer verifies")
	}
	for _, bad := range []string{"", "plain", "pbkdf2-sha256$x$a$b", "md5$1$a$b"} {
		if passwordMatches(bad, "anything") {
			t.Errorf("malformed hash %q matched", bad)
		}
	}
}

func TestUserRules(t *testing.T) {
	for _, tt := range []struct {
		name string
		ok   bool
	}{{"alice", true}, {"Alice", true}, {"a.b-c_d", true}, {"a", false}, {"-x", false}, {"has space", false}, {strings.Repeat("a", 65), false}} {
		if _, err := NormalizeUserName(tt.name); (err == nil) != tt.ok {
			t.Errorf("NormalizeUserName(%q) err = %v, want ok=%v", tt.name, err, tt.ok)
		}
	}
	for _, tt := range []struct {
		pw string
		ok bool
	}{{"short", false}, {"ten chars!", true}, {"alice12345", true}, {"ALICEALICE", true}, {strings.Repeat("x", 257), false}} {
		if err := CheckPassword("aliceali", tt.pw); (err == nil) != tt.ok {
			t.Errorf("CheckPassword(%q) err = %v, want ok=%v", tt.pw, err, tt.ok)
		}
	}
	if err := CheckPassword("longusername", "LongUserName"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("password equal to the name accepted: %v", err)
	}
}

func TestLoginAndSessions(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	if _, err := s.CreateUser(ctx, "Alice", "a long password", []string{"invoices:rw"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "alice", "another password", []string{"*:r"}); !errors.Is(err, store.ErrUserExists) {
		t.Fatalf("duplicate (case-insensitive) user: err = %v", err)
	}

	if _, _, _, err := s.Login(ctx, "alice", "wrong password", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong password: err = %v", err)
	}
	if _, _, _, err := s.Login(ctx, "nobody", "a long password", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("unknown user: err = %v (must be the same error as a wrong password)", err)
	}
	token, sess, user, err := s.Login(ctx, " ALICE ", "a long password", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(token, "bfs_") || user.Name != "alice" || user.LastLogin.IsZero() || sess.Expires != clock.Add(SessionMax) {
		t.Fatalf("login = %q %+v %+v", token, sess, user)
	}

	p, err := authAs(t, s, token)
	if err != nil {
		t.Fatal(err)
	}
	if p.User != "alice" || p.KeyID != "user:alice" || !p.Can(PermWrite, "invoices", "") || p.Can(PermRead, "other", "") {
		t.Fatalf("principal = %+v", p)
	}

	// Scope changes apply to open sessions at once.
	if _, err := s.SetUserScopes(ctx, "alice", []string{"other:r"}); err != nil {
		t.Fatal(err)
	}
	if p, _ := authAs(t, s, token); p.Can(PermRead, "invoices", "") || !p.Can(PermRead, "other", "") {
		t.Errorf("scopes not updated: %+v", p)
	}

	// Idle timeout: used just before it is fine and refreshes LastSeen...
	*clock = clock.Add(SessionIdle - time.Minute)
	if _, err := authAs(t, s, token); err != nil {
		t.Fatalf("session ended before the idle timeout: %v", err)
	}
	// ...so another almost-idle stretch is fine too.
	*clock = clock.Add(SessionIdle - time.Minute)
	if _, err := authAs(t, s, token); err != nil {
		t.Fatalf("activity did not extend the session: %v", err)
	}
	// Past the idle timeout it is gone.
	*clock = clock.Add(SessionIdle + time.Minute)
	if _, err := authAs(t, s, token); !errors.Is(err, ErrUnauthenticated) {
		t.Fatalf("idle session still works: %v", err)
	}

	// Absolute expiry, however active: used every hour, it still ends
	// at SessionMax.
	token, sess, _, _ = s.Login(ctx, "alice", "a long password", "1.2.3.4")
	for {
		*clock = clock.Add(time.Hour)
		if _, err := authAs(t, s, token); err != nil {
			break
		}
		if clock.Sub(sess.Created) > SessionMax+time.Hour {
			t.Fatal("session outlived SessionMax")
		}
	}
	if lived := clock.Sub(sess.Created); lived < SessionMax {
		t.Fatalf("active session ended after %s, before SessionMax", lived)
	}

	// Logout ends just that session.
	t1, _, _, _ := s.Login(ctx, "alice", "a long password", "1.2.3.4")
	t2, _, _, _ := s.Login(ctx, "alice", "a long password", "5.6.7.8")
	p1, _ := authAs(t, s, t1)
	if err := s.Logout(ctx, p1); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, s, t1); err == nil {
		t.Error("logged-out session still works")
	}
	if _, err := authAs(t, s, t2); err != nil {
		t.Errorf("logout ended another session: %v", err)
	}
	if err := s.Logout(ctx, Principal{KeyID: "k"}); !errors.Is(err, ErrNotSession) {
		t.Errorf("logout with an API key: err = %v", err)
	}

	// Deleting the user ends their sessions.
	if err := s.DeleteUser(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, s, t2); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("session of a deleted user works: %v", err)
	}
}

func TestPasswordChangeEndsOtherSessions(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	s.CreateUser(ctx, "bob", "first password", []string{"*:r"})
	here, _, _, _ := s.Login(ctx, "bob", "first password", "1.1.1.1")
	there, _, _, _ := s.Login(ctx, "bob", "first password", "2.2.2.2")
	p, _ := authAs(t, s, here)

	if err := s.ChangeOwnPassword(ctx, p, "not it", "second password"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("wrong current password: err = %v", err)
	}
	if err := s.ChangeOwnPassword(ctx, p, "first password", "short"); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("too-short new password: err = %v", err)
	}
	if err := s.ChangeOwnPassword(ctx, p, "first password", "second password"); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, s, here); err != nil {
		t.Errorf("own session ended by own password change: %v", err)
	}
	if _, err := authAs(t, s, there); err == nil {
		t.Error("other session survived the password change")
	}
	if _, _, _, err := s.Login(ctx, "bob", "first password", "1.1.1.1"); err == nil {
		t.Error("old password still works")
	}
	if _, _, _, err := s.Login(ctx, "bob", "second password", "1.1.1.1"); err != nil {
		t.Errorf("new password: %v", err)
	}

	// An admin reset ends every session of that user.
	if err := s.SetPassword(ctx, "bob", "third password", Principal{User: "admin"}); err != nil {
		t.Fatal(err)
	}
	if _, err := authAs(t, s, here); err == nil {
		t.Error("session survived an admin password reset")
	}
	if n, err := s.SignOutUser(ctx, "nobody"); !errors.Is(err, store.ErrNoSuchUser) {
		t.Errorf("SignOutUser(unknown) = %d, %v", n, err)
	}
}

func TestLoginThrottle(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	s.CreateUser(ctx, "carol", "carols password", []string{"*:r"})

	// Per user: after maxFailsPerUser misses, even the right password waits.
	for i := range maxFailsPerUser {
		if _, _, _, err := s.Login(ctx, "carol", "guess", "10.0.0."+string(rune('a'+i))); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	_, _, _, err := s.Login(ctx, "carol", "carols password", "10.9.9.9")
	var tm TooManyAttemptsError
	if !errors.As(err, &tm) || tm.Wait <= 0 || tm.Wait > throttleWindow {
		t.Fatalf("after %d misses: err = %v", maxFailsPerUser, err)
	}
	*clock = clock.Add(throttleWindow)
	if _, _, _, err := s.Login(ctx, "carol", "carols password", "10.9.9.9"); err != nil {
		t.Fatalf("still blocked after the window: %v", err)
	}

	// Per IP: one address spraying many names is stopped too.
	for i := range maxFailsPerIP {
		s.Login(ctx, "user"+string(rune('a'+i%26))+string(rune('a'+i/26)), "guess", "6.6.6.6")
	}
	if _, _, _, err := s.Login(ctx, "carol", "carols password", "6.6.6.6"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("IP not throttled: %v", err)
	}
	if _, _, _, err := s.Login(ctx, "carol", "carols password", "7.7.7.7"); err != nil {
		t.Fatalf("another IP was blocked: %v", err)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	s.CreateUser(ctx, "dave", "daves password", []string{"*:r"})
	old, _, _, _ := s.Login(ctx, "dave", "daves password", "1.1.1.1")
	*clock = clock.Add(SessionIdle + time.Hour)
	fresh, _, _, _ := s.Login(ctx, "dave", "daves password", "1.1.1.1")
	n, err := s.PurgeExpiredSessions(ctx)
	if err != nil || n != 1 {
		t.Fatalf("purge = %d, %v; want 1", n, err)
	}
	if _, err := authAs(t, s, old); err == nil {
		t.Error("expired session works")
	}
	if _, err := authAs(t, s, fresh); err != nil {
		t.Errorf("fresh session purged: %v", err)
	}
}

func TestConcurrentLogins(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	s.CreateUser(ctx, "erin", "erins password", []string{"*:r"})
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			if _, _, _, err := s.Login(ctx, "erin", "erins password", "1.1.1."+string(rune('0'+i))); err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
}
