package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/engine"
)

func trashList(t *testing.T, a *testAPI, token string) trashListJSON {
	t.Helper()
	var l trashListJSON
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/trash/bin", token, nil).Body.Bytes(), &l)
	return l
}

func TestTrashAPI(t *testing.T) {
	a := newTestAPIWith(t, engine.Options{TrashRetention: 7 * 24 * time.Hour})
	a.must(http.StatusCreated, "PUT", "/v1/buckets/bin", a.admin, nil)
	writer := a.key("bin:rw")
	reader := a.key("bin:r")
	outsider := a.key("other:rw")
	a.must(http.StatusOK, "PUT", "/v1/b/bin/docs/a.txt", writer, strings.NewReader("hello"), "Content-Type", "text/plain")
	link := createLinkJSON(t, a, reader, "bin", "docs/a.txt", 0, http.StatusCreated)

	var who whoamiResponse
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/whoami", reader, nil).Body.Bytes(), &who)
	if who.TrashDays != 7 {
		t.Errorf("whoami trashDays = %d", who.TrashDays)
	}

	a.must(http.StatusNoContent, "DELETE", "/v1/b/bin/docs/a.txt", writer, nil)
	a.must(http.StatusNotFound, "GET", "/v1/b/bin/docs/a.txt", writer, nil)
	if c := errCode(t, a.must(http.StatusNotFound, "GET", linkPath(t, link), "", nil)); c != CodeNoSuchLink {
		t.Errorf("link while in trash: %s", c)
	}

	// Trash is for those who may delete: readers and outsiders see nothing.
	a.must(http.StatusForbidden, "GET", "/v1/trash/bin", reader, nil)
	a.must(http.StatusForbidden, "GET", "/v1/trash/bin", outsider, nil)
	a.must(http.StatusUnauthorized, "GET", "/v1/trash/bin", "", nil)
	a.must(http.StatusNotFound, "GET", "/v1/trash/no-such", a.admin, nil)

	l := trashList(t, a, writer)
	if len(l.Items) != 1 || l.Items[0].Object.Key != "docs/a.txt" || l.Items[0].Object.Size != 5 ||
		l.Items[0].PurgeAt.Sub(l.Items[0].Deleted) != 7*24*time.Hour {
		t.Fatalf("trash = %+v", l)
	}
	id := l.Items[0].ID

	// Restore is refused while a new object holds the key.
	a.must(http.StatusOK, "PUT", "/v1/b/bin/docs/a.txt", writer, strings.NewReader("new"))
	if c := errCode(t, a.must(http.StatusConflict, "POST", "/v1/trash/bin/"+id+"/restore", writer, nil)); c != CodeKeyExists {
		t.Errorf("restore over live object: %s", c)
	}
	a.must(http.StatusForbidden, "POST", "/v1/trash/bin/"+id+"/restore", reader, nil)
	a.must(http.StatusNoContent, "DELETE", "/v1/b/bin/docs/a.txt", writer, nil) // "new" to trash too

	rr := a.must(http.StatusOK, "POST", "/v1/trash/bin/"+id+"/restore", writer, nil)
	var obj objectJSON
	json.Unmarshal(rr.Body.Bytes(), &obj)
	if obj.Key != "docs/a.txt" || obj.Size != 5 {
		t.Errorf("restored = %+v", obj)
	}
	if body := a.must(http.StatusOK, "GET", "/v1/b/bin/docs/a.txt", writer, nil).Body.String(); body != "hello" {
		t.Errorf("restored body %q", body)
	}
	a.must(http.StatusOK, "GET", linkPath(t, link), "", nil)
	if c := errCode(t, a.must(http.StatusNotFound, "POST", "/v1/trash/bin/"+id+"/restore", writer, nil)); c != CodeNoSuchTrashItem {
		t.Errorf("restore twice: %s", c)
	}
	a.must(http.StatusNotFound, "POST", "/v1/trash/bin/not-an-id/restore", writer, nil)

	// A bucket with trash can't be deleted until the trash is emptied.
	a.must(http.StatusNoContent, "DELETE", "/v1/b/bin/docs/a.txt", writer, nil)
	if l := trashList(t, a, writer); len(l.Items) != 2 {
		t.Fatalf("trash has %d items", len(l.Items))
	}
	other := trashList(t, a, writer).Items[1].ID
	a.must(http.StatusNoContent, "DELETE", "/v1/trash/bin/"+other, writer, nil)
	a.must(http.StatusNotFound, "DELETE", "/v1/trash/bin/"+other, writer, nil)
	rr = a.must(http.StatusConflict, "DELETE", "/v1/buckets/bin", a.admin, nil)
	if !strings.Contains(rr.Body.String(), "trash") {
		t.Errorf("bucket delete message: %s", rr.Body)
	}
	a.must(http.StatusForbidden, "DELETE", "/v1/trash/bin", reader, nil)
	a.must(http.StatusNoContent, "DELETE", "/v1/trash/bin", writer, nil)
	if l := trashList(t, a, writer); len(l.Items) != 0 {
		t.Errorf("trash after emptying: %+v", l)
	}
	a.must(http.StatusNoContent, "DELETE", "/v1/buckets/bin", a.admin, nil)
}

func TestTrashOffDeletesAtOnce(t *testing.T) {
	a := newTestAPI(t)
	a.must(http.StatusCreated, "PUT", "/v1/buckets/bin", a.admin, nil)
	a.must(http.StatusOK, "PUT", "/v1/b/bin/k", a.admin, strings.NewReader("x"))
	a.must(http.StatusNoContent, "DELETE", "/v1/b/bin/k", a.admin, nil)
	if l := trashList(t, a, a.admin); len(l.Items) != 0 {
		t.Errorf("trash with trash off: %+v", l)
	}
	var who whoamiResponse
	json.Unmarshal(a.must(http.StatusOK, "GET", "/v1/whoami", a.admin, nil).Body.Bytes(), &who)
	if who.TrashDays != 0 {
		t.Errorf("trashDays = %d", who.TrashDays)
	}
}
