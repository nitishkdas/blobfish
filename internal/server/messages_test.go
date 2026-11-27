package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"unicode"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Error messages are written for people: a capital, a full stop, the
// detail after a colon, and nothing internal.
func TestErrorMessagesAreSentences(t *testing.T) {
	for _, m := range errorMapping {
		for _, err := range []error{m.err, fmt.Errorf("context: %w: some detail", m.err)} {
			_, _, msg := classify(err)
			first := []rune(msg)[0]
			if !unicode.IsUpper(first) || !strings.HasSuffix(msg, ".") {
				t.Errorf("%s: message %q is not a sentence", m.code, msg)
			}
			if strings.Contains(msg, "context:") {
				t.Errorf("%s: internal context leaked: %q", m.code, msg)
			}
		}
	}
	for _, tt := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%w: use at least 10 characters", auth.ErrInvalidPassword), "Invalid password: use at least 10 characters."},
		{fmt.Errorf("create: %w: docs", store.ErrNoSuchBucket), "Bucket not found: docs."},
		{badRequest("the filename is empty"), "The filename is empty."},
		{fmt.Errorf("commit: %w: input/output error", store.ErrCommitUncertain), "The write may or may not have been stored. Check with a HEAD request before retrying."},
		{errors.New("open /var/lib/blobfish/meta.db: input/output error"), internalErrorMessage},
	} {
		if _, _, got := classify(tt.err); got != tt.want {
			t.Errorf("classify(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestJSONErrorsNameTheField(t *testing.T) {
	a := newTestAPI(t)
	for _, tt := range []struct{ body, want string }{
		{`{"name":"x","password":"long enough pw","scopes":"docs:r"}`, `The field "scopes" must be a list of strings.`},
		{`{"name":5,"password":"long enough pw","scopes":["docs:r"]}`, `The field "name" must be a string.`},
		{`{"name":"x",`, `The request body is not valid JSON.`},
		{``, `The request body is empty; send a JSON object.`},
		{`{"nmae":"x"}`, `Unknown field "nmae".`},
		{`["x"]`, `The request body must be a JSON object.`},
		{`{} {}`, `The request body must hold one JSON object and nothing after it.`},
	} {
		rr := a.must(http.StatusBadRequest, "POST", "/v1/users", a.admin, strings.NewReader(tt.body))
		var e struct{ Error struct{ Message string } }
		json.Unmarshal(rr.Body.Bytes(), &e)
		if e.Error.Message != tt.want {
			t.Errorf("body %q: message %q, want %q", tt.body, e.Error.Message, tt.want)
		}
	}
}
