package engine

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCache asks the kernel to forget cached pages of f, so the next read
// comes from the disk. Best effort: errors are ignored.
func dropCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED)
}
