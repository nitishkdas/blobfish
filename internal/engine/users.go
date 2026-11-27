package engine

import (
	"cmp"
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

var (
	bktUsers    = []byte("users")    // name -> store.UserRecord
	bktSessions = []byte("sessions") // token hash -> store.SessionRecord
	// bktUserEmail indexes users by email: normalized email -> name. Kept
	// in the same transaction as every change to a user's Email.
	bktUserEmail = []byte("useremail")
)

var _ store.Users = (*Engine)(nil)

func getUser(tx *bbolt.Tx, name string) (store.UserRecord, error) {
	var rec store.UserRecord
	v := tx.Bucket(bktUsers).Get([]byte(name))
	if v == nil {
		return rec, fmt.Errorf("%w: %s", store.ErrNoSuchUser, name)
	}
	if err := json.Unmarshal(v, &rec); err != nil {
		return rec, fmt.Errorf("decode user %s: %w", name, err)
	}
	return rec, nil
}

func putJSON(b *bbolt.Bucket, key string, v any) error {
	val, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return b.Put([]byte(key), val)
}

// CreateUser implements store.Users.
func (e *Engine) CreateUser(ctx context.Context, rec store.UserRecord) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		users := tx.Bucket(bktUsers)
		if users.Get([]byte(rec.Name)) != nil {
			return fmt.Errorf("%w: %s", store.ErrUserExists, rec.Name)
		}
		if err := setUserEmail(tx, rec.Name, "", rec.Email); err != nil {
			return err
		}
		return putJSON(users, rec.Name, rec)
	})
}

// setUserEmail moves name's entry in the email index from old to new
// (either may be empty). ErrEmailTaken if new belongs to someone else.
func setUserEmail(tx *bbolt.Tx, name, old, new string) error {
	if old == new {
		return nil
	}
	idx := tx.Bucket(bktUserEmail)
	if new != "" {
		if owner := idx.Get([]byte(new)); owner != nil && string(owner) != name {
			return store.ErrEmailTaken // no address in the error: it may be logged
		}
		if err := idx.Put([]byte(new), []byte(name)); err != nil {
			return err
		}
	}
	if old != "" {
		if owner := idx.Get([]byte(old)); string(owner) == name {
			return idx.Delete([]byte(old))
		}
	}
	return nil
}

// UserByEmail implements store.Users.
func (e *Engine) UserByEmail(ctx context.Context, email string) (store.UserRecord, error) {
	var rec store.UserRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		name := tx.Bucket(bktUserEmail).Get([]byte(email))
		if name == nil {
			return store.ErrNoSuchUser
		}
		var err error
		rec, err = getUser(tx, string(name))
		return err
	})
	return rec, err
}

// GetUser implements store.Users.
func (e *Engine) GetUser(ctx context.Context, name string) (store.UserRecord, error) {
	var rec store.UserRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		var err error
		rec, err = getUser(tx, name)
		return err
	})
	return rec, err
}

// ListUsers implements store.Users.
func (e *Engine) ListUsers(ctx context.Context) ([]store.UserRecord, error) {
	var out []store.UserRecord
	err := e.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktUsers).ForEach(func(k, v []byte) error {
			var rec store.UserRecord
			if err := json.Unmarshal(v, &rec); err != nil {
				return fmt.Errorf("decode user %s: %w", k, err)
			}
			out = append(out, rec)
			return nil
		})
	})
	slices.SortFunc(out, func(a, b store.UserRecord) int {
		return cmp.Or(a.Created.Compare(b.Created), cmp.Compare(a.Name, b.Name))
	})
	return out, err
}

// UpdateUser implements store.Users.
func (e *Engine) UpdateUser(ctx context.Context, name string, change func(*store.UserRecord) error, revokeSessions bool, keepSession string) (store.UserRecord, error) {
	var rec store.UserRecord
	err := e.db.Update(func(tx *bbolt.Tx) error {
		var err error
		if rec, err = getUser(tx, name); err != nil {
			return err
		}
		oldEmail := rec.Email
		if err := change(&rec); err != nil {
			return err
		}
		rec.Name = name // the name is the key; it can't change here
		if err := setUserEmail(tx, name, oldEmail, rec.Email); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket(bktUsers), name, rec); err != nil {
			return err
		}
		if revokeSessions {
			_, err = deleteSessions(tx, func(s store.SessionRecord) bool { return s.User == name && s.Hash != keepSession })
		}
		return err
	})
	return rec, err
}

// DeleteUser implements store.Users.
func (e *Engine) DeleteUser(ctx context.Context, name string) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		user, err := getUser(tx, name)
		if err != nil {
			return err
		}
		if err := setUserEmail(tx, name, user.Email, ""); err != nil {
			return err
		}
		if _, err := deleteSessions(tx, func(s store.SessionRecord) bool { return s.User == name }); err != nil {
			return err
		}
		return tx.Bucket(bktUsers).Delete([]byte(name))
	})
}

// CreateSession implements store.Users.
func (e *Engine) CreateSession(ctx context.Context, rec store.SessionRecord, passwordHash string) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		user, err := getUser(tx, rec.User)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(user.PasswordHash), []byte(passwordHash)) != 1 {
			return fmt.Errorf("%w: %s", store.ErrUserChanged, rec.User)
		}
		sessions := tx.Bucket(bktSessions)
		if sessions.Get([]byte(rec.Hash)) != nil {
			return fmt.Errorf("session hash collision") // 256-bit tokens: never happens
		}
		user.LastLogin = rec.Created
		if err := putJSON(tx.Bucket(bktUsers), user.Name, user); err != nil {
			return err
		}
		return putJSON(sessions, rec.Hash, rec)
	})
}

// SessionByHash implements store.Users.
func (e *Engine) SessionByHash(ctx context.Context, hash string) (store.SessionRecord, store.UserRecord, error) {
	var (
		s store.SessionRecord
		u store.UserRecord
	)
	err := e.db.View(func(tx *bbolt.Tx) error {
		v := tx.Bucket(bktSessions).Get([]byte(hash))
		if v == nil {
			return store.ErrNoSuchSession
		}
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}
		var err error
		u, err = getUser(tx, s.User)
		return err
	})
	return s, u, err
}

// TouchSession implements store.Users.
func (e *Engine) TouchSession(ctx context.Context, hash string, t time.Time) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		sessions := tx.Bucket(bktSessions)
		v := sessions.Get([]byte(hash))
		if v == nil {
			return nil
		}
		var s store.SessionRecord
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}
		s.LastSeen = t
		return putJSON(sessions, hash, s)
	})
}

// DeleteSession implements store.Users.
func (e *Engine) DeleteSession(ctx context.Context, hash string) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bktSessions).Delete([]byte(hash))
	})
}

// DeleteUserSessions implements store.Users.
func (e *Engine) DeleteUserSessions(ctx context.Context, name string) (int, error) {
	var n int
	err := e.db.Update(func(tx *bbolt.Tx) error {
		var err error
		n, err = deleteSessions(tx, func(s store.SessionRecord) bool { return s.User == name })
		return err
	})
	return n, err
}

// PurgeSessions implements store.Users.
func (e *Engine) PurgeSessions(ctx context.Context, expired func(store.SessionRecord) bool) (int, error) {
	var n int
	err := e.db.Update(func(tx *bbolt.Tx) error {
		var err error
		n, err = deleteSessions(tx, expired)
		return err
	})
	return n, err
}

// deleteSessions removes every session match accepts. Sessions are few
// (one per signed-in browser), so a scan is fine.
func deleteSessions(tx *bbolt.Tx, match func(store.SessionRecord) bool) (int, error) {
	sessions := tx.Bucket(bktSessions)
	var doomed [][]byte
	err := sessions.ForEach(func(k, v []byte) error {
		var s store.SessionRecord
		if err := json.Unmarshal(v, &s); err != nil {
			return fmt.Errorf("decode session: %w", err)
		}
		if match(s) {
			doomed = append(doomed, slices.Clone(k))
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	for _, k := range doomed {
		if err := sessions.Delete(k); err != nil {
			return 0, err
		}
	}
	return len(doomed), nil
}

// indexUserEmails (data format 5 -> 6) creates the email index and fills
// it from the user records. Format-5 users have no email, so this
// normally only creates the bucket.
func indexUserEmails(tx *bbolt.Tx) error {
	if _, err := tx.CreateBucketIfNotExists(bktUserEmail); err != nil {
		return err
	}
	users, err := tx.CreateBucketIfNotExists(bktUsers)
	if err != nil {
		return err
	}
	return users.ForEach(func(k, v []byte) error {
		var rec store.UserRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			return fmt.Errorf("decode user %s: %w", k, err)
		}
		return setUserEmail(tx, rec.Name, "", rec.Email)
	})
}

// checkUserEmails compares the email index with the user records: each
// user's email must point back to them, and every entry must belong to a
// user with that email. Damage here could sign someone in as another user.
func checkUserEmails(tx *bbolt.Tx) []string {
	var problems []string
	users, idx := tx.Bucket(bktUsers), tx.Bucket(bktUserEmail)
	if users == nil || idx == nil {
		return []string{"users or email index bucket is missing"}
	}
	users.ForEach(func(k, v []byte) error {
		var rec store.UserRecord
		if err := json.Unmarshal(v, &rec); err != nil {
			problems = append(problems, fmt.Sprintf("user %s: unreadable record: %v", k, err))
			return nil
		}
		if rec.Email != "" && string(idx.Get([]byte(rec.Email))) != rec.Name {
			problems = append(problems, fmt.Sprintf("user %s: email is not in the email index", rec.Name))
		}
		return nil
	})
	idx.ForEach(func(email, name []byte) error {
		var rec store.UserRecord
		v := users.Get(name)
		if v == nil || json.Unmarshal(v, &rec) != nil || rec.Email != string(email) {
			// The address itself stays out of the report: it is personal data.
			problems = append(problems, fmt.Sprintf("email index: an entry for user %s does not match that user", name))
		}
		return nil
	})
	return problems
}
