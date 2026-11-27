package auth

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
)

// newUserService returns a Service backed by a real engine, with a fake clock.
func newUserService(t *testing.T) (*Service, *time.Time) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	clock := time.Date(2026, 11, 26, 9, 0, 0, 0, time.UTC)
	s := NewService(eng, eng.PresignSecret()).UseUsers(eng).UseCheapPasswordHashForTests()
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
	h, err := hashPassword("correct horse battery", cheapPasswordCost)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(h, "pbkdf2-sha256$1000$") {
		t.Errorf("hash format = %q", h)
	}
	if !passwordMatches(h, "correct horse battery") || passwordMatches(h, "correct horse batterY") {
		t.Error("password check is wrong")
	}
	h2, _ := hashPassword("correct horse battery", cheapPasswordCost)
	if h == h2 {
		t.Error("same hash twice: salt missing")
	}
	// A hash made with another iteration count still verifies.
	h3, _ := hashPassword("pw-made-with-2000", 2000)
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
		if err := CheckPassword("aliceali", "", tt.pw); (err == nil) != tt.ok {
			t.Errorf("CheckPassword(%q) err = %v, want ok=%v", tt.pw, err, tt.ok)
		}
	}
	if err := CheckPassword("longusername", "", "LongUserName"); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("password equal to the name accepted: %v", err)
	}
}

func TestLoginAndSessions(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	if _, err := s.CreateUser(ctx, "Alice", "", "a long password", []string{"invoices:rw"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateUser(ctx, "alice", "", "another password", []string{"*:r"}); !errors.Is(err, store.ErrUserExists) {
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
	if p.User != "alice" || !strings.HasPrefix(p.KeyID, "user:alice@") || !p.Can(PermWrite, "invoices", "") || p.Can(PermRead, "other", "") {
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
	s.CreateUser(ctx, "bob", "", "first password", []string{"*:r"})
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
	s.CreateUser(ctx, "carol", "", "carols password", []string{"*:r"})
	login := func(pw, ip string) error {
		_, _, _, err := s.Login(ctx, "carol", pw, ip)
		return err
	}

	// One network guessing carol's password gets blocked after
	// maxFailsPerUserNet, even with the right password...
	for i := range maxFailsPerUserNet {
		if err := login("guess guess", "10.0.0.1"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	var tm TooManyAttemptsError
	if err := login("carols password", "10.0.0.1"); !errors.As(err, &tm) || tm.Wait <= 0 || tm.Wait > throttleWindow {
		t.Fatalf("after %d misses: err = %v", maxFailsPerUserNet, err)
	}
	// ...but carol herself, elsewhere, can still sign in (no lockout).
	if err := login("carols password", "192.0.2.7"); err != nil {
		t.Fatalf("carol locked out by someone else's guesses: %v", err)
	}
	*clock = clock.Add(throttleWindow)

	// A distributed attack on one name stops at maxFailsPerUser overall.
	for i := range maxFailsPerUser {
		login("guess guess", fmt.Sprintf("10.1.%d.%d", i/250, i%250))
	}
	if err := login("carols password", "198.51.100.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("name not throttled after %d misses from many networks: %v", maxFailsPerUser, err)
	}
	*clock = clock.Add(throttleWindow)

	// One network spraying many names is stopped too.
	for i := range maxFailsPerNet {
		s.Login(ctx, fmt.Sprintf("user%d", i), "guess guess", "6.6.6.6")
	}
	if err := login("carols password", "6.6.6.6"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("network not throttled: %v", err)
	}

	// IPv6 counts per /64: rotating addresses inside it doesn't help.
	*clock = clock.Add(throttleWindow)
	for i := range maxFailsPerNet {
		s.Login(ctx, fmt.Sprintf("user%d", i), "guess guess", fmt.Sprintf("2001:db8:1:2::%x", i+1))
	}
	if err := login("carols password", "2001:db8:1:2:ffff::9"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("same /64 not throttled: %v", err)
	}
	if err := login("carols password", "2001:db8:1:3::1"); err != nil {
		t.Fatalf("another /64 was blocked: %v", err)
	}
}

func TestThrottleTableStaysSmall(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	// Junk names (impossible formats, huge) are never stored per name.
	long := strings.Repeat("x", 60_000)
	for i := range 50 {
		s.Login(ctx, long+fmt.Sprint(i), "pw", fmt.Sprintf("10.9.0.%d", i))
	}
	s.throttle.mu.Lock()
	for _, m := range []map[string]*failCount{s.throttle.pinned, s.throttle.loose} {
		for k := range m {
			if len(k) > 200 {
				t.Errorf("throttle key of %d bytes stored", len(k))
			}
		}
	}
	s.throttle.mu.Unlock()
	// A flood of fresh networks within one window stays under the cap.
	now := s.now()
	for i := range 3 * maxLooseEntries {
		s.throttle.begin(now, limit{key: fmt.Sprintf("net:k%d", i), max: maxFailsPerNet})
	}
	if n := len(s.throttle.loose); n > maxLooseEntries {
		t.Errorf("throttle table grew to %d entries, cap %d", n, maxLooseEntries)
	}
}

// Guesses sent all at once are counted before they are hashed, so a burst
// can't slip past the limit while it waits for a hashing slot.
func TestThrottleHoldsUnderParallelGuesses(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	if _, err := s.CreateUser(ctx, "dave", "", "daves password", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	for range cap(s.hashing) {
		s.hashing <- struct{}{} // hold every slot, so the whole burst queues
	}
	const burst = 60
	errs := make(chan error, burst)
	var wg sync.WaitGroup
	for i := range burst {
		wg.Go(func() {
			_, _, _, err := s.Login(ctx, "dave", fmt.Sprintf("guess %d!!!", i), "198.51.100.7")
			errs <- err
		})
	}
	// Every request has either been refused or is waiting for a slot.
	for deadline := time.Now().Add(5 * time.Second); ; {
		s.throttle.mu.Lock()
		f := s.throttle.loose["net:198.51.100.7"]
		counted := 0
		if f != nil {
			counted = f.n
		}
		s.throttle.mu.Unlock()
		if counted == maxFailsPerUserNet || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // let the refused ones return
	for range cap(s.hashing) {
		<-s.hashing
	}
	wg.Wait()
	close(errs)
	checked := 0
	for err := range errs {
		if errors.Is(err, ErrBadCredentials) {
			checked++
		} else if !errors.Is(err, ErrTooManyAttempts) {
			t.Errorf("unexpected error: %v", err)
		}
	}
	if checked != maxFailsPerUserNet {
		t.Errorf("%d of %d parallel guesses were checked, limit %d", checked, burst, maxFailsPerUserNet)
	}
}

// Filling the table from many networks must not wipe a real user's count.
func TestThrottleFloodKeepsUserCounts(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	if _, err := s.CreateUser(ctx, "erin", "", "erins password", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	for i := range maxFailsPerUser {
		s.Login(ctx, "erin", "wrong wrong", fmt.Sprintf("10.1.%d.%d", i/250, i%250))
	}
	if _, _, _, err := s.Login(ctx, "erin", "wrong again", "10.2.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("per-name limit not reached: %v", err)
	}
	// Junk names cost no hashing; flood the table with them from fresh
	// networks, well past its cap.
	for i := range 2 * maxLooseEntries {
		s.Login(ctx, "!!", "x", fmt.Sprintf("2001:db8:%x:%x::1", i>>16, i&0xffff))
	}
	if _, _, _, err := s.Login(ctx, "erin", "erins password", "10.3.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Fatalf("flood reset erin's count: %v", err)
	}
}

// A distributed attack on a name doesn't lock its owner out of a network
// they signed in from; it still stops everyone else.
func TestThrottleTrustsKnownNetwork(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	if _, err := s.CreateUser(ctx, "fay", "", "fays password", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Login(ctx, "fay", "fays password", "203.0.113.9"); err != nil {
		t.Fatal(err)
	}
	*clock = clock.Add(time.Hour)
	for i := range maxFailsPerUser + 5 {
		s.Login(ctx, "fay", "wrong wrong", fmt.Sprintf("10.4.%d.%d", i/250, i%250))
	}
	if _, _, _, err := s.Login(ctx, "fay", "fays password", "10.5.0.1"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("new network not stopped by the per-name limit: %v", err)
	}
	if _, _, _, err := s.Login(ctx, "fay", "fays password", "203.0.113.9"); err != nil {
		t.Errorf("owner locked out of their own network: %v", err)
	}
	// After a password reset the network is no longer vouched for.
	if err := s.SetPassword(ctx, "fay", "fays new password", Principal{}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Login(ctx, "fay", "fays new password", "203.0.113.9"); !errors.Is(err, ErrTooManyAttempts) {
		t.Errorf("reset didn't forget known networks: %v", err)
	}
}

// resetDuringLogin changes the user's password right after a sign-in has
// read the user record, i.e. while the old password is being checked.
type resetDuringLogin struct {
	store.Users
	once  sync.Once
	reset func()
}

func (r *resetDuringLogin) GetUser(ctx context.Context, name string) (store.UserRecord, error) {
	u, err := r.Users.GetUser(ctx, name)
	r.once.Do(r.reset)
	return u, err
}

func TestLoginRacingPasswordReset(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	if _, err := s.CreateUser(ctx, "gil", "", "old password!", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	base := s.users
	s.users = &resetDuringLogin{Users: base, reset: func() {
		if _, err := base.UpdateUser(ctx, "gil", func(u *store.UserRecord) error {
			u.PasswordHash, _ = s.hash("new password!")
			return nil
		}, true, ""); err != nil {
			t.Error(err)
		}
	}}
	if _, _, _, err := s.Login(ctx, "gil", "old password!", "1.2.3.4"); !errors.Is(err, ErrBadCredentials) {
		t.Fatalf("old password signed in after a reset: %v", err)
	}
	s.users = base
	if _, _, _, err := s.Login(ctx, "gil", "new password!", "1.2.3.4"); err != nil {
		t.Fatalf("new password: %v", err)
	}
}

func TestClientNet(t *testing.T) {
	for in, want := range map[string]string{
		"203.0.113.5":          "203.0.113.5",
		"::ffff:203.0.113.5":   "203.0.113.5",
		"2001:db8:a:b:c:d:e:f": "2001:db8:a:b::/64",
		"2001:db8:a:b::1":      "2001:db8:a:b::/64",
		"not an ip":            "not an ip",
	} {
		if got := clientNet(in); got != want {
			t.Errorf("clientNet(%q) = %q, want %q", in, got, want)
		}
	}
}

// A request whose client gave up doesn't wait for a hashing slot.
func TestLoginGivesUpWhenCancelled(t *testing.T) {
	s, _ := newUserService(t)
	for range cap(s.hashing) {
		s.hashing <- struct{}{} // all slots busy
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, _, err := s.Login(ctx, "anyone", "some password", "1.1.1.1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's error", err)
	}
}

// failingTouch is a store whose LastSeen writes fail (a full disk).
type failingTouch struct{ store.Users }

func (failingTouch) TouchSession(context.Context, string, time.Time) error {
	return errors.New("no space left on device")
}

func TestSessionWorksWhenLastSeenCantBeWritten(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	s.CreateUser(ctx, "fay", "", "fays password", []string{"*:rw"})
	tok, _, _, _ := s.Login(ctx, "fay", "fays password", "1.1.1.1")
	s.users = failingTouch{s.users}
	*clock = clock.Add(touchEvery + time.Minute) // due for a LastSeen write
	if _, err := authAs(t, s, tok); err != nil {
		t.Fatalf("request failed because LastSeen couldn't be saved: %v", err)
	}
}

// A new user with a deleted user's name doesn't get their identity
// (KeyID is what share links record as their creator).
func TestRecreatedUserIsADifferentPrincipal(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	s.CreateUser(ctx, "gus", "", "first gus password", []string{"*:r"})
	tok, _, _, _ := s.Login(ctx, "gus", "first gus password", "1.1.1.1")
	first, _ := authAs(t, s, tok)
	s.DeleteUser(ctx, "gus")
	*clock = clock.Add(time.Second)
	s.CreateUser(ctx, "gus", "", "second gus password", []string{"*:r"})
	tok, _, _, _ = s.Login(ctx, "gus", "second gus password", "1.1.1.1")
	second, _ := authAs(t, s, tok)
	if !strings.HasPrefix(first.KeyID, "user:gus@") || first.KeyID == second.KeyID {
		t.Errorf("KeyIDs %q and %q", first.KeyID, second.KeyID)
	}
}

func TestPurgeExpiredSessions(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	s.CreateUser(ctx, "dave", "", "daves password", []string{"*:r"})
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
	s.CreateUser(ctx, "erin", "", "erins password", []string{"*:r"})
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

// Two deletes at once must not remove both of the last two admin keys.
func TestDeleteLastAdminKeysConcurrently(t *testing.T) {
	ctx := context.Background()
	s, _ := newUserService(t)
	for round := range 20 {
		_, a, err := s.CreateKey(ctx, fmt.Sprintf("a%d", round), []string{"*:admin"})
		if err != nil {
			t.Fatal(err)
		}
		_, b, err := s.CreateKey(ctx, fmt.Sprintf("b%d", round), []string{"*:admin"})
		if err != nil {
			t.Fatal(err)
		}
		keys, _ := s.ListKeys(ctx)
		for _, k := range keys { // leave exactly a and b as admins
			if k.ID != a.ID && k.ID != b.ID {
				if err := s.DeleteKey(ctx, k.ID); err != nil {
					t.Fatal(err)
				}
			}
		}
		var wg sync.WaitGroup
		for _, id := range []string{a.ID, b.ID} {
			wg.Go(func() { s.DeleteKey(ctx, id) })
		}
		wg.Wait()
		if keys, _ := s.ListKeys(ctx); len(keys) != 1 {
			t.Fatalf("round %d: %d admin keys left, want 1", round, len(keys))
		}
	}
}

// A session write that fails is logged (once a minute), not silent.
func TestSessionWriteFailureIsLogged(t *testing.T) {
	ctx := context.Background()
	s, clock := newUserService(t)
	var buf syncBuffer
	s.WithLogger(slog.New(slog.NewTextHandler(&buf, nil)))
	if _, err := s.CreateUser(ctx, "hal", "", "hals password", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	tok, _, _, err := s.Login(ctx, "hal", "hals password", "1.2.3.4")
	if err != nil {
		t.Fatal(err)
	}
	s.users = failingTouch{s.users}
	*clock = clock.Add(touchEvery + time.Second) // due for a LastSeen write
	for range 3 {                                // each retries the write, since LastSeen stays old
		*clock = clock.Add(10 * time.Second)
		if _, err := authAs(t, s, tok); err != nil {
			t.Fatal(err)
		}
	}
	if n := strings.Count(buf.String(), "could not record session activity"); n != 1 {
		t.Errorf("logged %d times within a minute, want 1:\n%s", n, buf.String())
	}
}

type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
