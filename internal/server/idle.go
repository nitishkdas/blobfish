package server

import (
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// idleTimeoutMiddleware drops transfers that stop making progress.
//
// http.Server's ReadTimeout and WriteTimeout bound a whole request, which
// would cut off a 5 GB upload or a two-hour video. Instead, the deadline is
// pushed forward on every body read and every response write: a transfer
// that keeps moving runs as long as it needs, while a client that stalls,
// or trickles one byte a minute, is dropped after d without progress.
func idleTimeoutMiddleware(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if d <= 0 {
			return next
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			rc := http.NewResponseController(w)
			// Leave no deadline behind for the next request on this
			// keep-alive connection; the server sets its own idle and
			// header deadlines between requests.
			defer func() {
				_ = rc.SetReadDeadline(time.Time{})
				_ = rc.SetWriteDeadline(time.Time{})
			}()
			r.Body = &progressReader{r: r.Body, rc: rc, d: d, h: w.Header()}
			next.ServeHTTP(&progressWriter{ResponseWriter: w, rc: rc, d: d}, r)
		})
	}
}

// progressReader extends the read deadline before each read of the body.
// SetReadDeadline is cheap (a timer update in the runtime, no syscall).
type progressReader struct {
	r  io.ReadCloser
	rc *http.ResponseController
	d  time.Duration
	h  http.Header
}

func (p *progressReader) Read(b []byte) (int, error) {
	_ = p.rc.SetReadDeadline(time.Now().Add(p.d)) // unsupported in tests' recorders: fine
	n, err := p.r.Read(b)
	if err != nil && err != io.EOF {
		// The upload broke off (stall, hang-up, size limit). Close the
		// connection after the error reply: otherwise net/http first tries
		// to read and discard the rest of the body for keep-alive, which
		// for a stalled client would wait forever and the reply would
		// never be sent.
		p.h.Set("Connection", "close")
	}
	return n, err
}

func (p *progressReader) Close() error { return p.r.Close() }

// progressWriter extends the write deadline before each response write.
type progressWriter struct {
	http.ResponseWriter
	rc *http.ResponseController
	d  time.Duration
}

func (p *progressWriter) Write(b []byte) (int, error) {
	_ = p.rc.SetWriteDeadline(time.Now().Add(p.d))
	return p.ResponseWriter.Write(b)
}

func (p *progressWriter) Unwrap() http.ResponseWriter { return p.ResponseWriter }

// LimitListener accepts at most n simultaneous connections. Further clients
// wait in the kernel's accept queue until one closes, instead of each one
// costing a goroutine, buffers and a file handle.
func LimitListener(l net.Listener, n int) net.Listener {
	return &limitListener{Listener: l, sem: make(chan struct{}, n)}
}

type limitListener struct {
	net.Listener
	sem chan struct{}
}

func (l *limitListener) Accept() (net.Conn, error) {
	l.sem <- struct{}{}
	c, err := l.Listener.Accept()
	if err != nil {
		<-l.sem
		return nil, err
	}
	return &limitConn{Conn: c, release: func() { <-l.sem }}, nil
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
