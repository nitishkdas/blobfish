package server

import (
	"errors"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Users sign in with a name and password (POST /v1/login) and get a
// session token they send like an API key. Server admins manage users at
// /v1/users; a signed-in user changes their own password at /v1/password.

type userJSON struct {
	Name            string    `json:"name"`
	Scopes          []string  `json:"scopes"`
	Created         time.Time `json:"created"`
	PasswordChanged time.Time `json:"passwordChanged"`
	LastLogin       time.Time `json:"lastLogin,omitzero"`
}

func toUserJSON(u store.UserRecord) userJSON {
	return userJSON{Name: u.Name, Scopes: u.Scopes, Created: u.Created, PasswordChanged: u.PasswordChanged, LastLogin: u.LastLogin}
}

// failThrottled answers a TooManyAttemptsError with 429 and Retry-After.
func (a *api) failThrottled(w http.ResponseWriter, r *http.Request, err error) bool {
	var tm auth.TooManyAttemptsError
	if !errors.As(err, &tm) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(tm.Wait.Seconds()))))
	a.fail(w, r, err)
	return true
}

func (a *api) login(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	setActor(r, "login:"+truncate(req.Username, 64))
	token, sess, user, err := a.auth.Login(r.Context(), req.Username, req.Password, clientFrom(r).ip)
	if err != nil {
		if !a.failThrottled(w, r, err) {
			a.fail(w, r, err)
		}
		return
	}
	setActor(r, "user:"+user.Name+"/"+sess.ID)
	a.log.InfoContext(r.Context(), "user signed in", "request_id", RequestID(r.Context()), "user", user.Name, "session", sess.ID)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"token":              token,
		"expires":            sess.Expires,
		"idleTimeoutSeconds": int(auth.SessionIdle.Seconds()),
		"user":               toUserJSON(user),
	})
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func (a *api) logout(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.auth.Logout(r.Context(), p); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) changeOwnPassword(w http.ResponseWriter, r *http.Request) {
	p, err := a.authenticate(r, auth.Target{})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	var req struct {
		Current string `json:"currentPassword"`
		New     string `json:"newPassword"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if err := a.auth.ChangeOwnPassword(r.Context(), p, req.Current, req.New); err != nil {
		if !a.failThrottled(w, r, err) {
			a.fail(w, r, err)
		}
		return
	}
	a.log.InfoContext(r.Context(), "user changed own password", "request_id", RequestID(r.Context()), "user", p.User)
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) listUsers(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	users, err := a.auth.ListUsers(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	out := make([]userJSON, 0, len(users))
	for _, u := range users {
		out = append(out, toUserJSON(u))
	}
	writeJSON(w, http.StatusOK, map[string][]userJSON{"users": out})
}

func (a *api) createUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	var req struct {
		Name     string   `json:"name"`
		Password string   `json:"password"`
		Scopes   []string `json:"scopes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	u, err := a.auth.CreateUser(r.Context(), req.Name, req.Password, req.Scopes)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "user created", "request_id", RequestID(r.Context()), "user", u.Name, "scopes", u.Scopes)
	writeJSON(w, http.StatusCreated, toUserJSON(u))
}

// updateUser sets a new password (ending all the user's sessions) and/or
// new scopes (applied to open sessions at once).
func (a *api) updateUser(w http.ResponseWriter, r *http.Request) {
	p, ok := a.authorize(w, r, serverWide, auth.PermAdmin)
	if !ok {
		return
	}
	name := strings.ToLower(r.PathValue("name")) // names are stored lowercase
	var req struct {
		Password *string   `json:"password"`
		Scopes   *[]string `json:"scopes"`
	}
	if err := decodeJSON(w, r, &req); err != nil {
		a.fail(w, r, err)
		return
	}
	if req.Password == nil && req.Scopes == nil {
		a.fail(w, r, badRequest(`nothing to change: send {"password": "..."} and/or {"scopes": [...]}`))
		return
	}
	// Check both before changing either, so a bad request changes nothing.
	if req.Password != nil {
		if err := auth.CheckPassword(name, *req.Password); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	if req.Scopes != nil {
		if _, err := a.auth.SetUserScopes(r.Context(), name, *req.Scopes); err != nil {
			a.fail(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "user access changed", "request_id", RequestID(r.Context()), "user", name, "scopes", *req.Scopes)
	}
	if req.Password != nil {
		if err := a.auth.SetPassword(r.Context(), name, *req.Password, p); err != nil {
			a.fail(w, r, err)
			return
		}
		a.log.InfoContext(r.Context(), "user password reset", "request_id", RequestID(r.Context()), "user", name)
	}
	u, err := a.auth.GetUser(r.Context(), name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toUserJSON(u))
}

func (a *api) deleteUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	name := strings.ToLower(r.PathValue("name")) // names are stored lowercase
	if err := a.auth.DeleteUser(r.Context(), name); err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "user deleted", "request_id", RequestID(r.Context()), "user", name)
	w.WriteHeader(http.StatusNoContent)
}

// signOutUser ends every session of a user ("sign out everywhere").
func (a *api) signOutUser(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	name := strings.ToLower(r.PathValue("name")) // names are stored lowercase
	n, err := a.auth.SignOutUser(r.Context(), name)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.log.InfoContext(r.Context(), "user signed out everywhere", "request_id", RequestID(r.Context()), "user", name, "sessions", n)
	writeJSON(w, http.StatusOK, map[string]int{"sessionsEnded": n})
}
