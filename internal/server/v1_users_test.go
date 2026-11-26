package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/auth"
)

func init() { auth.UseCheapPasswordHashForTests() }

func (a *testAPI) login(name, pw string) string {
	a.t.Helper()
	var out struct {
		Token string   `json:"token"`
		User  userJSON `json:"user"`
	}
	body := `{"username":` + strconv.Quote(name) + `,"password":` + strconv.Quote(pw) + `}`
	json.Unmarshal(a.must(http.StatusOK, "POST", "/v1/login", "", strings.NewReader(body)).Body.Bytes(), &out)
	if !strings.HasPrefix(out.Token, "bfs_") || out.User.Name != strings.ToLower(name) {
		a.t.Fatalf("login response: %+v", out)
	}
	return out.Token
}

func TestUsersAPI(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/other", a.admin, nil)

	// Only server admins manage users.
	bucketAdmin := a.key("docs:admin")
	a.must(http.StatusForbidden, "GET", "/v1/users", bucketAdmin, nil)
	a.must(http.StatusForbidden, "POST", "/v1/users", bucketAdmin, strings.NewReader(`{"name":"x","password":"0123456789","scopes":["docs:r"]}`))

	// Validation.
	a.must(http.StatusBadRequest, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"ann","password":"short","scopes":["docs:rw"]}`))
	a.must(http.StatusBadRequest, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"a b","password":"long enough pw","scopes":["docs:rw"]}`))
	a.must(http.StatusBadRequest, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"ann","password":"long enough pw","scopes":[]}`))
	rr := a.must(http.StatusCreated, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"Ann","password":"long enough pw","scopes":["docs:rw"]}`))
	if strings.Contains(rr.Body.String(), "pbkdf2") || strings.Contains(rr.Body.String(), "assword\":") && strings.Contains(rr.Body.String(), "Hash") {
		t.Fatalf("user JSON leaks the password hash: %s", rr.Body)
	}
	a.must(http.StatusConflict, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"ann","password":"long enough pw","scopes":["docs:rw"]}`))

	// Sign in; the session works like a key with the user's scopes.
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"ann","password":"wrong password"}`))
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"ghost","password":"long enough pw"}`))
	tok := a.login("ANN", "long enough pw")
	var me whoamiResponse
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/whoami", tok, nil).Body.Bytes(), &me)
	if me.User != "ann" || me.KeyID != "user:ann" || len(me.Scopes) != 1 || me.Scopes[0] != "docs:rw" {
		t.Fatalf("whoami = %+v", me)
	}
	a.must(http.StatusOK, "PUT", "/v1/b/docs/a.txt", tok, strings.NewReader("hi"))
	a.must(http.StatusForbidden, "PUT", "/v1/b/other/a.txt", tok, strings.NewReader("hi"))
	a.must(http.StatusForbidden, "GET", "/v1/users", tok, nil)

	// The list shows the last sign-in, never a hash.
	rr = a.must(http.StatusOK, "GET", "/v1/users", a.admin, nil)
	var list struct{ Users []userJSON }
	json.Unmarshal(rr.Body.Bytes(), &list)
	if len(list.Users) != 1 || list.Users[0].LastLogin.IsZero() || strings.Contains(rr.Body.String(), "pbkdf2") {
		t.Fatalf("users = %s", rr.Body)
	}

	// Scope change applies to the open session at once.
	a.must(http.StatusOK, "PATCH", "/v1/users/ann", a.admin, strings.NewReader(`{"scopes":["other:rw"]}`))
	a.must(http.StatusOK, "PUT", "/v1/b/other/a.txt", tok, strings.NewReader("hi"))
	a.must(http.StatusForbidden, "PUT", "/v1/b/docs/b.txt", tok, strings.NewReader("hi"))
	// A bad PATCH changes nothing.
	a.must(http.StatusBadRequest, "PATCH", "/v1/users/ann", a.admin, strings.NewReader(`{"scopes":["docs:rw"],"password":"short"}`))
	a.must(http.StatusForbidden, "PUT", "/v1/b/docs/b.txt", tok, strings.NewReader("hi"))
	a.must(http.StatusNotFound, "PATCH", "/v1/users/ghost", a.admin, strings.NewReader(`{"scopes":["docs:r"]}`))

	// Own password change: wrong current refused; this session survives,
	// others end.
	other := a.login("ann", "long enough pw")
	a.must(http.StatusUnauthorized, "POST", "/v1/password", tok, strings.NewReader(`{"currentPassword":"nope nope nope","newPassword":"brand new password"}`))
	a.must(http.StatusBadRequest, "POST", "/v1/password", a.admin, strings.NewReader(`{"currentPassword":"x","newPassword":"brand new password"}`)) // an API key has no password
	a.must(http.StatusNoContent, "POST", "/v1/password", tok, strings.NewReader(`{"currentPassword":"long enough pw","newPassword":"brand new password"}`))
	a.must(http.StatusOK, "GET", "/v1/whoami", tok, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/whoami", other, nil)
	a.login("ann", "brand new password")

	// Admin: sign out everywhere, then reset password.
	rr = a.must(http.StatusOK, "DELETE", "/v1/users/ann/sessions", a.admin, nil)
	if !strings.Contains(rr.Body.String(), `"sessionsEnded":2`) {
		t.Errorf("sign out everywhere = %s", rr.Body)
	}
	a.must(http.StatusUnauthorized, "GET", "/v1/whoami", tok, nil)
	a.must(http.StatusOK, "PATCH", "/v1/users/ann", a.admin, strings.NewReader(`{"password":"admin chose this"}`))
	tok = a.login("ann", "admin chose this")

	// Logout ends this session only; with an API key it is an error.
	a.must(http.StatusBadRequest, "POST", "/v1/logout", a.admin, nil)
	a.must(http.StatusNoContent, "POST", "/v1/logout", tok, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/whoami", tok, nil)

	// Delete.
	tok = a.login("ann", "admin chose this")
	a.must(http.StatusNoContent, "DELETE", "/v1/users/ann", a.admin, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/whoami", tok, nil)
	a.must(http.StatusNotFound, "DELETE", "/v1/users/ann", a.admin, nil)
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"ann","password":"admin chose this"}`))
}

func TestLoginThrottleHTTP(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "POST", "/v1/users", a.admin, strings.NewReader(`{"name":"bob","password":"bobs password","scopes":["*:r"]}`))
	for range 10 {
		a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"bob","password":"guess guess"}`))
	}
	rr := a.must(http.StatusTooManyRequests, "POST", "/v1/login", "", strings.NewReader(`{"username":"bob","password":"bobs password"}`))
	if s, _ := strconv.Atoi(rr.Header().Get("Retry-After")); s <= 0 || s > 900 {
		t.Errorf("Retry-After = %q", rr.Header().Get("Retry-After"))
	}
	if !strings.Contains(rr.Body.String(), CodeTooManyAttempts) {
		t.Errorf("body = %s", rr.Body)
	}
}
