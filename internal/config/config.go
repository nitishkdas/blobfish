// Package config loads BlobFish server configuration.
//
// Values are resolved in this order, each layer overriding the previous one:
// built-in defaults, an optional JSON config file, then BLOBFISH_* environment
// variables.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Config is the full server configuration.
type Config struct {
	// Listen is the TCP address the HTTP server binds to. The default,
	// "127.0.0.1:8080", is reachable only from this machine: BlobFish
	// speaks plain HTTP and expects a TLS proxy (Caddy) in front. Use
	// ":8080" in a container, or to reach it from other machines.
	Listen string `json:"listen"`
	// DataDir is the folder holding meta.db, objects/ and tmp/.
	DataDir string `json:"data_dir"`
	// MaxObjectSize is the largest accepted request body, in bytes.
	MaxObjectSize int64 `json:"max_object_size"`
	// LogLevel is one of debug, info, warn, error.
	LogLevel string `json:"log_level"`
	// LogFormat is "text" or "json".
	LogFormat string `json:"log_format"`
	// PublicURL is the base URL clients use to reach this server, e.g.
	// "https://files.example.com". Used to build presigned URLs. When empty,
	// the request's Host header is used.
	PublicURL string `json:"public_url"`
	// TransferIdleTimeout drops a connection whose body upload or download
	// makes no progress for this long (e.g. "60s"). The timer restarts on
	// every chunk, so long transfers that keep moving are never cut off.
	TransferIdleTimeout Duration `json:"transfer_idle_timeout"`
	// MaxConnections caps simultaneously open client connections; extra
	// clients wait in the kernel's accept queue instead of exhausting
	// memory or file handles.
	MaxConnections int `json:"max_connections"`
	// MinFreeSpace is disk space, in bytes, that uploads may never use, so
	// deletes and metadata updates always fit. 0 disables the check.
	MinFreeSpace int64 `json:"min_free_space"`
	// TrashDays keeps deleted objects restorable for this many days before
	// they are deleted for good. 0 turns trash off.
	TrashDays int `json:"trash_days"`
	// TrustedProxies lists reverse proxies (addresses or CIDR ranges) whose
	// X-Forwarded-For / X-Forwarded-Proto headers are believed, e.g.
	// ["127.0.0.1"] when Caddy runs on the same machine.
	TrustedProxies []string `json:"trusted_proxies"`
	// OperatorName and PrivacyURL are shown on upload-link pages, so
	// people sending files know who receives them and where to read how
	// their data is handled (privacy laws expect this notice).
	OperatorName string `json:"operator_name"`
	PrivacyURL   string `json:"privacy_url"`
}

// MaxTrashDays caps trash_days (ten years).
const MaxTrashDays = 3650

// TrashRetention is TrashDays as a duration; 0 means trash is off.
func (c Config) TrashRetention() time.Duration {
	return time.Duration(c.TrashDays) * 24 * time.Hour
}

// Duration is a time.Duration written as a string like "60s" or "2m" in
// the JSON config.
type Duration time.Duration

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = Duration(v)
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(time.Duration(d).String()), nil }

// Default returns the built-in configuration.
func Default() Config {
	return Config{
		Listen:        "127.0.0.1:8080",
		DataDir:       "data",
		MaxObjectSize: 5 << 30, // 5 GiB
		LogLevel:      "info",
		LogFormat:     "text",

		TransferIdleTimeout: Duration(60 * time.Second),
		MaxConnections:      1000,
		MinFreeSpace:        512 << 20, // 512 MiB
		TrashDays:           7,
	}
}

// Load builds a Config from defaults, the JSON file at path (skipped when
// path is empty) and environment variables read through getenv. Passing
// getenv explicitly keeps Load free of global state and easy to test.
func Load(path string, getenv func(string) string) (Config, error) {
	cfg := Default()

	if path != "" {
		f, err := os.Open(path)
		if err != nil {
			return Config{}, fmt.Errorf("open config file: %w", err)
		}
		defer f.Close()
		if err := decodeJSON(f, &cfg); err != nil {
			return Config{}, fmt.Errorf("parse config file %s: %w", path, err)
		}
	}

	if err := applyEnv(&cfg, getenv); err != nil {
		return Config{}, err
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func decodeJSON(r io.Reader, cfg *Config) error {
	data, err := io.ReadAll(io.LimitReader(r, 1<<20)) // config files are tiny
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields() // catch typos like "data_directory"
	if err := dec.Decode(cfg); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("unexpected data after JSON object")
	}
	return nil
}

func applyEnv(cfg *Config, getenv func(string) string) error {
	if v := getenv("BLOBFISH_LISTEN"); v != "" {
		cfg.Listen = v
	}
	if v := getenv("BLOBFISH_DATA_DIR"); v != "" {
		cfg.DataDir = v
	}
	if v := getenv("BLOBFISH_MAX_OBJECT_SIZE"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("BLOBFISH_MAX_OBJECT_SIZE: %w", err)
		}
		cfg.MaxObjectSize = n
	}
	if v := getenv("BLOBFISH_LOG_LEVEL"); v != "" {
		cfg.LogLevel = v
	}
	if v := getenv("BLOBFISH_LOG_FORMAT"); v != "" {
		cfg.LogFormat = v
	}
	if v := getenv("BLOBFISH_PUBLIC_URL"); v != "" {
		cfg.PublicURL = v
	}
	if v := getenv("BLOBFISH_TRANSFER_IDLE_TIMEOUT"); v != "" {
		if err := cfg.TransferIdleTimeout.UnmarshalText([]byte(v)); err != nil {
			return fmt.Errorf("BLOBFISH_TRANSFER_IDLE_TIMEOUT: %w", err)
		}
	}
	if v := getenv("BLOBFISH_TRUSTED_PROXIES"); v != "" {
		cfg.TrustedProxies = strings.Split(v, ",")
	}
	if v := getenv("BLOBFISH_MIN_FREE_SPACE"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("BLOBFISH_MIN_FREE_SPACE: %w", err)
		}
		cfg.MinFreeSpace = n
	}
	if v := getenv("BLOBFISH_TRASH_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BLOBFISH_TRASH_DAYS: %w", err)
		}
		cfg.TrashDays = n
	}
	if v := getenv("BLOBFISH_OPERATOR_NAME"); v != "" {
		cfg.OperatorName = v
	}
	if v := getenv("BLOBFISH_PRIVACY_URL"); v != "" {
		cfg.PrivacyURL = v
	}
	if v := getenv("BLOBFISH_MAX_CONNECTIONS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("BLOBFISH_MAX_CONNECTIONS: %w", err)
		}
		cfg.MaxConnections = n
	}
	return nil
}

// Validate reports the first invalid field, if any.
func (c Config) Validate() error {
	if c.Listen == "" {
		return errors.New("config: listen must not be empty")
	}
	if c.DataDir == "" {
		return errors.New("config: data_dir must not be empty")
	}
	if c.MaxObjectSize <= 0 {
		return fmt.Errorf("config: max_object_size must be positive, got %d", c.MaxObjectSize)
	}
	if _, err := c.SlogLevel(); err != nil {
		return err
	}
	switch c.LogFormat {
	case "text", "json":
	default:
		return fmt.Errorf("config: log_format must be text or json, got %q", c.LogFormat)
	}
	if time.Duration(c.TransferIdleTimeout) < time.Second {
		return fmt.Errorf("config: transfer_idle_timeout must be at least 1s, got %s", time.Duration(c.TransferIdleTimeout))
	}
	for _, p := range c.TrustedProxies {
		p = strings.TrimSpace(p)
		if _, err := netip.ParsePrefix(p); err == nil {
			continue
		}
		if _, err := netip.ParseAddr(p); err != nil {
			return fmt.Errorf("config: trusted_proxies entry %q is not an IP address or CIDR range", p)
		}
	}
	if c.MinFreeSpace < 0 {
		return fmt.Errorf("config: min_free_space must be 0 or more bytes, got %d", c.MinFreeSpace)
	}
	if c.TrashDays < 0 || c.TrashDays > MaxTrashDays {
		return fmt.Errorf("config: trash_days must be 0 (off) to %d, got %d", MaxTrashDays, c.TrashDays)
	}
	if c.MaxConnections < 1 {
		return fmt.Errorf("config: max_connections must be at least 1, got %d", c.MaxConnections)
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" ||
			(u.Path != "" && u.Path != "/") || u.RawQuery != "" {
			return fmt.Errorf("config: public_url must look like https://host[:port], got %q", c.PublicURL)
		}
	}
	if len(c.OperatorName) > 200 || strings.ContainsFunc(c.OperatorName, unicode.IsControl) {
		return errors.New("config: operator_name must be at most 200 bytes, without control characters")
	}
	if c.PrivacyURL != "" {
		u, err := url.Parse(c.PrivacyURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return fmt.Errorf("config: privacy_url must be an http(s) address, got %q", c.PrivacyURL)
		}
	}
	return nil
}

// SlogLevel converts LogLevel to a slog.Level.
func (c Config) SlogLevel() (slog.Level, error) {
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(strings.ToUpper(c.LogLevel))); err != nil {
		return 0, fmt.Errorf("config: log_level must be debug, info, warn or error, got %q", c.LogLevel)
	}
	return lvl, nil
}
