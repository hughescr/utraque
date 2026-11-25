//go:build !unix

package auth

import "os"

// fileIdentity has no portable source off unix, so the file fingerprint stays
// size and modification time.
func fileIdentity(os.FileInfo) (dev, ino uint64) { return 0, 0 }

// fileGroup has no portable group source off unix, so keepGroup narrows the
// mode instead of comparing groups.
func fileGroup(os.FileInfo) (int, bool) { return 0, false }
