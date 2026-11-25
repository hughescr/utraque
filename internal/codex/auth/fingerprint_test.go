//go:build unix

package auth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// replaceByRename puts body at path the way the Codex CLI's safe path does: a
// temp file renamed over it, so the file at path is a new inode. When keep is
// non-nil the new file takes that modification time.
func replaceByRename(t *testing.T, path string, body []byte, keep *time.Time) {
	t.Helper()
	tmp := filepath.Join(filepath.Dir(path), "replacement.tmp")
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if keep != nil {
		if err := os.Chtimes(tmp, *keep, *keep); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

// sameShape returns two valid auth files for different accounts whose bytes
// have the same length.
func sameShape(access string) (first, second []byte) {
	first = authJSON{access: access, refresh: "refresh-A", accountID: "acct-aaaa"}.bytes()
	second = authJSON{access: access, refresh: "refresh-A", accountID: "acct-bbbb"}.bytes()
	return first, second
}

func getAccount(t *testing.T, s *Source) string {
	t.Helper()
	cred, err := s.Get(context.Background())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	return cred.AccountID
}

// A file replaced by another login is noticed even when the replacement has
// the old file's size and modification time: the file's identity is part of
// the fingerprint.
func TestReplacementWithSameSizeAndMtimeIsNoticed(t *testing.T) {
	f := newFakeOAuth(t)
	first, second := sameShape(makeJWT(time.Now().Add(time.Hour)))
	s, path := newTestSource(t, f, first)
	if got := getAccount(t, s); got != "acct-aaaa" {
		t.Fatalf("first account = %q", got)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	mtime := before.ModTime()
	replaceByRename(t, path, second, &mtime)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) || os.SameFile(before, after) {
		t.Fatalf("fixture must keep size and mtime and change the file: size %d/%d mtime %v/%v same=%v",
			before.Size(), after.Size(), before.ModTime(), after.ModTime(), os.SameFile(before, after))
	}
	if got := getAccount(t, s); got != "acct-bbbb" {
		t.Fatalf("after replacement Get returned %q, want the new login", got)
	}
}

// The documented limit: an in-place rewrite that keeps both size and
// modification time is not noticed, and the cached login keeps being used.
// Changing that is a deliberate decision, so this pins it.
func TestInPlaceRewriteWithSameSizeAndMtimeIsNotNoticed(t *testing.T) {
	f := newFakeOAuth(t)
	first, second := sameShape(makeJWT(time.Now().Add(time.Hour)))
	s, path := newTestSource(t, f, first)
	if got := getAccount(t, s); got != "acct-aaaa" {
		t.Fatalf("first account = %q", got)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, second, 0o600); err != nil { // truncate and rewrite: same inode
		t.Fatal(err)
	}
	if err := os.Chtimes(path, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) || after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("fixture must be an in-place rewrite with unchanged size and mtime")
	}
	if got := getAccount(t, s); got != "acct-aaaa" {
		t.Fatalf("in-place rewrite with the same size and mtime was noticed (%q); update the documented limit if that is intended", got)
	}
}

// The fingerprint stored after utraque's own refresh describes the file
// utraque wrote, not whatever is at the path afterwards: if another process
// replaces the file in the instant after the rename, the next Get must see the
// replacement.
func TestRefreshFingerprintIsOfTheFileWritten(t *testing.T) {
	f := newFakeOAuth(t)
	s, path := newTestSource(t, f, authJSON{
		access:    makeJWT(time.Now().Add(30 * time.Second)), // within skew: stale
		refresh:   "refresh-old",
		accountID: "acct-aaaa",
	}.bytes())

	other := authJSON{access: makeJWT(time.Now().Add(time.Hour)), refresh: "refresh-B", accountID: "acct-bbbb"}.bytes()
	realRename := renameFile
	t.Cleanup(func() { renameFile = realRename })
	renameFile = func(from, to string) error {
		if err := realRename(from, to); err != nil {
			return err
		}
		renameFile = realRename // once
		replaceByRename(t, to, other, nil)
		return nil
	}

	if got := getAccount(t, s); got != "acct-aaaa" {
		t.Fatalf("the refresh itself returned %q, want the refreshed login", got)
	}
	if got := getAccount(t, s); got != "acct-bbbb" {
		t.Fatalf("Get after the file was replaced returned %q: the refresh stamped its credential with the replacement's fingerprint", got)
	}
	if b, _ := os.ReadFile(path); string(b) != string(other) {
		t.Fatal("fixture: the replacement is not what is on disk")
	}
}

// writeAuthFileAtomic returns the fingerprint the path shows once it is done.
func TestWriteAuthFileAtomicReturnsTheWrittenFilesFingerprint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := writeAuthFileAtomic(path, []byte(`{"tokens":{"x":"y"}}`), 0o600)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := statOf(st); got != want {
		t.Fatalf("returned fingerprint %+v, path shows %+v", got, want)
	}
	if got.ino == 0 {
		t.Fatal("no inode in the fingerprint on a unix build")
	}
}
