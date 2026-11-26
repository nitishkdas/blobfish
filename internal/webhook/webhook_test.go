package webhook

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
)

type received struct {
	typ, delivery, sig string
	body               []byte
}

// receiver is a webhook endpoint whose answer the test controls.
type receiver struct {
	mu     sync.Mutex
	got    []received
	status int
	srv    *httptest.Server
}

func newReceiver(t *testing.T) *receiver {
	rc := &receiver{status: http.StatusNoContent}
	rc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		defer rc.mu.Unlock()
		rc.got = append(rc.got, received{r.Header.Get(HeaderEvent), r.Header.Get(HeaderDelivery), r.Header.Get(HeaderSignature), b})
		if rc.status == http.StatusFound {
			w.Header().Set("Location", "http://example.com/")
		}
		w.WriteHeader(rc.status)
	}))
	t.Cleanup(rc.srv.Close)
	return rc
}

func (rc *receiver) take() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	out := rc.got
	rc.got = nil
	return out
}

func (rc *receiver) answer(status int) {
	rc.mu.Lock()
	rc.status = status
	rc.mu.Unlock()
}

type fixture struct {
	e     *engine.Engine
	d     *Dispatcher
	clock time.Time
}

func setup(t *testing.T, opts engine.Options) *fixture {
	t.Helper()
	e, err := engine.Open(t.TempDir(), opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { e.Close() })
	if _, err := e.CreateBucket(context.Background(), "hooked"); err != nil {
		t.Fatal(err)
	}
	// Events are stamped with the real time; start the dispatcher's clock
	// ahead so they are due.
	f := &fixture{e: e, clock: time.Now().Add(time.Minute)}
	f.d = New(e, slog.New(slog.DiscardHandler), "test")
	f.d.now = func() time.Time { return f.clock }
	return f
}

func (f *fixture) hook(t *testing.T, url string, allowPrivate bool, events ...string) store.WebhookRecord {
	t.Helper()
	if len(events) == 0 {
		events = store.EventTypes
	}
	w := store.WebhookRecord{ID: "h-" + url[len(url)-4:] + strings.Join(events, ""), Bucket: "hooked", URL: url, Events: events,
		Secret: "whsec_test", AllowPrivate: allowPrivate, Created: time.Now()}
	if err := f.e.CreateWebhook(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	return w
}

func (f *fixture) put(t *testing.T, key, body string) {
	t.Helper()
	if _, err := f.e.Put(context.Background(), "hooked", key, strings.NewReader(body), store.Meta{}); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) webhook(t *testing.T, id string) store.WebhookRecord {
	t.Helper()
	w, err := f.e.WebhookByID(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

func TestDeliversSignedEvents(t *testing.T) {
	ctx := context.Background()
	f := setup(t, engine.Options{TrashRetention: time.Hour})
	rc := newReceiver(t)
	h := f.hook(t, rc.srv.URL, true)

	f.put(t, "docs/a.txt", "hello")
	f.put(t, "docs/a.txt", "hello again")
	f.e.Delete(ctx, "hooked", "docs/a.txt")
	items, _ := f.e.ListTrash(ctx, "hooked", store.TrashListOptions{})
	f.e.RestoreTrash(ctx, "hooked", items.Items[0].ID)

	select {
	case <-f.e.EventSignal():
	default:
		t.Error("no wake-up signal after queueing events")
	}
	f.d.RunOnce(ctx)
	got := rc.take()
	if len(got) != 4 {
		t.Fatalf("got %d deliveries, want 4", len(got))
	}
	wantTypes := []string{store.EventObjectCreated, store.EventObjectCreated, store.EventObjectDeleted, store.EventObjectRestored}
	for i, g := range got {
		var ev map[string]any
		if err := json.Unmarshal(g.body, &ev); err != nil {
			t.Fatal(err)
		}
		if g.typ != wantTypes[i] || ev["type"] != wantTypes[i] || ev["key"] != "docs/a.txt" || ev["bucket"] != "hooked" || ev["id"] != g.delivery {
			t.Errorf("delivery %d: %s %s", i, g.typ, g.body)
		}
		// The receiver can check the signature with the secret alone.
		unix, err := strconv.ParseInt(strings.TrimPrefix(strings.Split(g.sig, ",")[0], "t="), 10, 64)
		if err != nil {
			t.Fatalf("signature %q", g.sig)
		}
		if want := Sign("whsec_test", time.Unix(unix, 0), g.body); g.sig != want {
			t.Errorf("signature %q, want %q", g.sig, want)
		}
	}
	if !strings.Contains(string(got[1].body), `"replaced":true`) || !strings.Contains(string(got[2].body), `"trash":true`) ||
		!strings.Contains(string(got[0].body), `"size":5`) {
		t.Errorf("payload flags: %s / %s / %s", got[0].body, got[1].body, got[2].body)
	}
	w := f.webhook(t, h.ID)
	if w.Pending != 0 || w.LastSuccess.IsZero() {
		t.Errorf("webhook after delivery: %+v", w)
	}
	f.d.RunOnce(ctx)
	if n := len(rc.take()); n != 0 {
		t.Errorf("delivered %d events twice", n)
	}
}

func TestEventFilterAndFailedWrites(t *testing.T) {
	ctx := context.Background()
	f := setup(t, engine.Options{})
	rc := newReceiver(t)
	f.hook(t, rc.srv.URL, true, store.EventObjectDeleted)
	f.put(t, "a", "1")
	f.e.Delete(ctx, "hooked", "a")
	f.e.Delete(ctx, "hooked", "never-existed") // no-op delete: no event
	f.e.SetBucketQuota(ctx, "hooked", 1)
	f.e.Put(ctx, "hooked", "big", strings.NewReader("too big"), store.Meta{}) // refused: no event
	f.d.RunOnce(ctx)
	got := rc.take()
	if len(got) != 1 || got[0].typ != store.EventObjectDeleted {
		t.Errorf("got %+v", got)
	}
}

func TestRetryBackoffAndGiveUp(t *testing.T) {
	ctx := context.Background()
	f := setup(t, engine.Options{})
	rc := newReceiver(t)
	h := f.hook(t, rc.srv.URL, true)
	rc.answer(http.StatusInternalServerError)
	f.put(t, "a", "1")
	f.put(t, "b", "2")

	f.d.RunOnce(ctx)
	if n := len(rc.take()); n != 1 {
		t.Fatalf("first run sent %d; after a failure the webhook should pause", n)
	}
	w := f.webhook(t, h.ID)
	if w.Pending != 2 || w.LastError != "HTTP 500" || !w.RetryAfter.Equal(f.clock.Add(backoff[0])) {
		t.Fatalf("after failure: %+v", w)
	}
	f.d.RunOnce(ctx) // still paused
	if n := len(rc.take()); n != 0 {
		t.Fatalf("sent %d while paused", n)
	}

	// The receiver recovers: both go out, in order.
	rc.answer(http.StatusOK)
	f.clock = f.clock.Add(backoff[0])
	f.d.RunOnce(ctx)
	got := rc.take()
	if len(got) != 2 || !strings.Contains(string(got[0].body), `"key":"a"`) || !strings.Contains(string(got[1].body), `"key":"b"`) {
		t.Fatalf("after recovery: %d deliveries", len(got))
	}
	if w := f.webhook(t, h.ID); w.Pending != 0 || !w.RetryAfter.IsZero() {
		t.Errorf("after recovery: %+v", w)
	}

	// A receiver that never recovers: given up after the last backoff step.
	rc.answer(http.StatusBadGateway)
	f.put(t, "c", "3")
	for range len(backoff) + 1 {
		f.d.RunOnce(ctx)
		f.clock = f.clock.Add(7 * time.Hour)
	}
	if n := len(rc.take()); n != len(backoff)+1 {
		t.Errorf("attempts = %d, want %d", n, len(backoff)+1)
	}
	w = f.webhook(t, h.ID)
	if w.GaveUp != 1 || w.Pending != 0 || w.LastError != "HTTP 502" {
		t.Errorf("after giving up: %+v", w)
	}
}

func TestAddressRules(t *testing.T) {
	ctx := context.Background()
	f := setup(t, engine.Options{})
	rc := newReceiver(t) // listens on 127.0.0.1
	locked := f.hook(t, rc.srv.URL, false)
	f.put(t, "a", "1")
	f.d.RunOnce(ctx)
	if n := len(rc.take()); n != 0 {
		t.Fatal("a bucket admin's webhook reached loopback")
	}
	if w := f.webhook(t, locked.ID); !strings.Contains(w.LastError, "private") {
		t.Errorf("error = %q", w.LastError)
	}

	// Redirects are not followed: a public receiver can't bounce a
	// delivery to an internal address.
	f2 := setup(t, engine.Options{})
	rc2 := newReceiver(t)
	rc2.answer(http.StatusFound)
	h := f2.hook(t, rc2.srv.URL, true)
	f2.put(t, "a", "1")
	f2.d.RunOnce(ctx)
	if w := f2.webhook(t, h.ID); w.LastError != "HTTP 302" {
		t.Errorf("redirect: %+v", w)
	}

	for addr, public := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "::1": false, "fe80::1": false, "fd00::1": false, "0.0.0.0": false, "::ffff:127.0.0.1": false,
		"224.0.0.1": false,
		// Special-purpose ranges, and IPv6 forms that embed an IPv4 address.
		"0.0.0.1": false, "198.18.0.1": false, "192.0.0.8": false, "240.0.0.1": false, "255.255.255.255": false,
		"192.0.2.1": false, "::": false, "::7f00:1": false, "::a00:1": false, "2002:7f00:1::": false,
		"2001:0:4136:e378:8000:63bf:3fff:fdd2": false, "2001:db8::1": false, "100::1": false, "ff02::1": false,
		"64:ff9b::7f00:1": false, "64:ff9b::a00:1": false, "64:ff9b::808:808": true, "2a00:1450::1": true,
	} {
		if got := IsPublic(netip.MustParseAddr(addr)); got != public {
			t.Errorf("IsPublic(%s) = %v", addr, got)
		}
	}
}

func TestQueueSurvivesRestartAndBucketDelete(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	e, _ := engine.Open(dir, engine.Options{})
	e.CreateBucket(ctx, "hooked")
	e.CreateWebhook(ctx, store.WebhookRecord{ID: "h1", Bucket: "hooked", URL: "http://x.invalid/", Events: store.EventTypes})
	e.Put(ctx, "hooked", "a", strings.NewReader("1"), store.Meta{})
	e.Close()

	e, _ = engine.Open(dir, engine.Options{})
	defer e.Close()
	due, err := e.DueDeliveries(ctx, time.Now(), 10)
	if err != nil || len(due) != 1 || due[0].EventType != store.EventObjectCreated {
		t.Fatalf("after restart: %+v, %v", due, err)
	}
	e.Delete(ctx, "hooked", "a")
	if err := e.DeleteBucket(ctx, "hooked"); err != nil {
		t.Fatal(err)
	}
	e.CreateBucket(ctx, "hooked")
	if hooks, _ := e.ListWebhooks(ctx, "hooked"); len(hooks) != 0 {
		t.Error("re-created bucket inherited a webhook")
	}
	if due, _ := e.DueDeliveries(ctx, time.Now(), 10); len(due) != 0 {
		t.Errorf("deliveries left after bucket delete: %d", len(due))
	}
}

func TestPingAndDelete(t *testing.T) {
	ctx := context.Background()
	f := setup(t, engine.Options{})
	rc := newReceiver(t)
	h := f.hook(t, rc.srv.URL, true, store.EventObjectDeleted)
	if err := f.e.QueueTestEvent(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	f.d.RunOnce(ctx)
	if got := rc.take(); len(got) != 1 || got[0].typ != store.EventPing {
		t.Fatalf("ping: %+v", got)
	}
	rc.answer(http.StatusInternalServerError)
	f.put(t, "x", "1")
	f.e.Delete(ctx, "hooked", "x")
	if err := f.e.DeleteWebhook(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if due, _ := f.e.DueDeliveries(ctx, time.Now().Add(time.Hour), 10); len(due) != 0 {
		t.Errorf("deleted webhook still has %d deliveries", len(due))
	}
}
