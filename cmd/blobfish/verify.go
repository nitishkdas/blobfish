package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/nitishkdas/blobfish/internal/config"
	"github.com/nitishkdas/blobfish/internal/engine"
)

// errVerifyFailed means verify ran to the end and found damage.
var errVerifyFailed = errors.New("verify found problems")

// runVerify: `blobfish verify [-bucket NAME]` with the server stopped.
// Reads every object back from disk and checks its SHA-256. Never writes.
func runVerify(ctx context.Context, args []string, getenv func(string) string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("blobfish verify", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", getenv("BLOBFISH_CONFIG"), "path to JSON config file (env BLOBFISH_CONFIG)")
	bucket := fs.String("bucket", "", "verify only this bucket")
	fs.Usage = func() {
		fmt.Fprintln(stderr, "Usage: blobfish verify [-bucket NAME] [-config FILE]")
		fmt.Fprintln(stderr, "Reads every object from disk and checks it against the SHA-256 recorded")
		fmt.Fprintln(stderr, "when it was written; also checks meta.db. Changes nothing. The server must")
		fmt.Fprintln(stderr, "be stopped. Exits 0 if all is well, 1 on problems or if it cannot run.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		fs.Usage()
		return fmt.Errorf("verify: unexpected argument %q", fs.Arg(0))
	}
	cfg, err := config.Load(*configPath, getenv)
	if err != nil {
		return err
	}

	start := time.Now()
	fmt.Fprintf(stderr, "Verifying %s ...\n", cfg.DataDir)
	st, err := engine.Verify(ctx, cfg.DataDir, engine.VerifyOptions{
		Bucket:        *bucket,
		Problem:       func(p engine.VerifyProblem) { fmt.Fprintln(stdout, p) },
		ProgressEvery: 10 * time.Second,
		Progress: func(s engine.VerifyStats) {
			fmt.Fprintf(stderr, "  %d objects, %s checked, %d problems\n", s.Objects, formatBytes(s.Bytes), s.Problems)
		},
	})
	if err != nil {
		return fmt.Errorf("verify: %w", err)
	}

	took := time.Since(start).Round(time.Second)
	if st.OrphanBlobs > 0 {
		fmt.Fprintf(stdout, "%d leftover blob file(s) not used by any object; the server removes them at its next start.\n", st.OrphanBlobs)
	}
	if st.Problems > 0 {
		fmt.Fprintf(stdout, "FAILED: %d problem(s) in %d objects (%s) across %d bucket(s), in %s.\n",
			st.Problems, st.Objects, formatBytes(st.Bytes), st.Buckets, took)
		fmt.Fprintln(stdout, "Restore damaged objects from a backup or upload them again. If any are")
		fmt.Fprintln(stdout, `"unindexed", don't start the server before saving them: it would delete them.`)
		return errVerifyFailed
	}
	fmt.Fprintf(stdout, "OK: %d objects (%s) across %d bucket(s) match their checksums, in %s.\n",
		st.Objects, formatBytes(st.Bytes), st.Buckets, took)
	return nil
}

// formatBytes prints a size in binary units, e.g. "1.5 GiB".
func formatBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
