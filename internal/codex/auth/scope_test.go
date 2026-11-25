package auth

import (
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/accountscope"
)

// A credential read from the file carries its account scope, computed once;
// Peek reports the same scope; a Credential built by hand computes it on
// demand; and one naming no account has none.
func TestCredentialScope(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{access: makeJWT(time.Now().Add(time.Hour)), refresh: "r", accountID: "acct-A"}.bytes())
	want := accountscope.Key("codex", "acct-A")
	cred, err := s.Get(t.Context())
	if err != nil || cred.scope != want || cred.Scope() != want {
		t.Fatalf("Get: scope %q (%v), want %q precomputed", cred.scope, err, want)
	}
	if got := s.Peek().Scope; got != want {
		t.Errorf("Peek scope %q, want %q", got, want)
	}
	if got := (Credential{AccountID: "acct-A"}).Scope(); got != want {
		t.Errorf("hand-built credential scope %q, want %q", got, want)
	}
	if got := (Credential{AccessToken: "t"}).Scope(); got != "" {
		t.Errorf("a credential naming no account has scope %q", got)
	}
}

// When a refresh fails, Get returns no token but still reports the scope of
// the account the file names: a caller retrying after a 401 must be able to
// tell a changed login from a failed refresh.
func TestGetErrorReportsTheFileScope(t *testing.T) {
	f := newFakeOAuth(t)
	f.status = http.StatusBadRequest
	s, path := newTestSource(t, f, authJSON{access: makeJWT(time.Now().Add(time.Hour)), refresh: "r", accountID: "acct-A"}.bytes())
	if err := os.WriteFile(path, authJSON{access: makeJWT(time.Now().Add(-time.Hour)), refresh: "r", accountID: "acct-B"}.bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	cred, err := s.Get(t.Context())
	if err == nil || cred.AccessToken != "" || cred.AccountID != "" {
		t.Fatalf("a failed refresh returned a token (err %v)", err)
	}
	if got, want := cred.Scope(), accountscope.Key("codex", "acct-B"); got != want {
		t.Errorf("scope %q, want the file's account %q", got, want)
	}
	if err := os.WriteFile(path, []byte(`{"tokens":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if cred, err := s.Get(t.Context()); err == nil || cred.Scope() != "" {
		t.Errorf("a file naming no account reported scope %q (err %v)", cred.Scope(), err)
	}
}

// As on main, account ids have no length limit: an 8 KiB id is an account, sent
// intact, with its scope derived from the whole value.
func TestLongAccountIDIsAcceptedIntact(t *testing.T) {
	id := strings.Repeat("a", 8<<10)
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{access: makeJWT(time.Now().Add(time.Hour)), refresh: "r", accountID: id}.bytes())
	cred, err := s.Get(t.Context())
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if cred.AccountID != id || cred.Scope() != accountscope.Key("codex", id) {
		t.Fatalf("an 8 KiB account id was altered: len %d, scope %q", len(cred.AccountID), cred.Scope())
	}
}

// As on main, only an absent or empty account_id is refused; any other value,
// even blank-looking, is an account and is sent as-is.
func TestWhitespaceAccountIDIsAnAccount(t *testing.T) {
	f := newFakeOAuth(t)
	s, _ := newTestSource(t, f, authJSON{access: makeJWT(time.Now().Add(time.Hour)), refresh: "r", accountID: " "}.bytes())
	cred, err := s.Get(t.Context())
	if err != nil || cred.AccountID != " " || cred.Scope() != accountscope.Key("codex", " ") {
		t.Fatalf("Get = %q, %v", cred.AccountID, err)
	}
}
