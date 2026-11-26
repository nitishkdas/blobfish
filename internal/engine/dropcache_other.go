//go:build !linux

package engine

import "os"

// dropCache is a no-op where posix_fadvise isn't available; reads may then
// come from the page cache.
func dropCache(*os.File) {}
