//go:build !unix

package main

import "os"

// fileIdentity has no portable device/inode source off unix; the path,
// symlink target, size and mtime still make up the fingerprint.
func fileIdentity(os.FileInfo) (dev, ino uint64) { return 0, 0 }
