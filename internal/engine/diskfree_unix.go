//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package engine

import "syscall"

// diskFree returns the bytes available to an unprivileged user on the file
// system holding path.
func diskFree(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return freeFromStatfs(uint64(st.Blocks), uint64(st.Bavail), uint64(st.Bsize))
}
