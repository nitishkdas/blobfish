package auth

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

func TestNormalizeEmail(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"nitish@example.com", "nitish@example.com"},
		{"  Nitish.Das+files@Example.CO.in ", "nitish.das+files@example.co.in"},
		{"a_b-c@sub.example.org", "a_b-c@sub.example.org"},
		{"", ""},
		{"nitish", ""},
		{"@example.com", ""},
		{"nitish@", ""},
		{"nitish@localhost", ""}, // no dot: not a real mail domain
		{"nitish@example..com", ""},
		{"nitish@.example.com", ""},
		{"nitish@example.com.", ""},
		{"a@b@example.com", ""},
		{"Nitish <nitish@example.com>", ""},
		{`"a b"@example.com`, ""},
		{"nitish@exa mple.com", ""},
		{strings.Repeat("a", 65) + "@example.com", ""},
		{"a@" + strings.Repeat("b", 250) + ".com", ""},
	} {
		got, err := NormalizeEmail(tt.in)
		if tt.want == "" {
			if !errors.Is(err, ErrInvalidEmail) {
				t.Errorf("NormalizeEmail(%q) = %q, %v; want ErrInvalidEmail", tt.in, got, err)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("NormalizeEmail(%q) = %q, %v; want %q", tt.in, got, err, tt.want)
		}
	}
}

func TestLoginByEmail(t *testing.T) {
	s, _ := newUserService(t)
	ctx := context.Background()
	u, err := s.CreateUser(ctx, "nitish", " Nitish@Example.com ", "long enough pw", []string{"*:r"})
	if err != nil {
		t.Fatal(err)
	}
	if u.Email != "nitish@example.com" {
		t.Fatalf("stored email = %q, want lowercase and trimmed", u.Email)
	}
	if _, err := s.CreateUser(ctx, "other", "NITISH@example.com", "long enough pw", []string{"*:r"}); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("same email in other case: err = %v, want ErrEmailTaken", err)
	}
	if _, err := s.CreateUser(ctx, "pw", "pw@example.com", "PW@example.com", []string{"*:r"}); !errors.Is(err, ErrInvalidPassword) {
		t.Fatalf("password equal to the email: err = %v", err)
	}
	if _, err := s.CreateUser(ctx, "bad", "not an email", "long enough pw", []string{"*:r"}); !errors.Is(err, ErrInvalidEmail) {
		t.Fatalf("bad email: err = %v", err)
	}

	for _, login := range []string{"nitish", "NITISH@example.com", "nitish@example.com"} {
		tok, _, user, err := s.Login(ctx, login, "long enough pw", "192.0.2.1")
		if err != nil || user.Name != "nitish" {
			t.Fatalf("Login(%q) = %+v, %v", login, user, err)
		}
		if p, err := authAs(t, s, tok); err != nil || p.User != "nitish" {
			t.Fatalf("session from %q: %+v, %v", login, p, err)
		}
	}
	for _, login := range []string{"ghost@example.com", "nitish@", "x@y"} {
		if _, _, _, err := s.Login(ctx, login, "long enough pw", "192.0.2.1"); !errors.Is(err, ErrBadCredentials) {
			t.Errorf("Login(%q): err = %v, want ErrBadCredentials", login, err)
		}
	}

	// A changed email signs in at once; the old one no longer does.
	if _, err := s.SetUserEmail(ctx, "nitish", "n@new.example"); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := s.Login(ctx, "nitish@example.com", "long enough pw", "192.0.2.1"); !errors.Is(err, ErrBadCredentials) {
		t.Errorf("old email still signs in: %v", err)
	}
	if _, _, _, err := s.Login(ctx, "n@new.example", "long enough pw", "192.0.2.1"); err != nil {
		t.Errorf("new email: %v", err)
	}
	// A new password can't be the email either.
	if err := s.SetPassword(ctx, "nitish", "N@new.example", Principal{}); !errors.Is(err, ErrInvalidPassword) {
		t.Errorf("SetPassword to the email: err = %v", err)
	}
}

// Guesses by email and by name count against the same per-user limits, so
// switching between them doesn't double an attacker's guesses.
func TestLoginByEmailSharesThrottle(t *testing.T) {
	s, _ := newUserService(t)
	ctx := context.Background()
	if _, err := s.CreateUser(ctx, "nitish", "nitish@example.com", "long enough pw", []string{"*:r"}); err != nil {
		t.Fatal(err)
	}
	for i := range maxFailsPerUserNet {
		login := "nitish"
		if i%2 == 0 {
			login = "nitish@example.com"
		}
		if _, _, _, err := s.Login(ctx, login, "wrong guess", "192.0.2.1"); !errors.Is(err, ErrBadCredentials) {
			t.Fatalf("guess %d: err = %v", i, err)
		}
	}
	for _, login := range []string{"nitish", "nitish@example.com"} {
		if _, _, _, err := s.Login(ctx, login, "long enough pw", "192.0.2.1"); !errors.Is(err, ErrTooManyAttempts) {
			t.Errorf("Login(%q) after %d mixed guesses: err = %v, want ErrTooManyAttempts", login, maxFailsPerUserNet, err)
		}
	}
}
