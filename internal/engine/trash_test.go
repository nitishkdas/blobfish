package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/nitishkdas/blobfish/internal/store"
)

const week = 7 * 24 * time.Hour

// newTrashEngine opens an engine with trash on and a clock the test moves.
func newTrashEngine(t *testing.T, dir string, retention time.Duration, clock *time.Time) *Engine {
	t.Helper()
	e, err := Open(dir, Options{TrashRetention: retention})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	e.now = func() time.Time { return *clock }
	if _, err := e.CreateBucket(context.Background(), testBucket); err != nil && !errors.Is(err, store.ErrBucketExists) {
		t.Fatal(err)
	}
	return e
}

func trashItems(t *testing.T, e *Engine) []store.TrashItem {
	t.Helper()
	l, err := e.ListTrash(context.Background(), testBucket, store.TrashListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return l.Items
}

func newLink(t *testing.T, e *Engine, key, id string) store.LinkRecord {
	t.Helper()
	rec := store.LinkRecord{ID: id, Token: "tok-" + id, Bucket: testBucket, Key: key, Created: time.Now()}
	if err := e.CreateLink(context.Background(), rec); err != nil {
		t.Fatal(err)
	}
	return rec
}

func linkWorks(e *Engine, token string) bool {
	_, err := e.LinkByToken(context.Background(), token)
	return err == nil
}

func TestTrashDeleteAndRestore(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	put(t, e, "docs/a.txt", "precious")
	link := newLink(t, e, "docs/a.txt", "l1")

	if err := e.Delete(ctx, testBucket, "docs/a.txt"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Head(ctx, testBucket, "docs/a.txt"); !errors.Is(err, store.ErrNoSuchKey) {
		t.Errorf("deleted object still visible: %v", err)
	}
	if res, _ := e.List(ctx, testBucket, store.ListOptions{}); len(res.Objects) != 0 {
		t.Errorf("deleted object still listed: %v", res.Objects)
	}
	items := trashItems(t, e)
	if len(items) != 1 || items[0].Object.Key != "docs/a.txt" || items[0].Object.Size != 8 ||
		!items[0].Deleted.Equal(clock) || !items[0].PurgeAt.Equal(clock.Add(week)) {
		t.Fatalf("trash = %+v", items)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 1 {
		t.Errorf("blob files = %d, want 1 (kept for restore)", n)
	}
	// The janitor must keep a trashed blob.
	if st, err := e.RunJanitor(ctx); err != nil || st.BlobsRemoved != 0 || st.TrashPurged != 0 {
		t.Errorf("janitor: %+v, %v", st, err)
	}
	if linkWorks(e, link.Token) {
		t.Error("share link works while its object is in trash")
	}
	if ls, _ := e.ListLinks(ctx, testBucket, "docs/a.txt"); len(ls) != 0 {
		t.Errorf("trashed links listed: %v", ls)
	}

	info, err := e.RestoreTrash(ctx, testBucket, items[0].ID)
	if err != nil || info.Key != "docs/a.txt" {
		t.Fatalf("restore: %+v, %v", info, err)
	}
	if got, _ := readAll(t, e, "docs/a.txt", nil); got != "precious" {
		t.Errorf("after restore: %q", got)
	}
	if !linkWorks(e, link.Token) {
		t.Error("share link dead after restore")
	}
	if len(trashItems(t, e)) != 0 {
		t.Error("trash not empty after restore")
	}
	if _, err := e.RestoreTrash(ctx, testBucket, items[0].ID); !errors.Is(err, store.ErrNoSuchTrashItem) {
		t.Errorf("second restore: %v", err)
	}
}

// A new upload to a deleted key: old links stay dead (they belong to the
// trashed copy), restore is refused while the key is taken, and each copy
// keeps its own links through further deletes and restores.
func TestTrashKeyReused(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	e := newTrashEngine(t, t.TempDir(), week, &clock)
	put(t, e, "k", "first")
	old := newLink(t, e, "k", "old")
	e.Delete(ctx, testBucket, "k")
	first := trashItems(t, e)[0].ID

	put(t, e, "k", "second")
	if linkWorks(e, old.Token) {
		t.Fatal("re-upload revived the trashed copy's link")
	}
	fresh := newLink(t, e, "k", "new")
	if _, err := e.RestoreTrash(ctx, testBucket, first); !errors.Is(err, store.ErrKeyExists) {
		t.Fatalf("restore over a live object: %v", err)
	}

	clock = clock.Add(time.Second)
	e.Delete(ctx, testBucket, "k") // second copy to trash too
	if len(trashItems(t, e)) != 2 || linkWorks(e, fresh.Token) {
		t.Fatal("second delete")
	}
	if _, err := e.RestoreTrash(ctx, testBucket, first); err != nil {
		t.Fatal(err)
	}
	if got, _ := readAll(t, e, "k", nil); got != "first" {
		t.Errorf("restored %q", got)
	}
	if !linkWorks(e, old.Token) || linkWorks(e, fresh.Token) {
		t.Error("restore must bring back exactly the first copy's link")
	}
}

func TestTrashPurge(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	for _, k := range []string{"a", "b", "c"} {
		put(t, e, k, k)
		newLink(t, e, k, "link-"+k)
		e.Delete(ctx, testBucket, k)
		clock = clock.Add(time.Second) // distinct delete times, so the order is known
	}
	items := trashItems(t, e) // newest first: c, b, a
	if items[0].Object.Key != "c" {
		t.Fatalf("newest = %q", items[0].Object.Key)
	}
	if err := e.PurgeTrash(ctx, testBucket, items[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := e.LinkByID(ctx, "link-c"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("purged object's link survives: %v", err)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 2 {
		t.Errorf("blob files = %d after purging one of 3", n)
	}
	if err := e.PurgeTrash(ctx, testBucket, items[0].ID); !errors.Is(err, store.ErrNoSuchTrashItem) {
		t.Errorf("purge twice: %v", err)
	}
	if err := e.PurgeTrash(ctx, testBucket, ""); err != nil {
		t.Fatal(err)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 0 || len(trashItems(t, e)) != 0 {
		t.Errorf("after emptying trash: %d blob files", n)
	}
	if ls, _ := e.ListLinks(ctx, "", ""); len(ls) != 0 {
		t.Errorf("links left: %v", ls)
	}
}

func TestTrashExpiry(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	put(t, e, "old", "1")
	e.Delete(ctx, testBucket, "old")
	clock = clock.Add(3 * 24 * time.Hour)
	put(t, e, "young", "2")
	e.Delete(ctx, testBucket, "young")

	clock = clock.Add(4*24*time.Hour + time.Minute) // old is 7d+1m, young 4d+1m
	st, err := e.RunJanitor(ctx)
	if err != nil || st.TrashPurged != 1 {
		t.Fatalf("janitor: %+v, %v", st, err)
	}
	items := trashItems(t, e)
	if len(items) != 1 || items[0].Object.Key != "young" {
		t.Errorf("trash after expiry: %+v", items)
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 1 {
		t.Errorf("blob files = %d", n)
	}
	e.Close()

	// The purge date is fixed when deleting: a shorter retention later
	// doesn't cut it short.
	e = newTrashEngine(t, dir, time.Hour, &clock)
	clock = clock.Add(2 * time.Hour)
	if st, _ := e.RunJanitor(ctx); st.TrashPurged != 0 {
		t.Errorf("new retention purged an older deletion early")
	}
}

func TestTrashOffKeepsTrashedLinks(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	put(t, e, "k", "v1")
	l := newLink(t, e, "k", "l1")
	e.Delete(ctx, testBucket, "k")
	id := trashItems(t, e)[0].ID
	e.Close()

	// Trash turned off: deletes are immediate again, but must not take
	// the links of the copy that is still in trash.
	e = newTrashEngine(t, dir, 0, &clock)
	put(t, e, "k", "v2")
	e.Delete(ctx, testBucket, "k")
	if len(trashItems(t, e)) != 1 {
		t.Fatal("delete with trash off went to trash")
	}
	if _, err := e.RestoreTrash(ctx, testBucket, id); err != nil {
		t.Fatal(err)
	}
	if !linkWorks(e, l.Token) {
		t.Error("link of the trashed copy was deleted")
	}
}

func TestTrashBlocksBucketDelete(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	e := newTrashEngine(t, t.TempDir(), week, &clock)
	put(t, e, "k", "v")
	e.Delete(ctx, testBucket, "k")
	err := e.DeleteBucket(ctx, testBucket)
	if !errors.Is(err, store.ErrBucketNotEmpty) || !strings.Contains(err.Error(), "trash") {
		t.Fatalf("delete bucket with trash: %v", err)
	}
	e.PurgeTrash(ctx, testBucket, "")
	if err := e.DeleteBucket(ctx, testBucket); err != nil {
		t.Fatal(err)
	}
	e.CreateBucket(ctx, testBucket)
	if len(trashItems(t, e)) != 0 {
		t.Error("re-created bucket inherited trash")
	}
	if _, err := e.ListTrash(ctx, "no-such-bucket", store.TrashListOptions{}); !errors.Is(err, store.ErrNoSuchBucket) {
		t.Errorf("missing bucket: %v", err)
	}
}

func TestTrashListPaging(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	e := newTrashEngine(t, t.TempDir(), week, &clock)
	var want []string
	for i := range 5 {
		k := fmt.Sprintf("k%d", i)
		put(t, e, k, k)
		e.Delete(ctx, testBucket, k)
		want = append([]string{k}, want...) // newest first
		clock = clock.Add(time.Second)
	}
	var got []string
	cursor := ""
	for range 10 {
		page, err := e.ListTrash(ctx, testBucket, store.TrashListOptions{Cursor: cursor, Limit: 2})
		if err != nil {
			t.Fatal(err)
		}
		for _, it := range page.Items {
			got = append(got, it.Object.Key)
		}
		if cursor = page.NextCursor; cursor == "" {
			break
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("paged = %v, want %v", got, want)
	}
	if _, err := e.ListTrash(ctx, testBucket, store.TrashListOptions{Cursor: "../x"}); !errors.Is(err, store.ErrInvalidCursor) {
		t.Errorf("bad cursor: %v", err)
	}
}

func TestTrashCrashDuringPurge(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	put(t, e, "k", "v")
	newLink(t, e, "k", "l1")
	e.Delete(ctx, testBucket, "k")
	e.crash = func(p string) bool { return p == "purge:committed" }
	if err := e.PurgeTrash(ctx, testBucket, ""); err != errSimulatedCrash {
		t.Fatalf("crash point did not fire: %v", err)
	}
	e.Close()

	e = newTrashEngine(t, dir, week, &clock) // Open runs the janitor
	if len(trashItems(t, e)) != 0 {
		t.Error("purged item back in trash")
	}
	if n := countFiles(t, filepath.Join(dir, "objects")); n != 0 {
		t.Errorf("orphan blob survived restart: %d files", n)
	}
	if _, err := e.LinkByID(ctx, "l1"); !errors.Is(err, store.ErrNoSuchLink) {
		t.Errorf("link survived purge: %v", err)
	}
}

func TestTrashSurvivesBackupAndVerify(t *testing.T) {
	ctx := context.Background()
	clock := time.Now()
	dir := t.TempDir()
	e := newTrashEngine(t, dir, week, &clock)
	put(t, e, "k", "in the bin")
	e.Delete(ctx, testBucket, "k")
	id := trashItems(t, e)[0].ID
	var buf bytes.Buffer
	if err := e.Backup(ctx, &buf); err != nil {
		t.Fatal(err)
	}
	trashedBlob := ""
	e.db.View(func(tx *bbolt.Tx) error {
		_, r, err := getTrash(tx, testBucket, id)
		trashedBlob = r.Object.Blob
		return err
	})
	e.Close()

	target := filepath.Join(t.TempDir(), "r")
	if _, err := Restore(ctx, &buf, target, Options{}); err != nil {
		t.Fatal(err)
	}
	r := newTrashEngine(t, target, week, &clock)
	if _, err := r.RestoreTrash(ctx, testBucket, id); err != nil {
		t.Fatalf("restore from trash after backup/restore: %v", err)
	}
	if got, _ := readAll(t, r, "k", nil); got != "in the bin" {
		t.Errorf("got %q", got)
	}
	r.Close()

	// verify reads trashed blobs too.
	os.WriteFile(blobFile(dir, trashedBlob), []byte("in the bun"), 0o600)
	_, probs := runVerify(t, dir, VerifyOptions{})
	if len(probs) != 1 || !probs[0].InTrash || probs[0].Kind != ProblemChecksum || !strings.Contains(probs[0].String(), "k (in trash)") {
		t.Errorf("verify: %v", probs)
	}
}

func TestFormat1UpgradesToTrash(t *testing.T) {
	ctx := context.Background()
	e, dir := newEngine(t)
	put(t, e, "k", "v")
	e.Close()
	// Make it look like format-1 data: stamp 1, no trash bucket.
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		if err := tx.DeleteBucket(bktTrash); err != nil {
			return err
		}
		return tx.Bucket(bktSettings).Put(setFormat, []byte("1"))
	})
	if _, err := Verify(ctx, dir, VerifyOptions{}); err == nil || !strings.Contains(err.Error(), "start the server once") {
		t.Errorf("verify of format-1 data: %v", err)
	}
	clock := time.Now()
	e = newTrashEngine(t, dir, week, &clock)
	if err := e.Delete(ctx, testBucket, "k"); err != nil {
		t.Fatal(err)
	}
	if len(trashItems(t, e)) != 1 {
		t.Error("delete after upgrade did not reach trash")
	}
}
