package server

import (
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/store"
)

// serverStats counts traffic for GET /v1/status.
type serverStats struct {
	started                time.Time
	requests               atomic.Int64
	s2xx, s3xx, s4xx, s5xx atomic.Int64
	bytesIn, bytesOut      atomic.Int64
}

func (s *serverStats) record(status int, in, out int64) {
	s.requests.Add(1)
	switch {
	case status >= 500:
		s.s5xx.Add(1)
	case status >= 400:
		s.s4xx.Add(1)
	case status >= 300:
		s.s3xx.Add(1)
	default:
		s.s2xx.Add(1)
	}
	s.bytesIn.Add(in)
	s.bytesOut.Add(out)
}

// countingBody counts request body bytes actually read.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (c *countingBody) Read(p []byte) (int, error) {
	n, err := c.ReadCloser.Read(p)
	c.n += int64(n)
	return n, err
}

// healthCacheFor is how long a health check's result is reused. Each check
// writes and fsyncs a probe file; without a cache anyone could make the
// server do that as fast as they can send requests (hard on SD cards).
const healthCacheFor = 5 * time.Second

// healthCache holds the last health report. The mutex is held while a
// check runs, so a burst of requests causes one check, not many.
type healthCache struct {
	mu  sync.Mutex
	at  time.Time
	rep store.HealthReport
}

// healthReport returns a recent report, running a check if the last one
// is older than healthCacheFor. fresh is true when this call ran it.
func (a *api) healthReport(r *http.Request) (rep store.HealthReport, fresh bool) {
	c := &a.healthCache
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.at.IsZero() && time.Since(c.at) < healthCacheFor {
		return c.rep, false
	}
	c.rep = a.health.Health(r.Context())
	c.at = time.Now()
	return c.rep, true
}

// healthz is for load balancers and monitors, so it needs no key and
// reveals only ok/failing per check. 503 when the metadata store or the
// data disk is broken; "degraded" (still 200) when free space is at the
// reserve, since reads and deletes still work.
func (a *api) healthz(w http.ResponseWriter, r *http.Request) {
	if a.health == nil {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
		return
	}
	rep, fresh := a.healthReport(r)
	okOr := func(err error) string {
		if err != nil {
			return "failing"
		}
		return "ok"
	}
	space := "ok"
	switch {
	case rep.FreeBytes < 0:
		space = "unknown"
	case rep.LowSpace:
		space = "low"
	}
	status, code := "ok", http.StatusOK
	if rep.LowSpace {
		status = "degraded"
	}
	if rep.Metadata != nil || rep.Disk != nil {
		status, code = "unavailable", http.StatusServiceUnavailable
	}
	if fresh && code != http.StatusOK { // once per check, not per request
		a.log.ErrorContext(r.Context(), "health check failing",
			"request_id", RequestID(r.Context()), "metadata", rep.Metadata, "disk", rep.Disk)
	}
	writeJSON(w, code, map[string]any{
		"status": status,
		"checks": map[string]string{"metadata": okOr(rep.Metadata), "disk": okOr(rep.Disk), "space": space},
	})
}

type statusJSON struct {
	Version    string    `json:"version"`
	DataFormat int       `json:"dataFormat"`
	Started    time.Time `json:"started"`
	UptimeSec  int64     `json:"uptimeSeconds"`
	Disk       struct {
		FreeBytes    int64 `json:"freeBytes"` // -1 = unknown
		ReserveBytes int64 `json:"reserveBytes"`
		Low          bool  `json:"low"`
	} `json:"disk"`
	Requests struct {
		Total int64 `json:"total"`
		S2xx  int64 `json:"2xx"`
		S3xx  int64 `json:"3xx"`
		S4xx  int64 `json:"4xx"`
		S5xx  int64 `json:"5xx"`
	} `json:"requests"`
	Bytes struct {
		In  int64 `json:"in"`
		Out int64 `json:"out"`
	} `json:"bytes"`
}

// status reports version, uptime, disk space and traffic since start.
// Server admins only.
func (a *api) status(w http.ResponseWriter, r *http.Request) {
	if _, ok := a.authorize(w, r, serverWide, auth.PermAdmin); !ok {
		return
	}
	var out statusJSON
	out.Version, out.DataFormat = a.version, a.dataFormat
	out.Started = a.stats.started
	out.UptimeSec = int64(time.Since(a.stats.started).Seconds())
	out.Disk.FreeBytes = -1
	if a.health != nil {
		rep, _ := a.healthReport(r)
		out.Disk.FreeBytes, out.Disk.ReserveBytes, out.Disk.Low = rep.FreeBytes, rep.Reserve, rep.LowSpace
	}
	s := a.stats
	out.Requests.Total, out.Requests.S2xx, out.Requests.S3xx = s.requests.Load(), s.s2xx.Load(), s.s3xx.Load()
	out.Requests.S4xx, out.Requests.S5xx = s.s4xx.Load(), s.s5xx.Load()
	out.Bytes.In, out.Bytes.Out = s.bytesIn.Load(), s.bytesOut.Load()
	writeJSON(w, http.StatusOK, out)
}
