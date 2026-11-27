package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// Stable API error codes. Clients may switch on these, so never rename one.
const (
	CodeNotFound           = "NotFound"
	CodeMethodNotAllowed   = "MethodNotAllowed"
	CodeInvalidRequest     = "InvalidRequest"
	CodeEntityTooLarge     = "EntityTooLarge"
	CodeInternalError      = "InternalError"
	CodeUnauthorized       = "Unauthorized"
	CodeAccessDenied       = "AccessDenied"
	CodeSignatureInvalid   = "SignatureInvalid"
	CodeSignatureExpired   = "SignatureExpired"
	CodeNoSuchBucket       = "NoSuchBucket"
	CodeNoSuchKey          = "NoSuchKey"
	CodeBucketExists       = "BucketAlreadyExists"
	CodeBucketNotEmpty     = "BucketNotEmpty"
	CodeInvalidBucketName  = "InvalidBucketName"
	CodeInvalidKey         = "InvalidKey"
	CodeInvalidRange       = "InvalidRange"
	CodeInvalidCursor      = "InvalidCursor"
	CodeMetadataTooLarge   = "MetadataTooLarge"
	CodeDataCorrupt        = "DataCorrupt"
	CodeNoSuchAPIKey       = "NoSuchAPIKey"
	CodeInvalidScope       = "InvalidScope"
	CodeLastAdminKey       = "LastAdminKey"
	CodeIncompleteBody     = "IncompleteBody"
	CodeNoSuchLink         = "NoSuchLink"
	CodeLinkExpired        = "LinkExpired"
	CodeRequestTimeout     = "RequestTimeout"
	CodeInsufficientSpace  = "InsufficientStorage"
	CodeCORSNotAllowed     = "CORSNotAllowed"
	CodeInvalidOrigin      = "InvalidOrigin"
	CodeNoSuchTrashItem    = "NoSuchTrashItem"
	CodeKeyExists          = "KeyExists"
	CodeQuotaExceeded      = "QuotaExceeded"
	CodeUploadLimit        = "UploadLimitReached"
	CodePreconditionFailed = "PreconditionFailed"
	CodeNoSuchWebhook      = "NoSuchWebhook"
	CodeTooManyWebhooks    = "TooManyWebhooks"
	CodeInvalidCredentials = "InvalidCredentials"
	CodeTooManyAttempts    = "TooManyAttempts"
	CodeNoSuchUser         = "NoSuchUser"
	CodeUserExists         = "UserAlreadyExists"
	CodeInvalidPassword    = "InvalidPassword"
	CodeInvalidUserName    = "InvalidUserName"
	CodeInvalidEmail       = "InvalidEmail"
	CodeEmailTaken         = "EmailTaken"
	CodeNotSession         = "NotSignedInAsUser"
	CodeCommitUncertain    = "CommitUncertain"
)

// errorMapping lists, in order, how internal errors become API errors. This
// table is the one place that mapping happens. msg is the message's lead,
// written for people; classify adds the error's detail after a colon
// ("Invalid username: use 2–64 …"). A row with fixed set never shows the
// detail (it is internal, or could mislead).
var errorMapping = []struct {
	err    error
	status int
	code   string
	msg    string
	fixed  bool
}{
	// First: it wraps the underlying cause (maybe ENOSPC), but what the
	// client must know is that the write may have happened anyway.
	{store.ErrCommitUncertain, http.StatusInternalServerError, CodeCommitUncertain,
		"The write may or may not have been stored. Check with a HEAD request before retrying", true},
	{store.ErrNoSuchBucket, http.StatusNotFound, CodeNoSuchBucket, "Bucket not found", false},
	{store.ErrNoSuchKey, http.StatusNotFound, CodeNoSuchKey, "Object not found", false},
	{store.ErrBucketExists, http.StatusConflict, CodeBucketExists, "A bucket with this name already exists", false},
	{store.ErrBucketNotEmpty, http.StatusConflict, CodeBucketNotEmpty, "The bucket is not empty", false},
	{store.ErrInvalidBucketName, http.StatusBadRequest, CodeInvalidBucketName, "Invalid bucket name", false},
	{store.ErrInvalidKey, http.StatusBadRequest, CodeInvalidKey, "Invalid object key", false},
	{store.ErrInvalidRange, http.StatusRequestedRangeNotSatisfiable, CodeInvalidRange, "The requested range can't be satisfied", false},
	{store.ErrInvalidCursor, http.StatusBadRequest, CodeInvalidCursor, "Invalid list cursor", false},
	{store.ErrMetadataTooLarge, http.StatusBadRequest, CodeMetadataTooLarge, "Metadata is too large", false},
	{store.ErrDataCorrupt, http.StatusInternalServerError, CodeDataCorrupt,
		"The stored data failed its integrity check. Ask the server administrator to run blobfish verify", true},
	{store.ErrInsufficientStorage, http.StatusInsufficientStorage, CodeInsufficientSpace, "The server does not have enough free disk space", false},
	{syscall.ENOSPC, http.StatusInsufficientStorage, CodeInsufficientSpace, "The server does not have enough free disk space", true}, // reserve disabled or raced
	{store.ErrNoSuchAPIKey, http.StatusNotFound, CodeNoSuchAPIKey, "API key not found", false},
	{store.ErrNoSuchLink, http.StatusNotFound, CodeNoSuchLink, "This link does not exist or no longer works", true},
	{store.ErrInvalidOrigin, http.StatusBadRequest, CodeInvalidOrigin, "Invalid website origin", false},
	{store.ErrNoSuchTrashItem, http.StatusNotFound, CodeNoSuchTrashItem, "Item not found in trash", false},
	{store.ErrKeyExists, http.StatusConflict, CodeKeyExists, "An object with this key already exists", false},
	// 507 like a full disk, but a distinct code: the bucket is full, not
	// the server. Deleting from the bucket makes room at once.
	{store.ErrQuotaExceeded, http.StatusInsufficientStorage, CodeQuotaExceeded, "The upload would exceed the bucket's quota", true},
	{store.ErrInvalidQuota, http.StatusBadRequest, CodeInvalidRequest, "Invalid quota", false},
	{store.ErrUploadLimitReached, http.StatusForbidden, CodeUploadLimit, "This upload link has no uploads left", true},
	{store.ErrNoSuchWebhook, http.StatusNotFound, CodeNoSuchWebhook, "Webhook not found", false},
	{store.ErrTooManyWebhooks, http.StatusConflict, CodeTooManyWebhooks, "Too many webhooks", false},
	{errLinkExpired, http.StatusGone, CodeLinkExpired, "This link has expired", true},
	{auth.ErrUnauthenticated, http.StatusUnauthorized, CodeUnauthorized, "Missing or invalid API key or session token", true},
	{auth.ErrSignatureInvalid, http.StatusForbidden, CodeSignatureInvalid, "The presigned URL's signature is invalid", true},
	{auth.ErrSignatureExpired, http.StatusForbidden, CodeSignatureExpired, "The presigned URL has expired", true},
	{auth.ErrInvalidScope, http.StatusBadRequest, CodeInvalidScope, "Invalid scope", false},
	{auth.ErrInvalidKeyName, http.StatusBadRequest, CodeInvalidRequest, "Invalid API key name", false},
	{auth.ErrInvalidPresign, http.StatusBadRequest, CodeInvalidRequest, "Invalid presign request", false},
	{auth.ErrLastAdminKey, http.StatusConflict, CodeLastAdminKey,
		"This is the last admin key. Create another admin key before revoking it", true},
	{auth.ErrBadCredentials, http.StatusUnauthorized, CodeInvalidCredentials, "The username, email or password is incorrect", true},
	{auth.ErrTooManyAttempts, http.StatusTooManyRequests, CodeTooManyAttempts, "Too many failed sign-in attempts. Try again later", true},
	{auth.ErrInvalidPassword, http.StatusBadRequest, CodeInvalidPassword, "Invalid password", false},
	{auth.ErrInvalidUserName, http.StatusBadRequest, CodeInvalidUserName, "Invalid username", false},
	{auth.ErrInvalidEmail, http.StatusBadRequest, CodeInvalidEmail, "Invalid email address", false},
	{auth.ErrNotSession, http.StatusBadRequest, CodeNotSession, "This request needs a signed-in user, not an API key", true},
	{store.ErrNoSuchUser, http.StatusNotFound, CodeNoSuchUser, "User not found", false},
	{store.ErrUserExists, http.StatusConflict, CodeUserExists, "A user with this username already exists", false},
	{store.ErrEmailTaken, http.StatusConflict, CodeEmailTaken, "Another user already has this email address", true},
	{errAccessDenied, http.StatusForbidden, CodeAccessDenied, "Access denied", false},
	{errBadRequest, http.StatusBadRequest, CodeInvalidRequest, "", false},
	{context.Canceled, http.StatusBadRequest, CodeInvalidRequest, "The request was cancelled", true},
	// The client hung up or cancelled mid-upload. The engine has already
	// discarded the partial data; this is not a server failure.
	{io.ErrUnexpectedEOF, http.StatusBadRequest, CodeIncompleteBody, "The request body ended early. Nothing was stored", true},
	{syscall.ECONNRESET, http.StatusBadRequest, CodeIncompleteBody, "The request body ended early. Nothing was stored", true},
	// The client stopped sending mid-upload for longer than the transfer
	// idle timeout. Its fault, not ours: no ERROR log line.
	{os.ErrDeadlineExceeded, http.StatusRequestTimeout, CodeRequestTimeout, "The upload made no progress for too long and was stopped", true},
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
		return http.StatusRequestEntityTooLarge, CodeEntityTooLarge, tooLargeMessage(tooBig.Limit)
	}
	for _, m := range errorMapping {
		if errors.Is(err, m.err) {
			detail := ""
			if !m.fixed {
				detail = detailOf(err, m.err)
			}
			return m.status, m.code, sentence(m.msg, detail)
		}
	}
	return http.StatusInternalServerError, CodeInternalError, internalErrorMessage
}

const internalErrorMessage = "Internal server error. Try again later, or give the administrator the request ID."

func tooLargeMessage(limit int64) string {
	return fmt.Sprintf("The request body is larger than the %d-byte limit.", limit)
}

// detailOf returns what err adds after sentinel's own text
// ("invalid user name: use 2-64 …" -> "use 2-64 …"), or "". Context added
// in front of the sentinel ("start session: …") is internal and dropped.
func detailOf(err, sentinel error) string {
	s, t := err.Error(), sentinel.Error()+": "
	if i := strings.Index(s, t); i >= 0 {
		return s[i+len(t):]
	}
	return ""
}

// sentence joins a lead and a detail clause into one message ending in a
// full stop: "Invalid username: use …." With no lead the detail stands
// alone, capitalized.
func sentence(lead, detail string) string {
	msg := lead
	switch {
	case lead == "" && detail == "":
		msg = "The request is invalid"
	case lead == "":
		msg = strings.ToUpper(detail[:1]) + detail[1:]
	case detail != "":
		msg = lead + ": " + detail
	}
	if !strings.HasSuffix(msg, ".") && !strings.HasSuffix(msg, "?") {
		msg += "."
	}
	return msg
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
