package store

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

func TestValidateBucketName(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"abc", true},
		{"invoices-2026", true},
		{"my.bucket.1", true},
		{"ab", false},
		{strings.Repeat("a", 64), false},
		{strings.Repeat("a", 63), true},
		{"Upper", false},
		{"-start", false},
		{"end-", false},
		{"a..b", false},
		{"has space", false},
		{"under_score", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateBucketName(tt.name)
			if tt.ok != (err == nil) {
				t.Fatalf("ValidateBucketName(%q) = %v, want ok=%v", tt.name, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidBucketName) {
				t.Errorf("error %v does not wrap ErrInvalidBucketName", err)
			}
		})
	}
}

func TestValidateKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		ok   bool
	}{
		{"simple", "a.txt", true},
		{"nested", "2026/11/invoice #1.pdf", true},
		{"dots", "../../etc/passwd", true}, // keys never touch the filesystem
		{"unicode", "fïsh/🐟", true},
		{"max len", strings.Repeat("k", MaxKeyLen), true},
		{"empty", "", false},
		{"too long", strings.Repeat("k", MaxKeyLen+1), false},
		{"bad utf8", "a\xffb", false},
		{"nul", "a\x00b", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := ValidateKey(tt.key)
			if tt.ok != (err == nil) {
				t.Fatalf("ValidateKey(%q) = %v, want ok=%v", tt.key, err, tt.ok)
			}
			if err != nil && !errors.Is(err, ErrInvalidKey) {
				t.Errorf("error %v does not wrap ErrInvalidKey", err)
			}
		})
	}
}

func TestValidateMeta(t *testing.T) {
	ok := Meta{User: map[string]string{"a": strings.Repeat("v", MaxUserMetaBytes-1)}}
	if err := ValidateMeta(ok); err != nil {
		t.Errorf("at limit: %v", err)
	}
	big := Meta{User: map[string]string{"ab": strings.Repeat("v", MaxUserMetaBytes-1)}}
	if err := ValidateMeta(big); !errors.Is(err, ErrMetadataTooLarge) {
		t.Errorf("over limit: %v, want ErrMetadataTooLarge", err)
	}
}

func TestRangeResolve(t *testing.T) {
	tests := []struct {
		name           string
		r              Range
		size           int64
		wantOff, wantN int64
		wantErr        bool
	}{
		{"whole explicit", Range{0, 9}, 10, 0, 10, false},
		{"middle", Range{2, 4}, 10, 2, 3, false},
		{"open end", Range{7, -1}, 10, 7, 3, false},
		{"end past size clamps", Range{5, 100}, 10, 5, 5, false},
		{"single byte", Range{9, 9}, 10, 9, 1, false},
		{"suffix", Range{-3, 0}, 10, 7, 3, false},
		{"suffix larger than size", Range{-50, 0}, 10, 0, 10, false},
		{"start at size", Range{10, -1}, 10, 0, 0, true},
		{"end before start", Range{5, 2}, 10, 0, 0, true},
		{"empty object", Range{0, -1}, 0, 0, 0, true},
		{"suffix on empty", Range{-5, 0}, 0, 0, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			off, n, err := tt.r.Resolve(tt.size)
			if tt.wantErr {
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("Resolve() error = %v, want ErrInvalidRange", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Resolve() unexpected error: %v", err)
			}
			if off != tt.wantOff || n != tt.wantN {
				t.Errorf("Resolve() = (%d, %d), want (%d, %d)", off, n, tt.wantOff, tt.wantN)
			}
		})
	}
}

func TestNormalizeCORSOrigins(t *testing.T) {
	got, err := NormalizeCORSOrigins([]string{" https://App.Example.com ", "http://localhost:3000", "https://app.example.com", ""})
	if err != nil || strings.Join(got, ",") != "http://localhost:3000,https://app.example.com" {
		t.Fatalf("got %v, %v", got, err)
	}
	if got, err := NormalizeCORSOrigins(nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("empty list: %v, %v", got, err)
	}
	for _, bad := range []string{"app.example.com", "https://app.example.com/", "https://a.com/x", "ftp://a.com", "https://a.com?x=1", "https://u:p@a.com", "mailto:a@b.c"} {
		if _, err := NormalizeCORSOrigins([]string{bad}); !errors.Is(err, ErrInvalidOrigin) {
			t.Errorf("%q accepted: %v", bad, err)
		}
	}
	many := make([]string, MaxCORSOrigins+1)
	for i := range many {
		many[i] = fmt.Sprintf("https://site%d.example", i)
	}
	if _, err := NormalizeCORSOrigins(many); !errors.Is(err, ErrInvalidOrigin) {
		t.Errorf("too many origins accepted: %v", err)
	}
}
