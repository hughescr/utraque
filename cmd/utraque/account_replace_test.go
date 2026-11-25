//go:build unix

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A login that replaces auth.json by rename and keeps both its size and its
// modification time is still noticed: the next request's POST is signed by the
// new account, not the one utraque cached.
func TestReplacedAuthFileWithSameSizeAndMtimeSignsAsTheNewAccount(t *testing.T) {
	env := newCodexEnv(t, nil)
	if s, _ := sendModel(t, env, "sol"); s != 200 {
		t.Fatalf("first request: HTTP %d", s)
	}
	before, err := os.Stat(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	old, err := os.ReadFile(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	// "acct-fake" and "acct-fakz" have the same length, so the file does too.
	replacement := []byte(strings.Replace(string(old), "acct-fake", "acct-fakz", 1))
	tmp := filepath.Join(filepath.Dir(env.authPath), "auth.json.replacement")
	if err := os.WriteFile(tmp, replacement, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, before.ModTime(), before.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, env.authPath); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() != before.Size() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("fixture must keep size and mtime: %d/%d %v/%v", before.Size(), after.Size(), before.ModTime(), after.ModTime())
	}
	if s, _ := sendModel(t, env, "sol"); s != 200 {
		t.Fatalf("request after the replacement: HTTP %d", s)
	}
	env.codex.mu.Lock()
	defer env.codex.mu.Unlock()
	last := env.codex.accounts[len(env.codex.accounts)-1]
	if last != "acct-fakz" {
		t.Fatalf("the request after the replacement was signed by %q; accounts seen: %v", last, slices.Clone(env.codex.accounts))
	}
}
