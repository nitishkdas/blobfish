package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/nitishkdas/blobfish/internal/store"
)

const testBucket = "test-bucket"

func openTest(t *testing.T, dir string) *Engine {
	t.Helper()
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { e.Close() })
	return e
}

// newEngine opens a fresh engine with one bucket.
func newEngine(t *testing.T) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	e := openTest(t, dir)
	if _, err := e.CreateBucket(context.Background(), testBucket); err != nil {
		t.Fatal(err)
	}
	return e, dir
}

func put(t *testing.T, e *Engine, key, body string) store.ObjectInfo {
	t.Helper()
	info, err := e.Put(context.Background(), testBucket, key, strings.NewReader(body), store.Meta{})
	if err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
	return info
}

func readAll(t *testing.T, e *Engine, key string, rng *store.Range) (string, store.ObjectInfo) {
	t.Helper()
	rc, info, err := e.Get(context.Background(), testBucket, key, rng)
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc) // test-only, small bodies
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	return string(b), info
}

func sha(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// countFiles returns the number of regular files under dir.
func countFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPutGetHead(t *testing.T) {
	tests := []struct {
		name     string
		key      string
		body     string
		meta     store.Meta
		wantType string
	}{
		{"default content type", "a.bin", "hello", store.Meta{}, store.DefaultContentType},
		{"content type and user meta", "docs/inv #1.pdf", "%PDF-1.7",
			store.Meta{ContentType: "application/pdf", User: map[string]string{"owner": "zoro"}}, "application/pdf"},
		{"empty body", "empty", "", store.Meta{}, store.DefaultContentType},
		{"odd key", "../../../etc/passwd", "not a file", store.Meta{}, store.DefaultContentType},
		{"large body", "big", strings.Repeat("blobfish", 200_000), store.Meta{}, store.DefaultContentType},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, dir := newEngine(t)
			ctx := context.Background()
			info, err := e.Put(ctx, testBucket, tt.key, strings.NewReader(tt.body), tt.meta)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size != int64(len(tt.body)) || info.ETag != sha(tt.body) || info.ContentType != tt.wantType {
				t.Errorf("Put info = %+v", info)
			}

			got, ginfo := readAll(t, e, tt.key, nil)
			if got != tt.body {
				t.Errorf("Get body mismatch: %d bytes, want %d", len(got), len(tt.body))
			}
			hinfo, err := e.Head(ctx, testBucket, tt.key)
			if err != nil {
				t.Fatal(err)
			}
			for _, i := range []store.ObjectInfo{ginfo, hinfo} {
				if i.ETag != info.ETag || i.Size != info.Size || i.ContentType != tt.wantType ||
					i.User["owner"] != tt.meta.User["owner"] || !i.Modified.Equal(info.Modified) {
					t.Errorf("info = %+v, want %+v", i, info)
				}
			}
			if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
				t.Errorf("tmp has %d leftover files", n)
			}
		})
	}
}

func TestOverwriteRemovesOldBlob(t *testing.T) {
	e, dir := newEngine(t)
	put(t, e, "k", "version one")
	put(t, e, "k", "version two")
	if got, _ := readAll(t, e, "k", nil); got != "version two" {
		t.Errorf("got %q", got)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 1 {
		t.Errorf("objects/ has %d files, want 1", n)
	}
}

func TestDelete(t *testing.T) {
	e, dir := newEngine(t)
	ctx := context.Background()
	put(t, e, "k", "data")
	for i := range 2 { // second delete proves idempotence
		if err := e.Delete(ctx, testBucket, "k"); err != nil {
			t.Fatalf("Delete #%d: %v", i+1, err)
		}
	}
	if _, err := e.Head(ctx, testBucket, "k"); !errors.Is(err, store.ErrNoSuchKey) {
		t.Errorf("Head after delete: %v, want ErrNoSuchKey", err)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 0 {
		t.Errorf("objects/ has %d files, want 0", n)
	}
}

func TestErrors(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()
	put(t, e, "k", "data")
	body := func() io.Reader { return strings.NewReader("x") }
	getErr := func(bucket, key string, r *store.Range) error {
		rc, _, err := e.Get(ctx, bucket, key, r)
		if rc != nil {
			rc.Close()
		}
		return err
	}
	_, putMissing := e.Put(ctx, "no-such-bucket", "k", body(), store.Meta{})
	_, putBadName := e.Put(ctx, "BAD", "k", body(), store.Meta{})
	_, putBadKey := e.Put(ctx, testBucket, "", body(), store.Meta{})
	_, putBigMeta := e.Put(ctx, testBucket, "k2", body(), store.Meta{User: map[string]string{"x": strings.Repeat("v", 5000)}})
	_, headMissing := e.Head(ctx, testBucket, "nope")
	_, listMissing := e.List(ctx, "no-such-bucket", store.ListOptions{})
	_, listBadCursor := e.List(ctx, testBucket, store.ListOptions{Cursor: "!!"})

	tests := []struct {
		name string
		err  error
		want error
	}{
		{"put missing bucket", putMissing, store.ErrNoSuchBucket},
		{"put bad bucket name", putBadName, store.ErrInvalidBucketName},
		{"put empty key", putBadKey, store.ErrInvalidKey},
		{"put meta too large", putBigMeta, store.ErrMetadataTooLarge},
		{"get missing key", getErr(testBucket, "nope", nil), store.ErrNoSuchKey},
		{"get missing bucket", getErr("no-such-bucket", "k", nil), store.ErrNoSuchBucket},
		{"get bad range", getErr(testBucket, "k", &store.Range{Start: 100, End: -1}), store.ErrInvalidRange},
		{"head missing", headMissing, store.ErrNoSuchKey},
		{"delete missing bucket", e.Delete(ctx, "no-such-bucket", "k"), store.ErrNoSuchBucket},
		{"list missing bucket", listMissing, store.ErrNoSuchBucket},
		{"list bad cursor", listBadCursor, store.ErrInvalidCursor},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if !errors.Is(tt.err, tt.want) {
				t.Errorf("err = %v, want %v", tt.err, tt.want)
			}
		})
	}
}

func TestRangeGet(t *testing.T) {
	e, _ := newEngine(t)
	put(t, e, "k", "0123456789")
	tests := []struct {
		name string
		r    store.Range
		want string
	}{
		{"middle", store.Range{Start: 2, End: 4}, "234"},
		{"open end", store.Range{Start: 7, End: -1}, "789"},
		{"suffix", store.Range{Start: -2}, "89"},
		{"clamped", store.Range{Start: 8, End: 99}, "89"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got, _ := readAll(t, e, "k", &tt.r); got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBuckets(t *testing.T) {
	e := openTest(t, t.TempDir())
	ctx := context.Background()
	for _, name := range []string{"zeta", "alpha", "mid"} {
		if _, err := e.CreateBucket(ctx, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := e.CreateBucket(ctx, "alpha"); !errors.Is(err, store.ErrBucketExists) {
		t.Errorf("duplicate create: %v, want ErrBucketExists", err)
	}
	if _, err := e.CreateBucket(ctx, "No"); !errors.Is(err, store.ErrInvalidBucketName) {
		t.Errorf("bad name: %v, want ErrInvalidBucketName", err)
	}

	list, err := e.ListBuckets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range list {
		names = append(names, b.Name)
		if b.Created.IsZero() {
			t.Errorf("bucket %s has zero Created", b.Name)
		}
	}
	if want := []string{"alpha", "mid", "zeta"}; !slices.Equal(names, want) {
		t.Errorf("ListBuckets = %v, want %v", names, want)
	}

	if _, err := e.Put(ctx, "mid", "k", strings.NewReader("x"), store.Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteBucket(ctx, "mid"); !errors.Is(err, store.ErrBucketNotEmpty) {
		t.Errorf("delete non-empty: %v, want ErrBucketNotEmpty", err)
	}
	if err := e.Delete(ctx, "mid", "k"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteBucket(ctx, "mid"); err != nil {
		t.Errorf("delete empty: %v", err)
	}
	if err := e.DeleteBucket(ctx, "mid"); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("delete twice: %v, want ErrNoSuchBucket", err)
	}
	// A deleted bucket's name can be reused, and starts empty.
	if _, err := e.CreateBucket(ctx, "mid"); err != nil {
		t.Fatal(err)
	}
	if res, _ := e.List(ctx, "mid", store.ListOptions{}); len(res.Objects) != 0 {
		t.Errorf("recreated bucket not empty: %+v", res)
	}
}

func TestList(t *testing.T) {
	e, _ := newEngine(t)
	keys := []string{
		"a.txt", "b/1", "b/2", "b/c/3", "b/d", "c", "d/x", "d/y", "e\xff/z", "z",
	}
	// Insert out of order; listings must come back sorted.
	for _, k := range slices.Backward(keys) {
		if !strings.Contains(k, "\xff") { // keep keys valid UTF-8
			put(t, e, k, k)
		}
	}
	tests := []struct {
		name         string
		opts         store.ListOptions
		wantObjects  []string
		wantPrefixes []string
	}{
		{"all", store.ListOptions{}, []string{"a.txt", "b/1", "b/2", "b/c/3", "b/d", "c", "d/x", "d/y", "z"}, nil},
		{"prefix", store.ListOptions{Prefix: "b/"}, []string{"b/1", "b/2", "b/c/3", "b/d"}, nil},
		{"delimiter at root", store.ListOptions{Delimiter: "/"}, []string{"a.txt", "c", "z"}, []string{"b/", "d/"}},
		{"delimiter under prefix", store.ListOptions{Prefix: "b/", Delimiter: "/"}, []string{"b/1", "b/2", "b/d"}, []string{"b/c/"}},
		{"no match", store.ListOptions{Prefix: "q"}, nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			res, err := e.List(context.Background(), testBucket, tt.opts)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, o := range res.Objects {
				got = append(got, o.Key)
				if o.ETag != sha(o.Key) {
					t.Errorf("%s: wrong ETag", o.Key)
				}
			}
			if !slices.Equal(got, tt.wantObjects) || !slices.Equal(res.CommonPrefixes, tt.wantPrefixes) {
				t.Errorf("got objects %v prefixes %v, want %v %v", got, res.CommonPrefixes, tt.wantObjects, tt.wantPrefixes)
			}
			if res.NextCursor != "" {
				t.Errorf("unexpected NextCursor %q", res.NextCursor)
			}
		})
	}
}

// TestListPagination checks that paging with every limit yields exactly the
// same sequence as one big page, with and without a delimiter.
func TestListPagination(t *testing.T) {
	e, _ := newEngine(t)
	for _, k := range []string{"a", "b/1", "b/2", "b/3", "c", "d/1", "d/e/2", "f"} {
		put(t, e, k, k)
	}
	for _, delim := range []string{"", "/"} {
		full, err := e.List(context.Background(), testBucket, store.ListOptions{Delimiter: delim})
		if err != nil {
			t.Fatal(err)
		}
		want := flatten(full)
		for limit := 1; limit <= len(want)+1; limit++ {
			var got []string
			cursor := ""
			for pages := 0; ; pages++ {
				if pages > len(want)+1 {
					t.Fatalf("delim %q limit %d: pagination does not terminate", delim, limit)
				}
				res, err := e.List(context.Background(), testBucket,
					store.ListOptions{Delimiter: delim, Limit: limit, Cursor: cursor})
				if err != nil {
					t.Fatal(err)
				}
				if n := len(res.Objects) + len(res.CommonPrefixes); n > limit {
					t.Fatalf("page has %d entries, limit %d", n, limit)
				}
				got = append(got, flatten(res)...)
				if res.NextCursor == "" {
					break
				}
				cursor = res.NextCursor
			}
			slices.Sort(got) // pages interleave objects and prefixes
			if !slices.Equal(got, want) {
				t.Errorf("delim %q limit %d: got %v, want %v", delim, limit, got, want)
			}
		}
	}
}

func flatten(r store.ListResult) []string {
	var out []string
	for _, o := range r.Objects {
		out = append(out, o.Key)
	}
	out = append(out, r.CommonPrefixes...)
	slices.Sort(out)
	return out
}

func TestPrefixSuccessor(t *testing.T) {
	tests := []struct {
		in, want []byte
	}{
		{[]byte("b/"), []byte("b0")},
		{[]byte{'a', 0xff}, []byte{'b'}},
		{[]byte{0xff, 0xff}, nil},
	}
	for _, tt := range tests {
		if got := prefixSuccessor(tt.in); !bytes.Equal(got, tt.want) {
			t.Errorf("prefixSuccessor(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	tests := []struct {
		name    string
		corrupt func(path string) error
	}{
		{"bit flip", func(p string) error {
			b, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			b[3] ^= 0x01
			return os.WriteFile(p, b, 0o600)
		}},
		{"truncated", func(p string) error { return os.Truncate(p, 4) }},
		{"missing", os.Remove},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, _ := newEngine(t)
			put(t, e, "k", "precious bytes")
			rec, err := e.lookup(testBucket, "k")
			if err != nil {
				t.Fatal(err)
			}
			if err := tt.corrupt(e.blobPath(rec.Blob)); err != nil {
				t.Fatal(err)
			}
			rc, _, err := e.Get(context.Background(), testBucket, "k", nil)
			if err == nil {
				_, err = io.ReadAll(rc)
				rc.Close()
			}
			if !errors.Is(err, store.ErrDataCorrupt) {
				t.Errorf("err = %v, want ErrDataCorrupt", err)
			}
		})
	}
}

// failingReader returns some bytes, then an error, like a client that drops
// the connection mid-upload.
type failingReader struct{ n int }

func (f *failingReader) Read(p []byte) (int, error) {
	if f.n <= 0 {
		return 0, io.ErrUnexpectedEOF
	}
	k := min(len(p), f.n)
	f.n -= k
	return k, nil
}

func TestFailedUploadKeepsOldObject(t *testing.T) {
	tests := []struct {
		name string
		body func(cancel context.CancelFunc) io.Reader
	}{
		{"client disconnect", func(context.CancelFunc) io.Reader { return &failingReader{n: 64 << 10} }},
		{"context cancelled", func(cancel context.CancelFunc) io.Reader {
			cancel()
			return strings.NewReader("never read")
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e, dir := newEngine(t)
			put(t, e, "k", "old")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if _, err := e.Put(ctx, testBucket, "k", tt.body(cancel), store.Meta{}); err == nil {
				t.Fatal("Put succeeded, want error")
			}
			if got, _ := readAll(t, e, "k", nil); got != "old" {
				t.Errorf("got %q, want old object", got)
			}
			if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
				t.Errorf("tmp has %d leftover files", n)
			}
			if n := countFiles(t, filepath.Join(dir, "objects")); n != 1 {
				t.Errorf("objects/ has %d files, want 1", n)
			}
		})
	}
}

func TestOpenTwiceFails(t *testing.T) {
	dir := t.TempDir()
	openTest(t, dir)
	if e, err := Open(dir, Options{}); err == nil {
		e.Close()
		t.Fatal("second Open of the same dir succeeded")
	}
}
