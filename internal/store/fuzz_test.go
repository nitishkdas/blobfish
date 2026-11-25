package store

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func FuzzRangeResolve(f *testing.F) {
	f.Add(int64(0), int64(-1), int64(10))
	f.Add(int64(-3), int64(0), int64(10))
	f.Add(int64(5), int64(2), int64(10))
	f.Add(int64(0), int64(0), int64(0))
	f.Add(int64(-1<<63), int64(1<<63-1), int64(1<<62))
	f.Fuzz(func(t *testing.T, start, end, size int64) {
		if size < 0 {
			return // sizes come from the engine and are never negative
		}
		off, n, err := Range{Start: start, End: end}.Resolve(size)
		if err != nil {
			return
		}
		if off < 0 || n < 1 || off > size || n > size-off {
			t.Fatalf("Resolve(%d,%d over %d) = off %d n %d: outside the object", start, end, size, off, n)
		}
	})
}

func FuzzValidateKey(f *testing.F) {
	for _, s := range []string{"a", "a/b", "../x", "", "\x00", "\xff", strings.Repeat("k", MaxKeyLen+1)} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, key string) {
		if ValidateKey(key) != nil {
			return
		}
		if key == "" || len(key) > MaxKeyLen || !utf8.ValidString(key) || strings.Contains(key, "\x00") {
			t.Fatalf("accepted bad key %q", key)
		}
	})
}

func FuzzValidateBucketName(f *testing.F) {
	for _, s := range []string{"abc", "a..b", "-ab", "ab", "UPPER", "a.b-c"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, name string) {
		if ValidateBucketName(name) != nil {
			return
		}
		// Accepted names must be safe in URLs, scopes ("bucket:perm") and
		// file systems, so the S3 module and the UI can use them verbatim.
		for _, c := range name {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '.') {
				t.Fatalf("accepted %q with char %q", name, c)
			}
		}
		if name == "." || name == ".." || strings.Contains(name, "..") {
			t.Fatalf("accepted dot name %q", name)
		}
	})
}
