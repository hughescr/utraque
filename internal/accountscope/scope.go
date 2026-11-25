// Package accountscope names account-derived state without retaining its credential.
package accountscope

import (
	"crypto/sha256"
	"encoding/hex"
)

// Key is the provider-report namespace: provider, NUL, exact identity bytes.
func Key(provider, identity string) string {
	sum := sha256.Sum256([]byte(provider + "\x00" + identity))
	return provider + ":" + hex.EncodeToString(sum[:])
}
