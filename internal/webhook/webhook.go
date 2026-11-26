// Package webhook delivers queued events to webhook URLs. Events are queued
// by the engine in the same transaction as the change they describe; the
// Dispatcher here reads them from the store.Outbox, signs them and POSTs
// them, retrying with backoff. Delivery is at least once: a receiver may
// see an event twice (for example after a crash right after sending) and
// should de-duplicate on the event ID.
package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"syscall"
	"time"

	"github.com/nitishkdas/blobfish/internal/store"
)

// Signature header: t=<unix seconds>,v1=<hex HMAC-SHA256(secret, "<t>.<body>")>.
// Receivers recompute it and should reject old timestamps (replays).
const (
	HeaderSignature = "X-BlobFish-Signature"
	HeaderEvent     = "X-BlobFish-Event"
	HeaderDelivery  = "X-BlobFish-Delivery"
)

// Sign returns the signature header value for body at time t.
func Sign(secret string, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(mac.Sum(nil))
}

// backoff is the wait after each failed attempt; after the last one the
// delivery is given up (about 24 hours in all).
var backoff = []time.Duration{
	10 * time.Second, 30 * time.Second, time.Minute, 2 * time.Minute, 5 * time.Minute,
	10 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour,
	6 * time.Hour, 6 * time.Hour, 6 * time.Hour,
}

const (
	sendTimeout  = 10 * time.Second
	batchSize    = 100
	pollInterval = 30 * time.Second // safety net; normally woken by EventSignal
	maxProblem   = 300              // bytes of error text kept per webhook
)

// Dispatcher sends queued events. Run it in one goroutine.
type Dispatcher struct {
	outbox  store.Outbox
	log     *slog.Logger
	version string
	now     func() time.Time
	// public may reach only public addresses; any may reach everything
	// (webhooks made by server admins).
	public, any *http.Client
}

// New returns a dispatcher reading from outbox.
func New(outbox store.Outbox, log *slog.Logger, version string) *Dispatcher {
	return &Dispatcher{
		outbox: outbox, log: log, version: version, now: time.Now,
		public: newClient(false), any: newClient(true),
	}
}

// Run delivers events until ctx is cancelled.
func (d *Dispatcher) Run(ctx context.Context) {
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-d.outbox.EventSignal():
		case <-t.C:
		}
		next := d.RunOnce(ctx)
		wait := pollInterval
		if !next.IsZero() {
			wait = min(max(time.Until(next), time.Second), pollInterval)
		}
		t.Reset(wait)
	}
}

// RunOnce sends every delivery that is due and returns when the next
// retry is due (zero if it doesn't know). After a failure the whole
// webhook pauses for the backoff (the store skips it until RetryAfter), so
// a dead receiver costs one attempt per backoff step, not one per queued
// event, and its events stay in order.
func (d *Dispatcher) RunOnce(ctx context.Context) time.Time {
	due, err := d.outbox.DueDeliveries(ctx, d.now(), batchSize)
	if err != nil {
		d.log.Error("webhook outbox unreadable", "err", err)
		return d.now().Add(pollInterval)
	}
	failing := map[string]bool{}
	var next time.Time
	for _, dl := range due {
		if ctx.Err() != nil {
			return time.Time{}
		}
		if failing[dl.WebhookID] {
			continue // retried with its own backoff after the first failure
		}
		problem := d.send(ctx, dl)
		if ctx.Err() != nil {
			return time.Time{} // shutting down: not the receiver's fault, retry after restart
		}
		now := d.now()
		if problem == "" {
			if err := d.outbox.DeliverySucceeded(ctx, dl.Seq, now); err != nil {
				d.log.Error("webhook delivery not recorded", "err", err)
			}
			continue
		}
		failing[dl.WebhookID] = true
		var retryAt time.Time
		if dl.Attempts < len(backoff) {
			retryAt = now.Add(backoff[dl.Attempts])
			if next.IsZero() || retryAt.Before(next) {
				next = retryAt
			}
		}
		lvl := slog.LevelWarn
		if retryAt.IsZero() {
			lvl = slog.LevelError
		}
		d.log.Log(ctx, lvl, "webhook delivery failed", "webhook", dl.WebhookID, "event", dl.EventID,
			"attempt", dl.Attempts+1, "err", problem, "gave_up", retryAt.IsZero())
		if err := d.outbox.DeliveryFailed(ctx, dl.Seq, now, problem, retryAt); err != nil {
			d.log.Error("webhook failure not recorded", "err", err)
		}
	}
	if len(due) == batchSize {
		return d.now() // more waiting
	}
	return next
}

// send POSTs one event and returns "" on a 2xx answer, else what went wrong.
func (d *Dispatcher) send(ctx context.Context, dl store.Delivery) string {
	ctx, cancel := context.WithTimeout(ctx, sendTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, dl.URL, bytes.NewReader(dl.Payload))
	if err != nil {
		return clip(err.Error())
	}
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("User-Agent", "BlobFish-Webhook/"+d.version)
	h.Set(HeaderEvent, dl.EventType)
	h.Set(HeaderDelivery, dl.EventID)
	h.Set(HeaderSignature, Sign(dl.Secret, d.now(), dl.Payload))
	client := d.public
	if dl.AllowPrivate {
		client = d.any
	}
	resp, err := client.Do(req)
	if err != nil {
		return clip(err.Error())
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096)) // let the connection be reused
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Sprintf("HTTP %d", resp.StatusCode)
	}
	return ""
}

func clip(s string) string {
	if len(s) > maxProblem {
		return s[:maxProblem] + "…"
	}
	return s
}

// ErrPrivateAddress: a webhook made by a bucket admin pointed (directly or
// through DNS) at this machine or a private network.
var ErrPrivateAddress = errors.New("webhook address is loopback, private or link-local; only server admins may target those")

// newClient builds the HTTP client for deliveries. Redirects are not
// followed (a 3xx is a failure) and environment proxies are ignored, so
// the address check below can't be sidestepped. Without allowPrivate, the
// check runs on the resolved IP right before connecting, so DNS that
// answers with an internal address is refused too.
func newClient(allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: sendTimeout}
	if !allowPrivate {
		dialer.Control = func(_, address string, _ syscall.RawConn) error {
			ap, err := netip.ParseAddrPort(address)
			if err != nil {
				return err
			}
			if !IsPublic(ap.Addr()) {
				return fmt.Errorf("%w (%s)", ErrPrivateAddress, ap.Addr())
			}
			return nil
		}
	}
	return &http.Client{
		Timeout: sendTimeout,
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: sendTimeout,
			MaxIdleConns:        10,
			IdleConnTimeout:     90 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// IsPublic reports whether a is a globally routable unicast address.
func IsPublic(a netip.Addr) bool {
	a = a.Unmap()
	return a.IsValid() && a.IsGlobalUnicast() && !a.IsPrivate() && !a.IsLoopback() &&
		!a.IsLinkLocalUnicast() && !cgnat.Contains(a)
}

// cgnat is carrier-grade NAT space (RFC 6598), private in practice.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")
