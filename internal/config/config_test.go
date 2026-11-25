package config

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func writeFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "blobfish.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		file    string // empty means no config file
		env     map[string]string
		want    func(Config) Config // mutates defaults into the expected value
		wantErr string
	}{
		{
			name: "defaults",
			want: func(c Config) Config { return c },
		},
		{
			name: "file overrides defaults",
			file: `{"listen": "127.0.0.1:9000", "data_dir": "/srv/blobfish", "max_object_size": 1024}`,
			want: func(c Config) Config {
				c.Listen, c.DataDir, c.MaxObjectSize = "127.0.0.1:9000", "/srv/blobfish", 1024
				return c
			},
		},
		{
			name: "env overrides file",
			file: `{"listen": ":9000", "log_level": "debug"}`,
			env: map[string]string{
				"BLOBFISH_LISTEN":          ":7000",
				"BLOBFISH_MAX_OBJECT_SIZE": "42",
				"BLOBFISH_LOG_FORMAT":      "json",
			},
			want: func(c Config) Config {
				c.Listen, c.MaxObjectSize, c.LogLevel, c.LogFormat = ":7000", 42, "debug", "json"
				return c
			},
		},
		{name: "unknown field", file: `{"data_directory": "x"}`, wantErr: "unknown field"},
		{name: "trailing data", file: `{} {}`, wantErr: "unexpected data"},
		{name: "bad size env", env: map[string]string{"BLOBFISH_MAX_OBJECT_SIZE": "5GB"}, wantErr: "BLOBFISH_MAX_OBJECT_SIZE"},
		{name: "zero size", file: `{"max_object_size": 0}`, wantErr: "max_object_size"},
		{name: "bad log level", env: map[string]string{"BLOBFISH_LOG_LEVEL": "loud"}, wantErr: "log_level"},
		{name: "bad log format", file: `{"log_format": "xml"}`, wantErr: "log_format"},
		{name: "empty listen", file: `{"listen": ""}`, wantErr: "listen"},
		{
			name: "public url",
			env:  map[string]string{"BLOBFISH_PUBLIC_URL": "https://files.example.com"},
			want: func(c Config) Config { c.PublicURL = "https://files.example.com"; return c },
		},
		{
			name: "transfer limits from file and env",
			file: `{"transfer_idle_timeout": "2m", "max_connections": 50}`,
			env:  map[string]string{"BLOBFISH_MAX_CONNECTIONS": "75"},
			want: func(c Config) Config {
				c.TransferIdleTimeout, c.MaxConnections = Duration(2*time.Minute), 75
				return c
			},
		},
		{name: "idle timeout env", env: map[string]string{"BLOBFISH_TRANSFER_IDLE_TIMEOUT": "90s"},
			want: func(c Config) Config { c.TransferIdleTimeout = Duration(90 * time.Second); return c }},
		{name: "idle timeout bad", file: `{"transfer_idle_timeout": "soon"}`, wantErr: "invalid duration"},
		{name: "idle timeout too small", file: `{"transfer_idle_timeout": "10ms"}`, wantErr: "transfer_idle_timeout"},
		{name: "zero connections", env: map[string]string{"BLOBFISH_MAX_CONNECTIONS": "0"}, wantErr: "max_connections"},
		{name: "public url no scheme", file: `{"public_url": "files.example.com"}`, wantErr: "public_url"},
		{name: "public url with path", file: `{"public_url": "https://x.com/sub"}`, wantErr: "public_url"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := ""
			if tt.file != "" {
				path = writeFile(t, tt.file)
			}
			got, err := Load(path, envMap(tt.env))
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Load() error = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			if want := tt.want(Default()); got != want {
				t.Errorf("Load() = %+v, want %+v", got, want)
			}
		})
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.json"), envMap(nil))
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Load() error = %v, want fs.ErrNotExist", err)
	}
}
