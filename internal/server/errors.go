package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"syscall"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Stable API error codes. Clients may switch on these, so never rename one.
const (
	CodeNotFound          = "NotFound"
	CodeMethodNotAllowed  = "MethodNotAllowed"
	CodeInvalidRequest    = "InvalidRequest"
	CodeEntityTooLarge    = "EntityTooLarge"
	CodeInternalError     = "InternalError"
	CodeUnauthorized      = "Unauthorized"
	CodeAccessDenied      = "AccessDenied"
	CodeSignatureInvalid  = "SignatureInvalid"
	CodeSignatureExpired  = "SignatureExpired"
	CodeNoSuchBucket      = "NoSuchBucket"
	CodeNoSuchKey         = "NoSuchKey"
	CodeBucketExists      = "BucketAlreadyExists"
	CodeBucketNotEmpty    = "BucketNotEmpty"
	CodeInvalidBucketName = "InvalidBucketName"
	CodeInvalidKey        = "InvalidKey"
	CodeInvalidRange      = "InvalidRange"
	CodeInvalidCursor     = "InvalidCursor"
	CodeMetadataTooLarge  = "MetadataTooLarge"
	CodeDataCorrupt       = "DataCorrupt"
	CodeNoSuchAPIKey      = "NoSuchAPIKey"
	CodeInvalidScope      = "InvalidScope"
	CodeLastAdminKey      = "LastAdminKey"
	CodeIncompleteBody    = "IncompleteBody"
	CodeNoSuchLink        = "NoSuchLink"
	CodeLinkExpired       = "LinkExpired"
	CodeRequestTimeout    = "RequestTimeout"
)

// errorMapping lists, in order, how internal errors become API errors. This
// table is the one place that mapping happens.
var errorMapping = []struct {
	err    error
	status int
	code   string
}{
	{store.ErrNoSuchBucket, http.StatusNotFound, CodeNoSuchBucket},
	{store.ErrNoSuchKey, http.StatusNotFound, CodeNoSuchKey},
	{store.ErrBucketExists, http.StatusConflict, CodeBucketExists},
	{store.ErrBucketNotEmpty, http.StatusConflict, CodeBucketNotEmpty},
	{store.ErrInvalidBucketName, http.StatusBadRequest, CodeInvalidBucketName},
	{store.ErrInvalidKey, http.StatusBadRequest, CodeInvalidKey},
	{store.ErrInvalidRange, http.StatusRequestedRangeNotSatisfiable, CodeInvalidRange},
	{store.ErrInvalidCursor, http.StatusBadRequest, CodeInvalidCursor},
	{store.ErrMetadataTooLarge, http.StatusBadRequest, CodeMetadataTooLarge},
	{store.ErrDataCorrupt, http.StatusInternalServerError, CodeDataCorrupt},
	{store.ErrNoSuchAPIKey, http.StatusNotFound, CodeNoSuchAPIKey},
	{store.ErrNoSuchLink, http.StatusNotFound, CodeNoSuchLink},
	{errLinkExpired, http.StatusGone, CodeLinkExpired},
	{auth.ErrUnauthenticated, http.StatusUnauthorized, CodeUnauthorized},
	{auth.ErrSignatureInvalid, http.StatusForbidden, CodeSignatureInvalid},
	{auth.ErrSignatureExpired, http.StatusForbidden, CodeSignatureExpired},
	{auth.ErrInvalidScope, http.StatusBadRequest, CodeInvalidScope},
	{auth.ErrInvalidKeyName, http.StatusBadRequest, CodeInvalidRequest},
	{auth.ErrInvalidPresign, http.StatusBadRequest, CodeInvalidRequest},
	{auth.ErrLastAdminKey, http.StatusConflict, CodeLastAdminKey},
	{errAccessDenied, http.StatusForbidden, CodeAccessDenied},
	{errBadRequest, http.StatusBadRequest, CodeInvalidRequest},
	{context.Canceled, http.StatusBadRequest, CodeInvalidRequest},
	// The client hung up or cancelled mid-upload. The engine has already
	// discarded the partial data; this is not a server failure.
	{io.ErrUnexpectedEOF, http.StatusBadRequest, CodeIncompleteBody},
	{syscall.ECONNRESET, http.StatusBadRequest, CodeIncompleteBody},
	// The client stopped sending mid-upload for longer than the transfer
	// idle timeout. Its fault, not ours: no ERROR log line.
	{os.ErrDeadlineExceeded, http.StatusRequestTimeout, CodeRequestTimeout},
}

var (
	errLinkExpired  = errors.New("share link has expired")
	errAccessDenied = errors.New("access denied")
	errBadRequest   = errors.New("bad request")
)

// badRequest returns an InvalidRequest error with a client-facing message.
func badRequest(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errBadRequest, fmt.Sprintf(format, args...))
}

// classify maps err to an HTTP status, API code and client-facing message.
// Internal failures get a generic message; their detail goes to the log.
func classify(err error) (status int, code, message string) {
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return http.StatusRequestEntityTooLarge, CodeEntityTooLarge,
			fmt.Sprintf("request body exceeds the %d byte limit", tooBig.Limit)
	}
	for _, m := range errorMapping {
		if errors.Is(err, m.err) {
			msg := err.Error()
			if m.status >= 500 && m.code == CodeInternalError {
				msg = "internal server error"
			}
			return m.status, m.code, msg
		}
	}
	return http.StatusInternalServerError, CodeInternalError, "internal server error"
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	RequestID string `json:"requestId,omitempty"`
}

// writeError writes the single BlobFish error shape:
//
//	{"error": {"code": "...", "message": "...", "requestId": "..."}}
func writeError(w http.ResponseWriter, r *http.Request, status int, code, message string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Bearer realm="blobfish"`)
	}
	if f := logFieldsFrom(r); f != nil {
		f.errCode = code
	}
	writeJSON(w, status, errorBody{Error: errorDetail{
		Code:      code,
		Message:   message,
		RequestID: RequestID(r.Context()),
	}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	// The status line is already sent; an encode error here means the client
	// went away, and there is nothing useful left to report to it.
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false) // keep "&" readable in presigned URLs; responses are never HTML
	_ = enc.Encode(v)
}
