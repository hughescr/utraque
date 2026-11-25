package catalog

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
)

// A slow (or hung) cache write delays only the fetch that produced it: it is
// staged with no lock held, so another account's fetch meanwhile commits and
// is served at once. The late record commits after it (a foreground fetch
// always does), and the next call for B misses on scope and puts B's list back
// in memory and on disk. No staged file is left behind.
func TestSlowDiskWriteDoesNotHoldPublication(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"models":[{"slug":"`+r.Header.Get("chatgpt-account-id")+`"}]}`)
	}))
	defer srv.Close()
	dir := t.TempDir()
	cache := filepath.Join(dir, "cache.json")
	c := New(Options{BaseURL: srv.URL, CacheFile: cache})
	entered, release := make(chan struct{}), make(chan struct{})
	var first atomic.Bool
	c.stageHook = func() {
		if first.CompareAndSwap(false, true) {
			close(entered)
			<-release
		}
	}
	a := auth.Credential{AccessToken: "token", AccountID: "acct-A"}
	b := auth.Credential{AccessToken: "token", AccountID: "acct-B"}

	aDone := make(chan error, 1)
	go func() { _, err := c.Models(t.Context(), a); aDone <- err }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("A's fetch never reached its disk write")
	}
	bDone := make(chan []cschema.CatalogModel, 1)
	go func() {
		rows, err := c.Models(t.Context(), b)
		if err != nil {
			t.Error(err)
		}
		bDone <- rows
	}()
	select {
	case rows := <-bDone:
		if len(rows) != 1 || rows[0].Slug != "acct-B" {
			t.Errorf("B served %v", rows)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("a stalled disk write blocked the switch to another account")
	}
	close(release)
	if err := <-aDone; err != nil {
		t.Error(err)
	}
	if rows, err := c.Models(t.Context(), b); err != nil || len(rows) != 1 || rows[0].Slug != "acct-B" {
		t.Fatalf("B served %v (%v) after A's late commit", rows, err)
	}
	raw, err := os.ReadFile(cache)
	if err != nil {
		t.Fatal(err)
	}
	var rec cschema.Cache
	if err := json.Unmarshal(raw, &rec); err != nil || rec.AccountScope != b.Scope() {
		t.Fatalf("disk record is not B's: %s", raw)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		names := []string{}
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("staged record left behind: %v", names)
	}
}
