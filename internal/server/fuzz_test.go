package server

import (
	"testing"
)

func FuzzParseRange(f *testing.F) {
	for _, s := range []string{"bytes=0-1", "bytes=5-", "bytes=-3", "bytes=-0", "bytes=1-0", "bytes=0-1,2-3",
		"bytes= 1 - 2", "bytes=--1", "bytes=9223372036854775807-", "bytes=-9223372036854775808", "items=1-2"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, h string) {
		rng, unsat := parseRange(h)
		if rng != nil && unsat {
			t.Fatalf("%q: both a range and unsatisfiable", h)
		}
		if rng == nil {
			return
		}
		if rng.Start < 0 && rng.End != 0 {
			t.Fatalf("%q: suffix range with End %d", h, rng.End)
		}
		if rng.Start >= 0 && rng.End != -1 && rng.End < rng.Start {
			t.Fatalf("%q: End %d before Start %d", h, rng.End, rng.Start)
		}
		// Whatever was parsed must resolve safely against any object size.
		for _, size := range []int64{0, 1, 10, 1 << 40} {
			off, n, err := rng.Resolve(size)
			if err == nil && (off < 0 || n < 1 || off+n > size) {
				t.Fatalf("%q over %d: off %d n %d", h, size, off, n)
			}
		}
	})
}

func FuzzETagMatches(f *testing.F) {
	f.Add(`"abc"`, "abc")
	f.Add(`W/"abc", "def"`, "def")
	f.Add(`*`, "x")
	f.Add(`"abc`, "abc")
	f.Fuzz(func(t *testing.T, header, etag string) {
		_ = etagMatches(header, etag) // must not panic
		if header == "" && etagMatches(header, etag) {
			t.Fatal("empty If-None-Match matched")
		}
	})
}
