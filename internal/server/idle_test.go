package server

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
	"github.com/nitishkdas/blobfish/internal/engine"
)

const testIdle = 300 * time.Millisecond

// lockedBuffer is a bytes.Buffer safe for the server's log writes and the
// test's reads.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// idleServer runs the full stack with a short transfer idle timeout on a
// real TCP listener, so deadlines behave as in production.
func idleServer(t *testing.T) (addr, admin string, logs *lockedBuffer) {
	t.Helper()
	eng, err := engine.Open(t.TempDir(), engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { eng.Close() })
	svc := auth.NewService(eng, eng.PresignSecret())
	admin, _ = svc.EnsureAdminKey(context.Background())
	logs = &lockedBuffer{}
	srv := httptest.NewServer(New(Options{
		Logger: slog.New(slog.NewTextHandler(logs, nil)), MaxObjectSize: 1 << 30, IdleTimeout: testIdle,
		Store: eng, Buckets: eng, Auth: svc, Links: eng,
	}))
	t.Cleanup(srv.Close)
	addr = strings.TrimPrefix(srv.URL, "http://")
	req, _ := http.NewRequest("PUT", srv.URL+"/v1/buckets/idle-b", nil)
	req.Header.Set("Authorization", "Bearer "+admin)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 201 {
		t.Fatalf("create bucket: %v %v", resp, err)
	}
	return addr, admin, logs
}

func TestStalledUploadIsDropped(t *testing.T) {
	addr, admin, logs := idleServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "PUT /v1/b/idle-b/stalled HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\nContent-Length: 100000\r\n\r\n", admin)
	conn.Write(bytes.Repeat([]byte("x"), 1000)) // then go silent

	start := time.Now()
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("no response after stall (%s): %v", elapsed, err)
	}
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Errorf("status %d, want 408", resp.StatusCode)
	}
	if elapsed > 3*testIdle+500*time.Millisecond {
		t.Errorf("took %s to drop a client stalled for longer than %s", elapsed, testIdle)
	}
	// The partial upload left nothing behind, and it isn't logged as our error.
	head, _ := http.NewRequest("HEAD", "http://"+addr+"/v1/b/idle-b/stalled", nil)
	head.Header.Set("Authorization", "Bearer "+admin)
	if r, _ := http.DefaultClient.Do(head); r == nil || r.StatusCode != 404 {
		t.Errorf("stalled upload left an object behind: %v", r)
	}
	l := logs.String()
	if !strings.Contains(l, "status=408 code=RequestTimeout") || strings.Contains(l, "level=ERROR") {
		t.Errorf("logs:\n%s", l)
	}
}

func TestStalledDownloadIsDropped(t *testing.T) {
	addr, admin, _ := idleServer(t)
	const size = 64 << 20 // far more than loopback socket buffers can hold
	req, _ := http.NewRequest("PUT", "http://"+addr+"/v1/b/idle-b/big", &patternReader{n: size})
	req.Header.Set("Authorization", "Bearer "+admin)
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("seed upload: %v %v", resp, err)
	}

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.(*net.TCPConn).SetReadBuffer(64 << 10)
	fmt.Fprintf(conn, "GET /v1/b/idle-b/big HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer %s\r\n\r\n", admin)

	// Prove the download really started, then stop reading.
	br := bufio.NewReader(conn)
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(br, nil)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("download did not start: %v %v", resp, err)
	}
	first := make([]byte, 1<<20)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("read first MB: %v", err)
	}
	time.Sleep(6 * testIdle) // a reader that has stopped reading

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	n, err := io.Copy(io.Discard, resp.Body)
	n += int64(len(first))
	if n >= size {
		t.Fatalf("server sent all %d bytes to a client that stopped reading", n)
	}
	if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatalf("server kept the stalled connection open (got %d bytes, then our own deadline fired)", n)
	}
	t.Logf("stalled reader got %d of %d MB before the server gave up (%v)", n>>20, size>>20, err)
}

func TestSlowButSteadyUploadSucceeds(t *testing.T) {
	addr, admin, _ := idleServer(t)
	// 16 chunks, one every 100 ms: 1.6 s in total, well over the 300 ms idle
	// timeout, but never 300 ms without progress.
	const chunk, chunks = 64 << 10, 16
	body := strings.Repeat("s", chunk)
	want := sha256.Sum256([]byte(strings.Repeat(body, chunks)))
	pr, pw := io.Pipe()
	go func() {
		for range chunks {
			pw.Write([]byte(body))
			time.Sleep(100 * time.Millisecond)
		}
		pw.Close()
	}()
	req, _ := http.NewRequest("PUT", "http://"+addr+"/v1/b/idle-b/steady", pr)
	req.Header.Set("Authorization", "Bearer "+admin)
	req.ContentLength = chunk * chunks
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var got objectJSON
	json.NewDecoder(resp.Body).Decode(&got)
	if resp.StatusCode != 200 || got.ETag != hex.EncodeToString(want[:]) {
		t.Fatalf("status %d, etag %q", resp.StatusCode, got.ETag)
	}
}

func TestKeepAliveSurvivesPauseLongerThanIdle(t *testing.T) {
	addr, _, _ := idleServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	for i := range 2 {
		if i == 1 {
			time.Sleep(3 * testIdle) // no deadline may leak from request 1
		}
		fmt.Fprintf(conn, "GET /healthz HTTP/1.1\r\nHost: x\r\n\r\n")
		conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		resp, err := http.ReadResponse(br, nil)
		if err != nil {
			t.Fatalf("request %d on the same connection: %v", i+1, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("request %d: status %d", i+1, resp.StatusCode)
		}
	}
}

func TestLimitListener(t *testing.T) {
	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := LimitListener(raw, 2)
	defer ln.Close()
	accepted := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			accepted <- c
		}
	}()
	for range 3 {
		c, err := net.Dial("tcp", raw.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
	}
	got := []net.Conn{<-accepted, <-accepted}
	select {
	case <-accepted:
		t.Fatal("accepted a third connection while two were open")
	case <-time.After(200 * time.Millisecond):
	}
	got[0].Close() // frees a slot; the queued client gets in
	select {
	case c := <-accepted:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("queued connection was never accepted after a slot freed")
	}
	got[1].Close()
}
