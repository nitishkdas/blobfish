package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestUserEmailAPI(t *testing.T) {
	a := newTestAPI(t)
	log := withLog(a)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)

	rr := a.must(http.StatusCreated, "POST", "/v1/users", a.admin,
		strings.NewReader(`{"name":"nitish","email":"Nitish@Example.com","password":"long enough pw","scopes":["docs:rw"]}`))
	var u userJSON
	json.Unmarshal(rr.Body.Bytes(), &u)
	if u.Name != "nitish" || u.Email != "nitish@example.com" {
		t.Fatalf("created user = %+v", u)
	}
	// Email is optional.
	a.must(http.StatusCreated, "POST", "/v1/users", a.admin,
		strings.NewReader(`{"name":"priya","password":"long enough pw","scopes":["docs:r"]}`))
	if code := a.errCode(http.StatusConflict, "POST", "/v1/users", a.admin,
		`{"name":"other","email":"NITISH@example.com","password":"long enough pw","scopes":["docs:r"]}`); code != "EmailTaken" {
		t.Errorf("duplicate email: code %s", code)
	}
	if code := a.errCode(http.StatusBadRequest, "POST", "/v1/users", a.admin,
		`{"name":"other","email":"not-an-email","password":"long enough pw","scopes":["docs:r"]}`); code != "InvalidEmail" {
		t.Errorf("bad email: code %s", code)
	}

	// Sign in with either; the session is the same user.
	loginAs := func(login string) string {
		t.Helper()
		var out struct {
			Token string   `json:"token"`
			User  userJSON `json:"user"`
		}
		body := `{"username":"` + login + `","password":"long enough pw"}`
		json.Unmarshal(a.must(http.StatusOK, "POST", "/v1/login", "", strings.NewReader(body)).Body.Bytes(), &out)
		if out.User.Name != "nitish" {
			t.Fatalf("login as %s: %+v", login, out)
		}
		return out.Token
	}
	tok := loginAs("nitish@EXAMPLE.com")
	a.must(http.StatusOK, "PUT", "/v1/b/docs/a.txt", tok, strings.NewReader("hi"))
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"nitish@example.com","password":"wrong password"}`))
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "", strings.NewReader(`{"username":"ghost@example.com","password":"wrong password"}`))

	// Priya can't take nitish's email, and a PATCH that fails changes
	// nothing else either.
	if code := a.errCode(http.StatusConflict, "PATCH", "/v1/users/priya", a.admin,
		`{"email":"nitish@example.com","scopes":["*:admin"]}`); code != "EmailTaken" {
		t.Errorf("PATCH to a taken email: code %s", code)
	}
	if code := a.errCode(http.StatusBadRequest, "PATCH", "/v1/users/priya", a.admin,
		`{"email":"priya@example.com","scopes":["nope"]}`); code != "InvalidScope" {
		t.Errorf("PATCH with a bad scope: code %s", code)
	}
	if code := a.errCode(http.StatusBadRequest, "PATCH", "/v1/users/priya", a.admin,
		`{"email":"priya@example.com","password":"priya@example.com"}`); code != "InvalidPassword" {
		t.Errorf("PATCH password = new email: code %s", code)
	}
	users := func() map[string]userJSON {
		var list struct{ Users []userJSON }
		json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/users", a.admin, nil).Body.Bytes(), &list)
		m := map[string]userJSON{}
		for _, u := range list.Users {
			m[u.Name] = u
		}
		return m
	}
	if p := users()["priya"]; p.Email != "" || len(p.Scopes) != 1 || p.Scopes[0] != "docs:r" {
		t.Fatalf("refused PATCHes changed priya: %+v", p)
	}

	// Change and remove.
	a.must(http.StatusOK, "PATCH", "/v1/users/nitish", a.admin, strings.NewReader(`{"email":"n@new.example"}`))
	a.must(http.StatusOK, "PATCH", "/v1/users/priya", a.admin, strings.NewReader(`{"email":"nitish@example.com"}`))
	if m := users(); m["nitish"].Email != "n@new.example" || m["priya"].Email != "nitish@example.com" {
		t.Fatalf("after changes: %+v", m)
	}
	a.must(http.StatusOK, "PATCH", "/v1/users/priya", a.admin, strings.NewReader(`{"email":""}`))
	if m := users(); m["priya"].Email != "" {
		t.Fatalf("email not removed: %+v", m["priya"])
	}
	loginAs("n@new.example")

	// Emails never reach the log; the user name does.
	out := log.String()
	for _, email := range []string{"nitish@example.com", "n@new.example", "ghost@example.com", "priya@example.com", "Nitish@Example.com"} {
		if strings.Contains(strings.ToLower(out), strings.ToLower(email)) {
			t.Errorf("log contains %q:\n%s", email, out)
		}
	}
	for _, want := range []string{"key=login:nitish", `key="login:(unknown name)"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

// errCode makes a request expecting status and returns the error code.
func (a *testAPI) errCode(status int, method, target, token, body string) string {
	a.t.Helper()
	var e struct {
		Error struct{ Code string } `json:"error"`
	}
	json.Unmarshal(a.must(status, method, target, token, strings.NewReader(body)).Body.Bytes(), &e)
	return e.Error.Code
}
