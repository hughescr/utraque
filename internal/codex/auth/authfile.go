package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// Known keys in a Codex auth.json. Every other key at either level is unknown
// to utraque and must survive a write-back untouched — hence the RawMessage
// maps below rather than a fixed struct.
const (
	keyTokens       = "tokens"
	keyLastRefresh  = "last_refresh"
	keyAccessToken  = "access_token"
	keyIDToken      = "id_token"
	keyRefreshToken = "refresh_token"
	keyAccountID    = "account_id"

	// fallbackTTL is added to last_refresh when the access token's own expiry
	// cannot be decoded.
	fallbackTTL = time.Hour
)

// authFile is a parsed Codex auth.json that preserves every key it did not
// recognise. top holds the top-level object; tokens holds the nested "tokens"
// object separately so unknown sub-keys (owned by the Codex CLI) round-trip
// verbatim. The Codex CLI may add fields at either level in any release; none
// of them may be dropped when utraque writes a refreshed token back.
type authFile struct {
	top    map[string]json.RawMessage
	tokens map[string]json.RawMessage
}

// parseAuthFile unmarshals the top-level object and the nested tokens object
// into raw-message maps, keeping unknown keys intact. A missing or JSON-null
// tokens object yields an empty (non-nil) tokens map.
func parseAuthFile(b []byte) (*authFile, error) {
	top := map[string]json.RawMessage{}
	if err := json.Unmarshal(b, &top); err != nil {
		return nil, fmt.Errorf("auth: parse auth.json: %w", err)
	}
	af := &authFile{top: top, tokens: map[string]json.RawMessage{}}
	if raw, ok := top[keyTokens]; ok && len(raw) > 0 {
		if !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			if err := json.Unmarshal(raw, &af.tokens); err != nil {
				return nil, fmt.Errorf("auth: parse auth.json tokens: %w", err)
			}
		}
	}
	return af, nil
}

// tokenString returns a string-valued key from the tokens object, or "".
func (af *authFile) tokenString(key string) string { return jsonString(af.tokens[key]) }

// topString returns a string-valued top-level key, or "".
func (af *authFile) topString(key string) string { return jsonString(af.top[key]) }

// setTokenString writes a string value into the tokens object.
func (af *authFile) setTokenString(key, val string) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	af.tokens[key] = raw
	return nil
}

// setTopString writes a string value at the top level.
func (af *authFile) setTopString(key, val string) error {
	raw, err := json.Marshal(val)
	if err != nil {
		return err
	}
	af.top[key] = raw
	return nil
}

// marshal renders the auth file back to JSON. Known fields carry their updated
// values; every unknown key at both levels is emitted from its preserved
// RawMessage. json.Marshal compacts each RawMessage but never rewrites its
// values, so a caller that stores compact values gets them back byte-for-byte.
func (af *authFile) marshal() ([]byte, error) {
	tokensRaw, err := json.Marshal(af.tokens)
	if err != nil {
		return nil, err
	}
	af.top[keyTokens] = tokensRaw
	b, err := json.Marshal(af.top)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func jsonString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return ""
	}
	return s
}

// writeAuthFileAtomic writes data to a temp file in the same directory, fsyncs
// it, restores the given mode, and renames it over path. The rename is atomic
// within a directory, so a concurrent reader (the Codex CLI) sees either the
// old file or the new one, never a half-written file. On any failure before the
// rename the temp file is removed.
func writeAuthFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	// If the target is a symlink (some users symlink auth.json into ~/.codex),
	// resolve it and write the real file. Renaming over the link path itself
	// would swap the symlink for a regular file, splitting utraque and the Codex
	// CLI onto different files.
	if fi, lerr := os.Lstat(path); lerr == nil && fi.Mode()&os.ModeSymlink != 0 {
		if resolved, rerr := filepath.EvalSymlinks(path); rerr == nil {
			path = resolved
		}
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("auth: create temp: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()

	if _, err = tmp.Write(data); err != nil {
		return fmt.Errorf("auth: write temp: %w", err)
	}
	if err = tmp.Sync(); err != nil {
		return fmt.Errorf("auth: fsync temp: %w", err)
	}
	if err = tmp.Close(); err != nil {
		return fmt.Errorf("auth: close temp: %w", err)
	}
	if mode == 0 {
		mode = 0o600
	}
	if err = os.Chmod(tmpName, mode.Perm()); err != nil {
		return fmt.Errorf("auth: chmod temp: %w", err)
	}
	if err = os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("auth: rename temp: %w", err)
	}
	committed = true

	// Best-effort durability of the rename itself; ignore platforms that reject
	// a directory fsync.
	if d, derr := os.Open(dir); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
