package engine

import (
	"cmp"
	"context"
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
		return putJSON(users, rec.Name, rec)
	})
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
		if err := change(&rec); err != nil {
			return err
		}
		rec.Name = name // the name is the key; it can't change here
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
		if tx.Bucket(bktUsers).Get([]byte(name)) == nil {
			return fmt.Errorf("%w: %s", store.ErrNoSuchUser, name)
		}
		if _, err := deleteSessions(tx, func(s store.SessionRecord) bool { return s.User == name }); err != nil {
			return err
		}
		return tx.Bucket(bktUsers).Delete([]byte(name))
	})
}

// CreateSession implements store.Users.
func (e *Engine) CreateSession(ctx context.Context, rec store.SessionRecord) error {
	return e.db.Update(func(tx *bbolt.Tx) error {
		user, err := getUser(tx, rec.User)
		if err != nil {
			return err
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
