package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/router"
)

// healthView is the account-dependent part of /healthz.
type healthView struct {
	Quota struct {
		Reported bool `json:"reported"`
	} `json:"codex_quota"`
	Catalog struct {
		Loaded    bool   `json:"loaded"`
		Models    int    `json:"models"`
		State     string `json:"state"`
		LastError string `json:"last_error"`
	} `json:"codex_catalog"`
	Routing struct {
		Bare []string `json:"bare_aliases"`
	} `json:"codex_routing"`
}

// Quota and catalog state are shown only for the account auth.json names now.
// After a switch they read as not reported until the new account has its own
// sample and list; a file that cannot be read says nothing about the account,
// so it does not blank them; and a late answer to a request sent under the
// previous login is that login's sample, not the new one's. A poll fetches
// nothing and changes nothing.
func TestHealthShowsOnlyTheCurrentAccount(t *testing.T) {
	restoreRegistry(t)
	env := newCodexEnv(t, nil)
	entered, release := make(chan struct{}), make(chan struct{})
	env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, n int64) {
		if n == 2 {
			close(entered)
			<-release
		}
		w.Header().Set("X-Codex-Primary-Used-Percent", "17")
		writeSSE(w, solStream)
	}
	send := func() {
		r := post(t, env.front.URL+"/v1/messages", `{"model":"sol","max_tokens":8,"messages":[{"role":"user","content":"test"}]}`, nil)
		_, _ = io.Copy(io.Discard, r.Body)
		r.Body.Close()
	}
	type state struct {
		catalogs, posts, refreshes int64
		bare, picker               []string
		auth                       string
	}
	snapshot := func() state {
		raw, err := os.ReadFile(env.authPath)
		if err != nil {
			t.Fatal(err)
		}
		return state{env.codex.catalog.Load(), env.codex.calls.Load(), env.refreshes.Load(),
			router.DefaultRegistry.BareAliases(), router.DefaultRegistry.PickerIDs(), string(raw)}
	}
	health := func(reported bool) {
		t.Helper()
		before := snapshot()
		var views [2]healthView
		for i := range views {
			r, err := http.Get(env.front.URL + "/healthz")
			if err != nil {
				t.Fatal(err)
			}
			err = json.NewDecoder(r.Body).Decode(&views[i])
			r.Body.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if after := snapshot(); !reflect.DeepEqual(before, after) || !reflect.DeepEqual(views[0], views[1]) {
			t.Errorf("a health poll changed state: %+v -> %+v", before, after)
		}
		h := views[0]
		if h.Quota.Reported != reported || h.Catalog.Loaded != reported || (h.Catalog.Models > 0) != reported {
			t.Errorf("health %+v, want reported=%v", h, reported)
		}
		if !reported && (h.Catalog.State != "cold" || h.Catalog.LastError != "") {
			t.Errorf("another account's catalog state shown: %+v", h.Catalog)
		}
	}
	health(false)
	send()
	health(true)
	good, err := os.ReadFile(env.authPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(env.authPath, []byte(`{"tokens":{"account_id":"acct-fa`), 0o600); err != nil {
		t.Fatal(err)
	}
	health(true) // unreadable: nothing contradicts A's state
	if err := os.WriteFile(env.authPath, good, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan struct{})
	go func() { defer close(done); send() }()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("second request did not reach upstream")
	}
	writeAuthJSONWithAccount(t, env.authPath, jwtWithExp(time.Now().Add(8*time.Hour)), "refresh-B", "account-B")
	health(false)
	close(release)
	<-done
	health(false) // the late sample was A's
	send()
	health(true)
}

func TestCodexInBandError(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, started := range []bool{false, true} {
			for _, kind := range []string{"error", "response.failed"} {
				t.Run(fmt.Sprintf("%s/started=%v/stream=%v", kind, started, stream), func(t *testing.T) {
					env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true})
					token := readAccessToken(t, env.authPath)
					message := token + fmt.Sprintf(` \u%04x%s`, token[0], token[1:]) + ` acct-fake acct-fake`
					data := fmt.Sprintf(`{"type":"error","message":%s}`, `"`+message+`"`)
					if kind == "response.failed" {
						data = `{"type":"response.failed","response":{"error":{"message":"` + message + `"}}}`
					}
					payload := "event: " + kind + "\ndata: " + data + "\n\n"
					if started {
						prefix, _, _ := strings.Cut(solStream, "event: response.output_text.done")
						payload = prefix + payload
					}
					env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int64) { writeSSE(w, payload) }
					r := post(t, env.front.URL+"/v1/messages", fmt.Sprintf(`{"model":"sol","max_tokens":8,"stream":%v,"messages":[{"role":"user","content":"test"}]}`, stream), nil)
					defer r.Body.Close()
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					public := string(body) + env.logs.String()
					if strings.Contains(public, token) || strings.Contains(public, "acct-fake") || !strings.Contains(string(body), "[REDACTED] [REDACTED] [REDACTED] [REDACTED]") {
						t.Error("upstream error message was not safely redacted")
					}
				})
			}
		}
	}
}

func TestChangedLoginWithFailedRefreshIsStill400(t *testing.T) {
	env := newCodexEnv(t, nil)
	env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, _ int64) {
		// B's expired credential has no refresh token. A refresh error alone
		// would obscure the fact that this is now a different account.
		writeAuthJSONWithAccount(t, env.authPath, jwtWithExp(time.Now().Add(-time.Hour)), "", "acct-second")
		w.WriteHeader(401)
	}
	r := post(t, env.front.URL+"/v1/messages", `{"model":"sol","max_tokens":8,"messages":[{"role":"user","content":"test"}]}`, nil)
	defer r.Body.Close()
	if r.StatusCode != 400 || env.codex.calls.Load() != 1 {
		t.Fatal("failed refresh for a different login must not invite a replay")
	}
}

func TestSameAccountRefreshRecipient(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, unchanged := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/unchanged=%v", stream, unchanged), func(t *testing.T) {
				env := newCodexEnvOpts(t, nil, codexEnvOptions{KeepToken: unchanged})
				first := readAccessToken(t, env.authPath)
				env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, n int64) {
					if n == 1 {
						w.WriteHeader(401)
						_, _ = io.WriteString(w, `{"error":{"message":"original rejection"}}`)
						return
					}
					writeSSE(w, solStream)
				}
				r := post(t, env.front.URL+"/v1/messages", fmt.Sprintf(`{"model":"sol","max_tokens":8,"stream":%v,"messages":[{"role":"user","content":"test"}]}`, stream), nil)
				defer r.Body.Close()
				body, err := io.ReadAll(r.Body)
				if err != nil {
					t.Fatal(err)
				}
				accounts, tokens := []string{"acct-fake"}, []string{"Bearer " + first}
				if unchanged {
					if r.StatusCode != 401 || !strings.Contains(string(body), "original rejection") {
						t.Error("original rejection lost")
					}
				} else {
					fresh := readAccessToken(t, env.authPath)
					if fresh == first || r.StatusCode != 200 {
						t.Error("rotated token did not succeed")
					}
					accounts = append(accounts, "acct-fake")
					tokens = append(tokens, "Bearer "+fresh)
				}
				env.codex.mu.Lock()
				defer env.codex.mu.Unlock()
				if !slices.Equal(accounts, env.codex.accounts) || !slices.Equal(tokens, env.codex.authHdrs) {
					t.Error("wrong attempt count or recipient")
				}
			})
		}
	}
}

func TestCodexAccountSwitch(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, at := range []string{"lookup", "401", "401-same-token"} {
			t.Run(fmt.Sprintf("stream=%v/%s", stream, at), func(t *testing.T) {
				env := newCodexEnvOpts(t, nil, codexEnvOptions{CaptureLogs: true})
				a := readAccessToken(t, env.authPath)
				b := jwtWithExp(time.Now().Add(6 * time.Hour))
				if at == "401-same-token" {
					b = a
				}
				switchAccount := func() { writeAuthJSONWithAccount(t, env.authPath, b, "refresh-B", "acct-second") }
				if at == "lookup" {
					env.codex.onModels = switchAccount
				} else {
					env.codex.respond = func(_ *testing.T, w http.ResponseWriter, _ *http.Request, n int64) {
						if n == 1 {
							switchAccount()
							w.WriteHeader(401)
							return
						}
						writeSSE(w, solStream)
					}
				}
				send := func(content string) (int, string) {
					r := post(t, env.front.URL+"/v1/messages", fmt.Sprintf(`{"model":"sol","max_tokens":8,"stream":%v,"messages":[{"role":"user","content":%q}]}`, stream, content), nil)
					defer r.Body.Close()
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Fatal(err)
					}
					return r.StatusCode, string(body)
				}
				status, body := send("first request")
				if at != "lookup" {
					if status != 400 || !strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, "the Codex login changed while this request was in flight; it was not sent under the new account — resubmit to send it under the current login") {
						t.Errorf("wrong account-change rejection, status %d", status)
					}
				} else if status != 200 {
					t.Errorf("lookup switch status %d", status)
				}
				if status, _ := send("second request"); status != 200 {
					t.Errorf("new request status %d", status)
				}
				env.codex.mu.Lock()
				defer env.codex.mu.Unlock()
				if !slices.Equal(env.codex.accounts, []string{"acct-fake", "acct-second"}) || !slices.Equal(env.codex.authHdrs, []string{"Bearer " + a, "Bearer " + b}) {
					t.Fatalf("incorrect recipients across %d sends", len(env.codex.accounts))
				}
				// The switch is logged by hash only.
				logs := env.logs.String()
				if !strings.Contains(logs, "codex") {
					t.Fatal("no logs captured")
				}
				for _, raw := range []string{"acct-fake", "acct-second"} {
					if strings.Contains(logs, raw) {
						t.Errorf("log carries the raw account id %q", raw)
					}
				}
			})
		}
	}
}
