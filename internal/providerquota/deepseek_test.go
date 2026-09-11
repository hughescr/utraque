package providerquota

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDeepSeekPreservesTotalAndComponents(t *testing.T) {
	const key = "deepseek-private-key"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/custom/user/balance" || r.Header.Get("Authorization") != "Bearer "+key {
			t.Errorf("request path=%q auth=%q", r.URL.Path, r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"is_available":true,"balance_infos":[{"currency":"USD","total_balance":"10.00","granted_balance":"0","topped_up_balance":"10.000"}]}`))
	}))
	defer server.Close()
	client, err := NewDeepSeekClient(DeepSeekOptions{BaseURL: server.URL + "/custom", APIKey: key, Now: func() time.Time { return fixedNow }})
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Read(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got.Available == nil || !*got.Available || len(got.Balances) != 1 {
		t.Fatalf("observation = %+v", got)
	}
	b := got.Balances[0]
	if b.Total != "10.00" || b.Components[0].Amount != "0" || b.Components[1].Amount != "10.000" {
		t.Errorf("balance precision/components changed: %+v", b)
	}
	wire, _ := json.Marshal(got)
	if strings.Contains(string(wire), key) || strings.Contains(string(wire), got.CacheScope()) {
		t.Errorf("JSON leaked private scope: %s", wire)
	}
}

func TestDeepSeekMissingOrNullBalancesDoNotBecomeZero(t *testing.T) {
	for _, body := range []string{
		`{"is_available":true}`,
		`{"is_available":true,"balance_infos":null}`,
	} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(body))
		}))
		client, err := NewDeepSeekClient(DeepSeekOptions{BaseURL: server.URL, APIKey: "key"})
		if err != nil {
			server.Close()
			t.Fatal(err)
		}
		got, err := client.Read(context.Background())
		server.Close()
		if err != nil {
			t.Fatal(err)
		}
		if got.Balances != nil {
			t.Errorf("body %s produced balances %+v", body, got.Balances)
		}
	}
}
