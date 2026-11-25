package engine

import (
	"errors"
	"fmt"
	"io"

	"github.com/nitishkdas/blobfish/internal/store"
)

// spaceCheckEvery is how much an upload may write between free-space checks.
const spaceCheckEvery = 64 << 20

// checkSpace returns store.ErrInsufficientStorage if writing need more bytes
// would leave less than the configured reserve free. If free space can't be
// measured (unsupported platform), it allows the write.
func (e *Engine) checkSpace(need int64) error {
	if e.minFree <= 0 {
		return nil
	}
	free, err := e.freeSpace()
	if err != nil {
		e.log.Debug("free space unknown; not enforcing reserve", "err", err)
		return nil
	}
	if int64(free)-max(need, 0) < e.minFree {
		return fmt.Errorf("%w: %s free, upload needs %s and %s stays reserved",
			store.ErrInsufficientStorage, human(int64(free)), human(max(need, 0)), human(e.minFree))
	}
	return nil
}

// spaceGuard passes writes through, checking free space every
// spaceCheckEvery bytes.
type spaceGuard struct {
	w       io.Writer
	e       *Engine
	pending int64
}

func (g *spaceGuard) Write(p []byte) (int, error) {
	g.pending += int64(len(p))
	if g.pending >= spaceCheckEvery {
		g.pending = 0
		if err := g.e.checkSpace(0); err != nil {
			return 0, err
		}
	}
	return g.w.Write(p)
}

func human(n int64) string {
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

// errCapacityUnknown: the file system reports a total size of zero, which
// some FUSE and network file systems do. Their free space is unknown, not
// zero; treating it as zero would refuse every upload.
var errCapacityUnknown = errors.New("file system does not report its capacity; free-space reserve not enforced")

func freeFromStatfs(blocks, avail, bsize uint64) (uint64, error) {
	if blocks == 0 {
		return 0, errCapacityUnknown
	}
	return avail * bsize, nil
}
