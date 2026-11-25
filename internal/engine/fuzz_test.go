package engine

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

// FuzzList compares paginated List output with a simple in-memory model
// for arbitrary key sets, prefixes, delimiters and page sizes.
func FuzzList(f *testing.F) {
	f.Add("a\nb/1\nb/2\nb/c/3\nz", "", "/", uint8(1))
	f.Add("x/y/z\nx/y\nx\nx//y", "x", "/", uint8(2))
	f.Add("aaa\naab\nab\nb", "a", "a", uint8(1))
	f.Add("ü/1\nü/2\nu", "", "/", uint8(3))
	f.Add("k\xff\nk", "k", "", uint8(1))

	e := openTest(f, f.TempDir())
	n := 0
	f.Fuzz(func(t *testing.T, keyList, prefix, delim string, limit uint8) {
		var keys []string
		for _, k := range strings.Split(keyList, "\n") {
			if store.ValidateKey(k) == nil && !slices.Contains(keys, k) {
				keys = append(keys, k)
			}
		}
		if len(keys) > 30 {
			keys = keys[:30] // each Put fsyncs; keep executions fast
		}
		ctx := context.Background()
		n++
		bucket := fmt.Sprintf("fuzz-%d", n)
		if _, err := e.CreateBucket(ctx, bucket); err != nil {
			t.Fatal(err)
		}
		for _, k := range keys {
			if _, err := e.Put(ctx, bucket, k, strings.NewReader(k), store.Meta{}); err != nil {
				t.Fatal(err)
			}
		}

		// Model: sort, filter by prefix, roll up at the first delimiter.
		slices.Sort(keys)
		var want []string
		for _, k := range keys {
			if !strings.HasPrefix(k, prefix) {
				continue
			}
			if delim != "" {
				if i := strings.Index(k[len(prefix):], delim); i >= 0 {
					cp := k[:len(prefix)+i+len(delim)]
					if !slices.Contains(want, "P:"+cp) {
						want = append(want, "P:"+cp)
					}
					continue
				}
			}
			want = append(want, "O:"+k)
		}

		page := int(limit%8) + 1
		var got []string
		cursor := ""
		for range len(keys) + 2 {
			res, err := e.List(ctx, bucket, store.ListOptions{Prefix: prefix, Delimiter: delim, Limit: page, Cursor: cursor})
			if err != nil {
				t.Fatalf("List: %v", err)
			}
			if len(res.Objects)+len(res.CommonPrefixes) > page {
				t.Fatalf("page over limit")
			}
			for _, o := range res.Objects {
				got = append(got, "O:"+o.Key)
			}
			for _, p := range res.CommonPrefixes {
				got = append(got, "P:"+p)
			}
			if res.NextCursor == "" {
				break
			}
			cursor = res.NextCursor
		}
		slices.Sort(got)
		slices.Sort(want)
		if !slices.Equal(got, want) {
			t.Fatalf("prefix %q delim %q page %d\n got  %q\n want %q", prefix, delim, page, got, want)
		}

		// Every listed object reads back as written.
		for _, k := range keys {
			rc, _, err := e.Get(ctx, bucket, k, nil)
			if err != nil {
				t.Fatalf("Get %q: %v", k, err)
			}
			b, err := io.ReadAll(rc)
			rc.Close()
			if err != nil || string(b) != k {
				t.Fatalf("Get %q = %q, %v", k, b, err)
			}
		}
	})
}
