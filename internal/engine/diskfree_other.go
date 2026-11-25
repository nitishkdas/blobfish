//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly)

package engine

import "errors"

func diskFree(string) (uint64, error) {
	return 0, errors.New("free disk space not supported on this platform")
}
