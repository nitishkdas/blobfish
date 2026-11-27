package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

func TestUserEmailIndex(t *testing.T) {
	ctx := context.Background()
	e, err := Open(t.TempDir(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	now := time.Now().UTC()
	user := func(name, email string) store.UserRecord {
		return store.UserRecord{Name: name, Email: email, Scopes: []string{"*:r"}, PasswordHash: "x", Created: now}
	}
	byEmail := func(email string) string {
		t.Helper()
		u, err := e.UserByEmail(ctx, email)
		if errors.Is(err, store.ErrNoSuchUser) {
			return ""
		}
		if err != nil {
			t.Fatal(err)
		}
		return u.Name
	}

	if err := e.CreateUser(ctx, user("ann", "ann@example.com")); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateUser(ctx, user("bob", "ann@example.com")); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("second user with the same email: err = %v, want ErrEmailTaken", err)
	}
	if _, err := e.GetUser(ctx, "bob"); !errors.Is(err, store.ErrNoSuchUser) {
		t.Fatal("refused user was stored anyway")
	}
	if err := e.CreateUser(ctx, user("bob", "")); err != nil { // no email is fine, and not unique
		t.Fatal(err)
	}
	if err := e.CreateUser(ctx, user("cy", "")); err != nil {
		t.Fatal(err)
	}
	if got := byEmail("ann@example.com"); got != "ann" {
		t.Fatalf("UserByEmail = %q, want ann", got)
	}

	setEmail := func(name, email string) error {
		_, err := e.UpdateUser(ctx, name, func(u *store.UserRecord) error { u.Email = email; return nil }, false, "")
		return err
	}
	// Taking someone else's email is refused and changes nothing.
	if err := setEmail("bob", "ann@example.com"); !errors.Is(err, store.ErrEmailTaken) {
		t.Fatalf("taking ann's email: err = %v", err)
	}
	if b, _ := e.GetUser(ctx, "bob"); b.Email != "" {
		t.Fatalf("bob's email = %q after a refused change", b.Email)
	}
	// Changing an email frees the old one at once.
	if err := setEmail("ann", "ann@new.example"); err != nil {
		t.Fatal(err)
	}
	if byEmail("ann@example.com") != "" || byEmail("ann@new.example") != "ann" {
		t.Fatal("email index not moved")
	}
	if err := setEmail("bob", "ann@example.com"); err != nil {
		t.Fatalf("old address should be free: %v", err)
	}
	// Setting the same email again is a no-op, not a conflict with oneself.
	if err := setEmail("bob", "ann@example.com"); err != nil {
		t.Fatal(err)
	}
	// Removing and deleting free the address.
	if err := setEmail("bob", ""); err != nil {
		t.Fatal(err)
	}
	if byEmail("ann@example.com") != "" {
		t.Fatal("removed email still signs in")
	}
	if err := e.DeleteUser(ctx, "ann"); err != nil {
		t.Fatal(err)
	}
	if byEmail("ann@new.example") != "" {
		t.Fatal("deleted user's email still in the index")
	}
	if err := setEmail("cy", "ann@new.example"); err != nil {
		t.Fatalf("deleted user's address should be free: %v", err)
	}
	e.db.View(func(tx *bbolt.Tx) error {
		if p := checkUserEmails(tx); len(p) != 0 {
			t.Errorf("index inconsistent: %v", p)
		}
		return nil
	})
}

// A format-5 data dir has no email index; opening it creates the index and
// fills it from any user records that have an email.
func TestFormat5To6IndexesEmails(t *testing.T) {
	dir := t.TempDir()
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	e.Close()
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(bktUserEmail); err != nil {
			return err
		}
		if err := tx.Bucket(bktUsers).Put([]byte("old"), []byte(`{"name":"old","scopes":["*:r"],"passwordHash":"x"}`)); err != nil {
			return err
		}
		if err := tx.Bucket(bktUsers).Put([]byte("mail"), []byte(`{"name":"mail","email":"m@example.com","scopes":["*:r"],"passwordHash":"x"}`)); err != nil {
			return err
		}
		return tx.Bucket(bktSettings).Put(setFormat, []byte("5"))
	})
	e, err = Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if u, err := e.UserByEmail(context.Background(), "m@example.com"); err != nil || u.Name != "mail" {
		t.Fatalf("UserByEmail after upgrade = %+v, %v", u, err)
	}
	e.db.View(func(tx *bbolt.Tx) error {
		if v := string(tx.Bucket(bktSettings).Get(setFormat)); v != "6" {
			t.Errorf("format = %s, want 6", v)
		}
		return nil
	})
}

func TestVerifyUserEmailIndex(t *testing.T) {
	dir, _ := verifyFixture(t)
	editDB(t, dir, func(tx *bbolt.Tx) error {
		users := tx.Bucket(bktUsers)
		// One user whose email is not indexed, one index entry for nobody.
		if err := users.Put([]byte("ann"), []byte(`{"name":"ann","email":"ann@example.com","scopes":["*:r"],"passwordHash":"x"}`)); err != nil {
			return err
		}
		return tx.Bucket(bktUserEmail).Put([]byte("ghost@example.com"), []byte("ghost"))
	})
	_, probs := runVerify(t, dir, VerifyOptions{})
	if len(probs) != 2 {
		t.Fatalf("problems = %v, want 2", probs)
	}
	for _, p := range probs {
		if p.Kind != ProblemMetadata || strings.Contains(p.String(), "@") {
			t.Errorf("problem %q: want a metadata problem that doesn't show the address", p)
		}
	}
}
