package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/engine"
	"github.com/nitishkdas/blobfish/internal/store"
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

func TestBackupAndRestoreCommands(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	env := func(k string) string {
		return map[string]string{"BLOBFISH_DATA_DIR": dataDir}[k]
	}
	// Seed data the way a stopped server would leave it.
	eng, err := engine.Open(dataDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	eng.CreateBucket(ctx, "cli-b")
	eng.Put(ctx, "cli-b", "k", strings.NewReader("hello"), store.Meta{})

	out := filepath.Join(t.TempDir(), "b.tar")
	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"backup", "-o", out}, env, &stdout, &stderr); err == nil ||
		!strings.Contains(err.Error(), "/v1/backup") {
		t.Fatalf("backup with the data dir in use: err = %v, want a hint to use the API", err)
	}
	eng.Close()

	if err := run(ctx, []string{"backup", "-o", out}, env, &stdout, &stderr); err != nil {
		t.Fatalf("backup: %v\n%s", err, stderr.String())
	}
	if _, err := os.Stat(out + ".partial"); !os.IsNotExist(err) {
		t.Error("left a .partial file behind")
	}
	target := filepath.Join(t.TempDir(), "restored")
	stdout.Reset()
	if err := run(ctx, []string{"restore", "-from", out, "-data-dir", target}, env, &stdout, &stderr); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if !strings.Contains(stdout.String(), "verified 1 objects") {
		t.Errorf("restore output: %s", stdout.String())
	}
	if err := run(ctx, []string{"restore", "-from", out, "-data-dir", target}, env, &stdout, &stderr); err == nil ||
		!strings.Contains(err.Error(), "not empty") {
		t.Errorf("restore into a non-empty dir: %v", err)
	}
	if err := run(ctx, []string{"backup"}, env, &stdout, &stderr); err == nil {
		t.Error("backup without -o succeeded")
	}
}
