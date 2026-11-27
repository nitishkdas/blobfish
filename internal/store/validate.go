package store

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"unicode/utf8"
)

// ValidateBucketName enforces S3-style names: 3-63 chars of lowercase
// letters, digits, '-' and '.', starting and ending with a letter or digit.
// Staying S3-compatible here keeps the optional S3 module simple later.
func ValidateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("%w: %q must be 3–63 characters", ErrInvalidBucketName, name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		alnum := c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
		if !alnum && c != '-' && c != '.' {
			return fmt.Errorf("%w: %q may only contain a-z, 0-9, '-' and '.'", ErrInvalidBucketName, name)
		}
		if (i == 0 || i == len(name)-1) && !alnum {
			return fmt.Errorf("%w: %q must start and end with a letter or digit", ErrInvalidBucketName, name)
		}
	}
	if strings.Contains(name, "..") {
		return fmt.Errorf("%w: %q must not contain '..'", ErrInvalidBucketName, name)
	}
	return nil
}

// ValidateKey accepts any non-empty valid UTF-8 key up to MaxKeyLen bytes
// without NUL. Keys never become file names, so '/', '..' and other odd
// strings are fine.
func ValidateKey(key string) error {
	switch {
	case key == "":
		return fmt.Errorf("%w: it is empty", ErrInvalidKey)
	case len(key) > MaxKeyLen:
		return fmt.Errorf("%w: it is longer than %d bytes", ErrInvalidKey, MaxKeyLen)
	case !utf8.ValidString(key):
		return fmt.Errorf("%w: it is not valid UTF-8", ErrInvalidKey)
	case strings.IndexByte(key, 0) >= 0:
		return fmt.Errorf("%w: it contains a NUL character", ErrInvalidKey)
	}
	return nil
}

// ValidateMeta checks the size of user metadata.
func ValidateMeta(m Meta) error {
	n := 0
	for k, v := range m.User {
		n += len(k) + len(v)
	}
	if n > MaxUserMetaBytes {
		return fmt.Errorf("%w: %d bytes, the limit is %d", ErrMetadataTooLarge, n, MaxUserMetaBytes)
	}
	return nil
}

// Range selects part of an object.
//
//   - Start >= 0: bytes Start through End inclusive. End < 0, or End past
//     the last byte, means "to the end" (HTTP "bytes=500-").
//   - Start < 0: the last -Start bytes (HTTP suffix range "bytes=-500").
type Range struct {
	Start, End int64
}

// Resolve turns r into an offset and length for an object of the given
// size, or returns ErrInvalidRange if no byte of the object is selected.
func (r Range) Resolve(size int64) (off, n int64, err error) {
	if r.Start < 0 {
		// Compare before negating: -math.MinInt64 overflows to itself.
		n = size
		if r.Start > -size {
			n = -r.Start
		}
		if n == 0 {
			return 0, 0, fmt.Errorf("%w: suffix range on empty object", ErrInvalidRange)
		}
		return size - n, n, nil
	}
	if r.Start >= size {
		return 0, 0, fmt.Errorf("%w: start %d, size %d", ErrInvalidRange, r.Start, size)
	}
	end := r.End
	if end < 0 || end >= size {
		end = size - 1
	}
	if end < r.Start {
		return 0, 0, fmt.Errorf("%w: end %d before start %d", ErrInvalidRange, r.End, r.Start)
	}
	return r.Start, end - r.Start + 1, nil
}

// MaxCORSOrigins caps the sites allowed per bucket.
const MaxCORSOrigins = 20

// NormalizeCORSOrigins validates web origins and returns them lower-cased,
// de-duplicated and sorted. Each is "*" or exactly scheme://host[:port],
// which is what browsers send in the Origin header: no path, no slash.
func NormalizeCORSOrigins(in []string) ([]string, error) {
	out := []string{}
	for _, o := range in {
		o = strings.ToLower(strings.TrimSpace(o))
		if o == "" {
			continue
		}
		if o != "*" {
			u, err := url.Parse(o)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
				u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || u.Opaque != "" {
				return nil, fmt.Errorf("%w: %q must look like https://app.example.com (no path or trailing slash), or *", ErrInvalidOrigin, o)
			}
		}
		if !slices.Contains(out, o) {
			out = append(out, o)
		}
	}
	if len(out) > MaxCORSOrigins {
		return nil, fmt.Errorf("%w: at most %d origins per bucket", ErrInvalidOrigin, MaxCORSOrigins)
	}
	slices.Sort(out)
	return out, nil
}
