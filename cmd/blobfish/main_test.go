package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunVersion(t *testing.T) {
	var out bytes.Buffer
	if err := run(context.Background(), []string{"-version"}, func(string) string { return "" }, &out, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.String(), "blobfish ") {
		t.Errorf("output = %q", out.String())
	}
}

func TestRunBadConfig(t *testing.T) {
	env := map[string]string{"BLOBFISH_LOG_FORMAT": "xml"}
	err := run(context.Background(), nil, func(k string) string { return env[k] }, &bytes.Buffer{}, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "log_format") {
		t.Fatalf("run() error = %v, want log_format error", err)
	}
}

// TestRunServesAndShutsDown starts the real server, hits /healthz, then
// cancels the context (as SIGTERM would) and expects a clean exit.
func TestRunServesAndShutsDown(t *testing.T) {
	addr := freeAddr(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	env := map[string]string{"BLOBFISH_LISTEN": addr, "BLOBFISH_DATA_DIR": dataDir}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- run(ctx, nil, func(k string) string { return env[k] }, &bytes.Buffer{}, &bytes.Buffer{})
	}()

	url := "http://" + addr + "/healthz"
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("healthz status = %d", resp.StatusCode)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never came up: %v", err)
		}
		select {
		case err := <-done:
			t.Fatalf("run exited early: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run() = %v, want nil after shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not return after cancel")
	}
}

func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().String()
}
