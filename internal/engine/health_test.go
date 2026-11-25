package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestHealth(t *testing.T) {
	ctx := context.Background()

	t.Run("healthy", func(t *testing.T) {
		e, dir := newEngine(t)
		rep := e.Health(ctx)
		if rep.Metadata != nil || rep.Disk != nil || rep.FreeBytes <= 0 || rep.LowSpace {
			t.Fatalf("report = %+v", rep)
		}
		if n := countFiles(t, filepath.Join(dir, "tmp")); n != 0 {
			t.Errorf("probe left %d files in tmp", n)
		}
	})

	t.Run("read-only disk", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root ignores directory permissions")
		}
		e, dir := newEngine(t)
		tmp := filepath.Join(dir, "tmp")
		os.Chmod(tmp, 0o500)
		t.Cleanup(func() { os.Chmod(tmp, 0o750) })
		if rep := e.Health(ctx); rep.Disk == nil || rep.Metadata != nil {
			t.Fatalf("report = %+v, want a disk failure only", rep)
		}
	})

	t.Run("metadata unavailable", func(t *testing.T) {
		e, _ := newEngine(t)
		e.db.Close()
		if rep := e.Health(ctx); rep.Metadata == nil {
			t.Fatalf("closed database reported healthy: %+v", rep)
		}
	})

	t.Run("low space", func(t *testing.T) {
		e, _ := newEngine(t)
		fakeSpace(e, func() uint64 { return 50 * mib }) // reserve is 100 MiB
		if rep := e.Health(ctx); !rep.LowSpace || rep.FreeBytes != 50*mib || rep.Reserve != 100*mib {
			t.Fatalf("report = %+v", rep)
		}
	})
}
