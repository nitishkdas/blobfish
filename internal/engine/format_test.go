package engine

import (
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func rawDB(t *testing.T, dir string, fn func(tx *bbolt.Tx) error) {
	t.Helper()
	db, err := bbolt.Open(filepath.Join(dir, "meta.db"), 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Update(fn); err != nil {
		t.Fatal(err)
	}
}

func storedFormat(t *testing.T, dir string) string {
	t.Helper()
	var v string
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		if b := tx.Bucket(bktSettings); b != nil {
			v = string(b.Get(setFormat))
		}
		return nil
	})
	return v
}

func TestFormatStampedOnNewDataDir(t *testing.T) {
	dir := t.TempDir()
	openTest(t, dir).Close()
	if v := storedFormat(t, dir); v != strconv.Itoa(FormatVersion) {
		t.Errorf("format_version = %q, want %d", v, FormatVersion)
	}
}

func TestFormatPreVersioningDataIsKept(t *testing.T) {
	e, dir := newEngine(t)
	put(t, e, "k", "precious")
	e.Close()
	rawDB(t, dir, func(tx *bbolt.Tx) error { return tx.Bucket(bktSettings).Delete(setFormat) })

	e = openTest(t, dir) // like a data dir made before versioning existed
	if got, _ := readAll(t, e, "k", nil); got != "precious" {
		t.Errorf("data after stamping: %q", got)
	}
	e.Close()
	if v := storedFormat(t, dir); v != strconv.Itoa(FormatVersion) {
		t.Errorf("format_version = %q, want %d", v, FormatVersion)
	}
}

func TestFormatTooNewRefusedWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	// A data dir from a hypothetical newer BlobFish: only its settings.
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucket(bktSettings)
		if err != nil {
			return err
		}
		return b.Put(setFormat, []byte("99"))
	})
	e, err := Open(dir, Options{})
	if err == nil {
		e.Close()
		t.Fatal("opened a data dir with a newer format")
	}
	if !errors.Is(err, ErrFormatTooNew) || !strings.Contains(err.Error(), "99") {
		t.Errorf("err = %v", err)
	}
	var names []string
	rawDB(t, dir, func(tx *bbolt.Tx) error {
		return tx.ForEach(func(name []byte, _ *bbolt.Bucket) error {
			names = append(names, string(name))
			return nil
		})
	})
	if len(names) != 1 || names[0] != "settings" {
		t.Errorf("refused open still wrote to meta.db: buckets %v", names)
	}
}

func TestUpgradeFormatSteps(t *testing.T) {
	mark := func(name string) migration {
		return func(tx *bbolt.Tx) error { _, err := tx.CreateBucket([]byte(name)); return err }
	}
	fail := func(*bbolt.Tx) error { return errors.New("boom") }
	tests := []struct {
		name      string
		start     string // stored version, "" = none
		target    int
		steps     map[int]migration
		wantErr   string
		wantStamp string
		wantMarks []string
	}{
		{"already current", "1", 1, nil, "", "1", nil},
		{"unstamped is v1", "", 1, nil, "", "1", nil},
		{"two steps in order", "1", 3, map[int]migration{1: mark("m1"), 2: mark("m2")}, "", "3", []string{"m1", "m2"}},
		{"missing step", "1", 3, map[int]migration{1: mark("m1")}, "no upgrade step from data format 2", "1", nil},
		{"failing step rolls back all", "1", 3, map[int]migration{1: mark("m1"), 2: fail}, "boom", "1", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			rawDB(t, dir, func(tx *bbolt.Tx) error {
				b, err := tx.CreateBucket(bktSettings)
				if err != nil || tt.start == "" {
					return err
				}
				return b.Put(setFormat, []byte(tt.start))
			})
			db, err := bbolt.Open(filepath.Join(dir, "meta.db"), 0o600, nil)
			if err != nil {
				t.Fatal(err)
			}
			err = db.Update(func(tx *bbolt.Tx) error {
				_, err := upgradeFormat(tx, tt.target, tt.steps)
				return err
			})
			db.Close()
			if tt.wantErr == "" && err != nil || tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tt.wantErr)
			}
			if v := storedFormat(t, dir); v != tt.wantStamp && !(tt.wantErr != "" && tt.start == v) {
				t.Errorf("stamp = %q, want %q", v, tt.wantStamp)
			}
			var marks []string
			rawDB(t, dir, func(tx *bbolt.Tx) error {
				for _, m := range []string{"m1", "m2"} {
					if tx.Bucket([]byte(m)) != nil {
						marks = append(marks, m)
					}
				}
				return nil
			})
			if strings.Join(marks, ",") != strings.Join(tt.wantMarks, ",") {
				t.Errorf("steps applied = %v, want %v", marks, tt.wantMarks)
			}
		})
	}
}
