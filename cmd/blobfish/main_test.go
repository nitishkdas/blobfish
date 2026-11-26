package main

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nitishkdas/blobfish/internal/auth"
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

func TestBootstrapKeyGoesToFileNotLog(t *testing.T) {
	addr := freeAddr(t)
	dataDir := filepath.Join(t.TempDir(), "data")
	env := map[string]string{"BLOBFISH_LISTEN": addr, "BLOBFISH_DATA_DIR": dataDir}
	getenv := func(k string) string { return env[k] }

	for start := 1; start <= 2; start++ {
		var stderr lockedBuf
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- run(ctx, nil, getenv, &bytes.Buffer{}, &stderr) }()
		waitHealthy(t, addr, done)
		cancel()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		logs := stderr.String()
		if strings.Contains(logs, "bf_") {
			t.Fatalf("start %d: the admin key appeared in the log output:\n%s", start, logs)
		}
		path := filepath.Join(dataDir, "initial-admin-key")
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("start %d: key file: %v", start, err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("key file mode %v, want 0600", st.Mode().Perm())
		}
		switch start {
		case 1:
			if !strings.Contains(logs, "admin key was created") || !strings.Contains(logs, path) {
				t.Errorf("first start should say where the key is:\n%s", logs)
			}
		case 2:
			if !strings.Contains(logs, "still on disk") {
				t.Errorf("second start should remind to delete the file:\n%s", logs)
			}
		}
	}
	// The saved key really is an admin key.
	key, _ := os.ReadFile(filepath.Join(dataDir, "initial-admin-key"))
	eng, err := engine.Open(dataDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	keys, _ := eng.ListKeys(context.Background())
	if len(keys) != 1 || keys[0].Scopes[0] != "*:admin" || !strings.HasPrefix(strings.TrimSpace(string(key)), "bf_") {
		t.Errorf("keys = %+v, file = %q", keys, key)
	}
}

// lockedBuf is a bytes.Buffer safe for the server's goroutines.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func waitHealthy(t *testing.T, addr string, done chan error) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-done:
			t.Fatalf("run exited early: %v", err)
		default:
		}
		if resp, err := http.Get("http://" + addr + "/healthz"); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("server never came up")
}

func TestVerifyCommand(t *testing.T) {
	dataDir := filepath.Join(t.TempDir(), "data")
	env := func(k string) string {
		return map[string]string{"BLOBFISH_DATA_DIR": dataDir}[k]
	}
	eng, err := engine.Open(dataDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	eng.CreateBucket(ctx, "cli-b")
	eng.Put(ctx, "cli-b", "docs/a.txt", strings.NewReader("hello"), store.Meta{})

	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"verify"}, env, &stdout, &stderr); err == nil ||
		!strings.Contains(err.Error(), "stop it first") {
		t.Fatalf("verify with the server running: err = %v", err)
	}
	eng.Close()

	stdout.Reset()
	if err := run(ctx, []string{"verify"}, env, &stdout, &stderr); err != nil {
		t.Fatalf("verify: %v\n%s", err, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "OK: 1 objects (5 B) across 1 bucket(s)") {
		t.Errorf("clean output: %s", stdout.String())
	}

	// Rot one byte of the only blob.
	var blob string
	filepath.WalkDir(filepath.Join(dataDir, "objects"), func(p string, d os.DirEntry, _ error) error {
		if !d.IsDir() {
			blob = p
		}
		return nil
	})
	os.WriteFile(blob, []byte("jello"), 0o600)
	stdout.Reset()
	if err := run(ctx, []string{"verify"}, env, &stdout, &stderr); err != errVerifyFailed {
		t.Fatalf("verify of damaged data: err = %v", err)
	}
	if out := stdout.String(); !strings.Contains(out, "checksum  cli-b/docs/a.txt: sha256 is") ||
		!strings.Contains(out, "FAILED: 1 problem(s)") {
		t.Errorf("damaged output: %s", out)
	}
	if err := run(ctx, []string{"verify", "extra"}, env, &stdout, &stderr); err == nil {
		t.Error("verify accepted a stray argument")
	}
}

func TestResetPasswordCommand(t *testing.T) {
	auth.UseCheapPasswordHashForTests()
	dataDir := filepath.Join(t.TempDir(), "data")
	env := func(k string) string { return map[string]string{"BLOBFISH_DATA_DIR": dataDir}[k] }
	ctx := context.Background()
	eng, err := engine.Open(dataDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	svc := auth.NewService(eng, eng.PresignSecret()).UseUsers(eng)
	if _, err := svc.CreateUser(ctx, "maya", "forgotten password", []string{"*:admin"}); err != nil {
		t.Fatal(err)
	}
	old, _, _, err := svc.Login(ctx, "maya", "forgotten password", "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	eng.Close()

	var stdout, stderr bytes.Buffer
	if err := run(ctx, []string{"-reset-password", "nobody"}, env, &stdout, &stderr); err == nil {
		t.Fatal("reset for an unknown user succeeded")
	}
	stdout.Reset()
	if err := run(ctx, []string{"-reset-password", "Maya"}, env, &stdout, &stderr); err != nil {
		t.Fatalf("reset: %v\n%s", err, stderr.String())
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	pw := lines[len(lines)-1]
	if len(pw) != 29 || strings.Count(pw, "-") != 3 {
		t.Fatalf("printed password = %q", pw)
	}

	eng, err = engine.Open(dataDir, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer eng.Close()
	svc = auth.NewService(eng, eng.PresignSecret()).UseUsers(eng)
	if _, _, _, err := svc.Login(ctx, "maya", pw, "1.1.1.1"); err != nil {
		t.Errorf("new password doesn't work: %v", err)
	}
	if _, _, _, err := svc.Login(ctx, "maya", "forgotten password", "1.1.1.1"); err == nil {
		t.Error("old password still works")
	}
	r := httptest.NewRequest("GET", "/v1/whoami", nil)
	r.Header.Set("Authorization", "Bearer "+old)
	if _, err := svc.Authenticate(r, auth.Target{}); err == nil {
		t.Error("old session survived the reset")
	}
}
