package server

import (
	"encoding/json"
	"net/http"
)

// Stable API error codes. Clients may switch on these, so never rename one.
// Internal errors are mapped to these codes only in this file.
const (
	CodeNotFound       = "NotFound"
	CodeEntityTooLarge = "EntityTooLarge"
	CodeInternalError  = "InternalError"
)

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
	_ = json.NewEncoder(w).Encode(v)
}
