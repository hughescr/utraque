//go:build unix

package main

import (
	"os"
	"syscall"
)

// fileIdentity returns the device and inode behind info, which change when an
// upgrade replaces the file even if its size and mtime happen to match.
func fileIdentity(info os.FileInfo) (dev, ino uint64) {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	// Dev is int32 on darwin and uint64 on linux; the conversion is for
	// comparison only.
	return uint64(st.Dev), uint64(st.Ino)
}
