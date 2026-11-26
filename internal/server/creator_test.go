package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

func createShareLink(t *testing.T, a *testAPI, token, bucket, key string) linkJSON {
	t.Helper()
	body := fmt.Sprintf(`{"bucket":%q,"key":%q}`, bucket, key)
	var l linkJSON
	json.Unmarshal(a.must(http.StatusCreated, "POST", "/v1/links", token, strings.NewReader(body)).Body.Bytes(), &l)
	if !l.Active {
		t.Fatalf("new link not active: %+v", l)
	}
	return l
}

func signIn(t *testing.T, a *testAPI, name, pw string) string {
	t.Helper()
	rr := a.must(http.StatusOK, "POST", "/v1/login", "", strings.NewReader(fmt.Sprintf(`{"username":%q,"password":%q}`, name, pw)))
	var out struct{ Token string }
	json.Unmarshal(rr.Body.Bytes(), &out)
	return out.Token
}

// A link acts for its creator: it stops when they are deleted or lose
// access, and a new user with the same name doesn't bring it back.
func TestLinksFollowCreatorAccess(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/docs/a.txt", a.admin, strings.NewReader("hello"))

	t.Run("API key deleted", func(t *testing.T) {
		tok, rec, err := a.auth.CreateKey(context.Background(), "sharer", []string{"docs:r"})
		if err != nil {
			t.Fatal(err)
		}
		l := createShareLink(t, a, tok, "docs", "a.txt")
		a.must(http.StatusOK, "GET", linkPath(t, l), "", nil)
		a.must(http.StatusNoContent, "DELETE", "/v1/keys/"+rec.ID, a.admin, nil)
		rr := a.must(http.StatusNotFound, "GET", linkPath(t, l), "", nil)
		if code := errCode(t, rr); code != CodeNoSuchLink {
			t.Errorf("code = %s", code)
		}
		var list struct{ Links []linkJSON }
		json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/links?bucket=docs&key=a.txt", a.admin, nil).Body.Bytes(), &list)
		if len(list.Links) != 1 || list.Links[0].Active {
			t.Errorf("listed links = %+v, want one inactive", list.Links)
		}
	})

	t.Run("user loses access, gets it back, then is replaced", func(t *testing.T) {
		a.must(http.StatusCreated, "POST", "/v1/users", a.admin,
			strings.NewReader(`{"name":"ivy","password":"ivys password","scopes":["docs:r"]}`))
		l := createShareLink(t, a, signIn(t, a, "ivy", "ivys password"), "docs", "a.txt")
		p := linkPath(t, l)
		a.must(http.StatusOK, "GET", p, "", nil)

		a.must(http.StatusOK, "PATCH", "/v1/users/ivy", a.admin, strings.NewReader(`{"scopes":["other:r"]}`))
		a.must(http.StatusNotFound, "GET", p, "", nil)
		a.must(http.StatusOK, "PATCH", "/v1/users/ivy", a.admin, strings.NewReader(`{"scopes":["docs:rw"]}`))
		a.must(http.StatusOK, "GET", p, "", nil)

		a.must(http.StatusNoContent, "DELETE", "/v1/users/ivy", a.admin, nil)
		a.must(http.StatusNotFound, "GET", p, "", nil)
		a.must(http.StatusCreated, "POST", "/v1/users", a.admin,
			strings.NewReader(`{"name":"ivy","password":"another password","scopes":["*:admin"]}`))
		a.must(http.StatusNotFound, "GET", p, "", nil)
	})

	t.Run("upload link creator deleted", func(t *testing.T) {
		tok, rec, err := a.auth.CreateKey(context.Background(), "collector", []string{"docs:w"})
		if err != nil {
			t.Fatal(err)
		}
		l := createUploadLinkJSON(t, a, tok, map[string]any{"bucket": "docs"}, http.StatusCreated)
		if !l.Active {
			t.Fatalf("new upload link not active: %+v", l)
		}
		p := uploadPath(t, l)
		a.must(http.StatusCreated, "PUT", p+"/one.txt", "", strings.NewReader("1"))
		a.must(http.StatusNoContent, "DELETE", "/v1/keys/"+rec.ID, a.admin, nil)
		a.must(http.StatusNotFound, "GET", p+"/info", "", nil)
		a.must(http.StatusNotFound, "PUT", p+"/two.txt", "", strings.NewReader("2"))
	})
}

// withLog rebuilds a's handler so its log lines land in the returned buffer.
func withLog(a *testAPI) *lockedBuffer {
	buf := &lockedBuffer{}
	a.h = New(Options{
		Logger:        slog.New(slog.NewTextHandler(buf, nil)),
		MaxObjectSize: 1 << 20,
		Store:         a.eng, Buckets: a.eng, Auth: a.auth, Links: a.eng,
		Trash: a.eng, UploadLinks: a.eng, Webhooks: a.eng,
		PublicURL: "https://files.example.com",
	})
	return buf
}

func TestLogsHoldNoSecrets(t *testing.T) {
	a := newTestAPI(t)
	log := withLog(a)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/docs", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/docs/a.txt", a.admin, strings.NewReader("hello"))
	share := createShareLink(t, a, a.admin, "docs", "a.txt")
	up := createUploadLinkJSON(t, a, a.admin, map[string]any{"bucket": "docs"}, http.StatusCreated)
	a.must(http.StatusOK, "GET", linkPath(t, share), "", nil)
	a.must(http.StatusOK, "GET", uploadPath(t, up)+"/info", "", nil)
	a.must(http.StatusCreated, "PUT", uploadPath(t, up)+"/x.txt", "", strings.NewReader("x"))
	a.must(http.StatusCreated, "POST", "/v1/webhooks", a.admin,
		strings.NewReader(`{"bucket":"docs","url":"https://hooks.example.com/services/T0/B0/s3cr3tpath?token=abc","events":["object.created"]}`))
	a.must(http.StatusUnauthorized, "POST", "/v1/login", "",
		strings.NewReader(`{"username":"mypassword123","password":"x"}`))

	out := log.String()
	shareTok := strings.Split(linkPath(t, share), "/")[2]
	upTok := strings.Split(uploadPath(t, up), "/")[2]
	for _, secret := range []string{shareTok, upTok, "s3cr3tpath", "token=abc", "mypassword123"} {
		if strings.Contains(out, secret) {
			t.Errorf("log contains %q:\n%s", secret, out)
		}
	}
	for _, want := range []string{"path=/s/<token>/a.txt", "path=/u/<token>/info", "key=link:" + share.ID,
		"host=https://hooks.example.com", `key="login:(unknown name)"`} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
}

func TestLogPath(t *testing.T) {
	for in, want := range map[string]string{
		"/s/abc":          "/s/<token>",
		"/s/abc/file.pdf": "/s/<token>/file.pdf",
		"/u/abc/info":     "/u/<token>/info",
		"/s/":             "/s/",
		"/v1/b/docs/s/x":  "/v1/b/docs/s/x",
		"/ui/":            "/ui/",
	} {
		if got := logPath(in); got != want {
			t.Errorf("logPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// uncertainStore reports every Put as possibly stored (an I/O error during
// the metadata commit), after storing it for real.
type uncertainStore struct{ store.Store }

func (u uncertainStore) Put(ctx context.Context, bucket, key string, r io.Reader, meta store.Meta) (store.ObjectInfo, error) {
	if _, err := u.Store.Put(ctx, bucket, key, r, meta); err != nil {
		return store.ObjectInfo{}, err
	}
	return store.ObjectInfo{}, fmt.Errorf("commit: %w: input/output error", store.ErrCommitUncertain)
}

// An upload whose commit may have reached disk keeps its slot, so a link
// can't hand out more files than maxFiles.
func TestUploadLinkKeepsSlotWhenCommitUncertain(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/inbox", a.admin, nil)
	l := createUploadLinkJSON(t, a, a.admin, map[string]any{"bucket": "inbox", "maxFiles": 1}, http.StatusCreated)
	a.h = New(Options{
		Logger: slog.New(slog.DiscardHandler), MaxObjectSize: 1 << 20,
		Store: uncertainStore{a.eng}, Buckets: a.eng, Auth: a.auth, Links: a.eng,
		Trash: a.eng, UploadLinks: a.eng, Webhooks: a.eng,
	})
	rr := a.must(http.StatusInternalServerError, "PUT", uploadPath(t, l)+"/a.txt", "", strings.NewReader("a"))
	if code := errCode(t, rr); code != CodeCommitUncertain || strings.Contains(rr.Body.String(), "input/output") {
		t.Errorf("response = %s", rr.Body)
	}
	rr = a.must(http.StatusForbidden, "PUT", uploadPath(t, l)+"/b.txt", "", strings.NewReader("b"))
	if code := errCode(t, rr); code != CodeUploadLimit {
		t.Errorf("second upload: %s", rr.Body)
	}
}
