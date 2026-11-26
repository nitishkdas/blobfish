package server

import (
	"mime"
	"net/http"
	"path"
	"strings"
	"unicode"
	"unicode/utf8"
)

// maxFilenameLen caps a download name in bytes; most file systems allow 255.
const maxFilenameLen = 255

// validateFilename checks a name a browser will save a download as. It is
// a single path element: no slashes, no control characters.
func validateFilename(name string) error {
	switch {
	case name == "":
		return badRequest("filename is empty")
	case len(name) > maxFilenameLen:
		return badRequest("filename is longer than %d bytes", maxFilenameLen)
	case !utf8.ValidString(name):
		return badRequest("filename is not valid UTF-8")
	case name == "." || name == "..":
		return badRequest("filename %q is not a file name", name)
	case strings.TrimSpace(name) != name:
		return badRequest("filename must not start or end with spaces")
	case strings.ContainsAny(name, `/\`):
		return badRequest("filename must not contain / or \\")
	case strings.ContainsFunc(name, unicode.IsControl):
		return badRequest("filename must not contain control characters")
	}
	return nil
}

// defaultFilename is the last part of a key, the name a file shows under
// in folder listings.
func defaultFilename(key string) string {
	name := path.Base(key)
	if validateFilename(name) != nil {
		return "download"
	}
	return name
}

// contentDisposition decides how a browser should treat an object
// response. With ?download (or ?download=name) the file is saved
// ("attachment"); otherwise it is shown in place ("inline"). name is used
// when ?download gives none.
//
// The signature of a presigned URL does not cover ?download. It only
// changes a response header, never which object is read or who may.
func contentDisposition(r *http.Request, name string) (string, error) {
	kind := "inline"
	q := r.URL.Query()
	if q.Has("download") {
		kind = "attachment"
		if v := q.Get("download"); v != "" {
			if err := validateFilename(v); err != nil {
				return "", err
			}
			name = v
		}
	}
	// FormatMediaType switches to RFC 2231 (filename*=utf-8''...) for
	// non-ASCII names.
	if d := mime.FormatMediaType(kind, map[string]string{"filename": name}); d != "" {
		return d, nil
	}
	return kind, nil
}
