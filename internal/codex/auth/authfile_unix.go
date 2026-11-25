//go:build unix

package auth

import (
	"os"
	"syscall"
)

// fileIdentity returns the device and inode numbers of the file behind fi, so
// a file replaced by rename is a different file even when its size and
// modification time match. It is (0, 0) when the platform gives no inode.
func fileIdentity(fi os.FileInfo) (dev, ino uint64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return uint64(st.Dev), uint64(st.Ino) // widths differ by platform
}

// fileGroup returns the numeric group that owns the file behind fi.
func fileGroup(fi os.FileInfo) (int, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, false
	}
	return int(st.Gid), true
}
