package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

func link(id, bucket, key string) store.LinkRecord {
	return store.LinkRecord{ID: id, Token: "tok-" + id, Bucket: bucket, Key: key, Created: time.Now().UTC(), CreatedBy: "k1"}
}

func TestLinks(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()
	put(t, e, "a/one.txt", "1")
	put(t, e, "a/one.txt.bak", "x") // shares a string prefix with a/one.txt
	put(t, e, "two.txt", "2")

	if err := e.CreateLink(ctx, link("missing", testBucket, "nope")); !errors.Is(err, store.ErrNoSuchKey) {
		t.Fatalf("link to missing object: %v, want ErrNoSuchKey", err)
	}
	for _, l := range []store.LinkRecord{
		link("L1", testBucket, "a/one.txt"),
		link("L2", testBucket, "a/one.txt"),
		link("L3", testBucket, "a/one.txt.bak"),
		link("L4", testBucket, "two.txt"),
	} {
		if err := e.CreateLink(ctx, l); err != nil {
			t.Fatal(err)
		}
	}

	ids := func(recs []store.LinkRecord) string {
		var s []string
		for _, r := range recs {
			s = append(s, r.ID)
		}
		return strings.Join(s, ",")
	}
	tests := []struct {
		name, bucket, key, want string
	}{
		{"one object", testBucket, "a/one.txt", "L1,L2"},
		{"no prefix bleed", testBucket, "a/one.txt.bak", "L3"},
		{"whole bucket", testBucket, "", "L1,L2,L3,L4"},
		{"everything", "", "", "L1,L2,L3,L4"},
	}
	for _, tt := range tests {
		got, err := e.ListLinks(ctx, tt.bucket, tt.key)
		if err != nil {
			t.Fatal(err)
		}
		if ids(got) != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, ids(got), tt.want)
		}
	}

	if r, err := e.LinkByToken(ctx, "tok-L4"); err != nil || r.Key != "two.txt" {
		t.Fatalf("LinkByToken = %+v, %v", r, err)
	}
	if _, err := e.LinkByToken(ctx, "bogus"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Fatalf("unknown token: %v", err)
	}

	// Revoke one link.
	if err := e.DeleteLink(ctx, "L2"); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ListLinks(ctx, testBucket, "a/one.txt"); ids(got) != "L1" {
		t.Errorf("after revoke: %s", ids(got))
	}
	if err := e.DeleteLink(ctx, "L2"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("revoke twice: %v", err)
	}

	// Overwriting keeps links; deleting the object removes them, and a new
	// upload to the same key does not bring them back.
	put(t, e, "a/one.txt", "1 again")
	if _, err := e.LinkByToken(ctx, "tok-L1"); err != nil {
		t.Errorf("overwrite dropped the link: %v", err)
	}
	if err := e.Delete(ctx, testBucket, "a/one.txt"); err != nil {
		t.Fatal(err)
	}
	put(t, e, "a/one.txt", "reborn")
	if _, err := e.LinkByToken(ctx, "tok-L1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("link survived object delete: %v", err)
	}
	if _, err := e.LinkByToken(ctx, "tok-L3"); err != nil {
		t.Errorf("deleting a/one.txt removed a/one.txt.bak's link: %v", err)
	}
}

func TestLinksGoWithBucket(t *testing.T) {
	e, _ := newEngine(t)
	ctx := context.Background()
	put(t, e, "k", "v")
	if err := e.CreateLink(ctx, link("L1", testBucket, "k")); err != nil {
		t.Fatal(err)
	}
	if err := e.Delete(ctx, testBucket, "k"); err != nil {
		t.Fatal(err)
	}
	if err := e.DeleteBucket(ctx, testBucket); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBucket(ctx, testBucket); err != nil {
		t.Fatal(err)
	}
	if got, _ := e.ListLinks(ctx, "", ""); len(got) != 0 {
		t.Errorf("re-created bucket inherited %d links", len(got))
	}
}

func TestBucketPublic(t *testing.T) {
	e, dir := newEngine(t)
	ctx := context.Background()
	if b, err := e.GetBucket(ctx, testBucket); err != nil || b.Public {
		t.Fatalf("new bucket = %+v, %v; want private", b, err)
	}
	if b, err := e.SetBucketPublic(ctx, testBucket, true); err != nil || !b.Public {
		t.Fatalf("SetBucketPublic = %+v, %v", b, err)
	}
	// Survives a restart, and shows in listings.
	e.Close()
	e = openTest(t, dir)
	if b, _ := e.GetBucket(ctx, testBucket); !b.Public {
		t.Error("public flag lost on reopen")
	}
	list, _ := e.ListBuckets(ctx)
	if len(list) != 1 || !list[0].Public {
		t.Errorf("ListBuckets = %+v", list)
	}
	if _, err := e.SetBucketPublic(ctx, "no-such", true); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
	if _, err := e.GetBucket(ctx, "no-such"); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("GetBucket missing: %v", err)
	}
}

// TestCrashDeleteRemovesLinks: the object row and its links go in one
// transaction, so a crash right after the delete commits leaves neither.
func TestCrashDeleteRemovesLinks(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	e, err := Open(dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.CreateBucket(ctx, testBucket); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Put(ctx, testBucket, "k", strings.NewReader("v"), store.Meta{}); err != nil {
		t.Fatal(err)
	}
	if err := e.CreateLink(ctx, link("L1", testBucket, "k")); err != nil {
		t.Fatal(err)
	}
	e.crash = func(p string) bool { return p == "delete:committed" }
	if err := e.Delete(ctx, testBucket, "k"); err != errSimulatedCrash {
		t.Fatalf("crash point did not fire: %v", err)
	}
	e.Close()

	e = openTest(t, dir)
	if _, err := e.Head(ctx, testBucket, "k"); !errors.Is(err, store.ErrNoSuchKey) {
		t.Errorf("object after crash: %v", err)
	}
	if _, err := e.LinkByToken(ctx, "tok-L1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("link after crash: %v", err)
	}
	if n := countFiles(t, dir+"/objects"); n != 0 {
		t.Errorf("%d orphan blobs after restart", n)
	}
}
