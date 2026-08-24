package discovery_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/discovery"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/transport"
)

// ---------------------------------------------------------------------------
// Fixtures
//
// Nothing here contacts a real upstream and nothing reads a real credential
// file: the Anthropic side is always an httptest server, the Codex side is
// always an in-process fake, and every credential is a literal.
// ---------------------------------------------------------------------------

// codexFixture mirrors the live catalog shapes the plan recorded: two codenamed
// models at the same version, two version-only models, and one hidden
// irregular slug.
func codexFixture() []schema.Model {
	return []schema.Model{
		{
			Slug: "gpt-5.6-sol", DisplayName: "GPT-5.6-Sol", Visibility: schema.VisibilityList,
			ContextWindow: 400000, DefaultReasoningLevel: "low", Priority: 10,
			SupportedReasoningLevels: []schema.ReasoningLevel{
				{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}, {Effort: "max"}, {Effort: "ultra"},
			},
		},
		{
			Slug: "gpt-5.6-terra", DisplayName: "GPT-5.6-Terra", Visibility: schema.VisibilityList,
			ContextWindow: 400000, DefaultReasoningLevel: "medium", Priority: 9,
			SupportedReasoningLevels: []schema.ReasoningLevel{{Effort: "medium"}, {Effort: "high"}},
		},
		{
			Slug: "gpt-5.5", DisplayName: "GPT-5.5", Visibility: schema.VisibilityList,
			ContextWindow: 400000, DefaultReasoningLevel: "medium", Priority: 5,
			SupportedReasoningLevels: []schema.ReasoningLevel{{Effort: "low"}, {Effort: "medium"}, {Effort: "high"}},
		},
		{
			Slug: "gpt-5.4-mini", DisplayName: "GPT-5.4-Mini", Visibility: schema.VisibilityList,
			ContextWindow: 272000, DefaultReasoningLevel: "medium", Priority: 2,
			SupportedReasoningLevels: []schema.ReasoningLevel{{Effort: "medium"}},
		},
		{
			// Hidden, and a slug the alias grammar cannot parse on its own.
			Slug: "gpt-5.3-codex-spark", DisplayName: "GPT-5.3-Codex-Spark", Visibility: schema.VisibilityHide,
			ContextWindow: 272000, DefaultReasoningLevel: "medium", Priority: 1,
			SupportedReasoningLevels: []schema.ReasoningLevel{{Effort: "medium"}, {Effort: "high"}},
		},
	}
}

// loadRegistry points the process-wide registry (the one router.Resolve reads)
// at the listed models from the fixture, and restores the static seed after the
// test. Hidden models are deliberately left out, exactly as the live loader
// leaves them out.
func loadRegistry(t *testing.T) *router.Registry {
	t.Helper()
	reg := router.DefaultRegistry
	entries := make([]router.CatalogEntry, 0, 4)
	for _, m := range codexFixture() {
		if !m.Listed() {
			continue
		}
		entries = append(entries, router.CatalogEntry{Slug: m.Slug, Priority: m.Priority})
	}
	reg.LoadCatalog(entries)
	t.Cleanup(func() {
		*reg = *router.NewStaticRegistry()
	})
	return reg
}

func codexOK() discovery.CodexCatalog {
	return discovery.CodexCatalogFunc(func(context.Context) ([]schema.Model, error) {
		return codexFixture(), nil
	})
}

func codexFailing() discovery.CodexCatalog {
	return discovery.CodexCatalogFunc(func(context.Context) ([]schema.Model, error) {
		return nil, fmt.Errorf("codex catalog unavailable")
	})
}

// fakeAnthropic starts an httptest server answering GET /v1/models, and returns
// a catalog client aimed at it plus a counter of requests received.
func fakeAnthropic(t *testing.T, handler http.HandlerFunc) (*anthropic.CatalogClient, *atomic.Int64) {
	t.Helper()
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		handler(w, r)
	}))
	t.Cleanup(srv.Close)

	cat, err := anthropic.NewCatalog(srv.URL, transport.NewStd(transport.DefaultOptions()))
	if err != nil {
		t.Fatalf("NewCatalog: %v", err)
	}
	return cat, &hits
}

// upstreamModels answers with a well-formed Anthropic catalog page.
func upstreamModels(models ...anthropic.CatalogModel) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": models})
	}
}

// testCred is a literal stand-in for the client's own credential. It is never a
// real token and never leaves the test process.
var testCred = anthropic.Credential{Authorization: "Bearer test-not-a-real-token"}

func mustHandler(t *testing.T, opts discovery.Options) *discovery.Handler {
	t.Helper()
	h, err := discovery.New(opts)
	if err != nil {
		t.Fatalf("discovery.New: %v", err)
	}
	return h
}

func ids(resp discovery.Response) []string {
	out := make([]string, len(resp.Data))
	for i, m := range resp.Data {
		out[i] = m.ID
	}
	return out
}

func hasID(resp discovery.Response, want string) bool {
	for _, m := range resp.Data {
		if m.ID == want {
			return true
		}
	}
	return false
}

func displayOf(resp discovery.Response, id string) string {
	for _, m := range resp.Data {
		if m.ID == id {
			return m.DisplayName
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Response shape
// ---------------------------------------------------------------------------

func TestResponseShapeIsWhatTheClientParses(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	// The client parses {"data":[{id, display_name}]} and ignores everything
	// else. Decode into a deliberately narrow shape to prove that works.
	var narrow struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &narrow); err != nil {
		t.Fatalf("client-shaped decode: %v\nbody: %s", err, rec.Body.String())
	}
	if len(narrow.Data) == 0 {
		t.Fatal("data is empty")
	}
	for _, m := range narrow.Data {
		if m.ID == "" {
			t.Error("a row has an empty id")
		}
		if m.DisplayName == "" {
			t.Errorf("row %q has an empty display_name; the picker would show a blank line", m.ID)
		}
	}

	var full discovery.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("full decode: %v", err)
	}
	if full.HasMore {
		t.Error("has_more should be false for an unlimited page")
	}
	if full.FirstID != full.Data[0].ID || full.LastID != full.Data[len(full.Data)-1].ID {
		t.Errorf("first_id/last_id = %q/%q, want %q/%q",
			full.FirstID, full.LastID, full.Data[0].ID, full.Data[len(full.Data)-1].ID)
	}
	for _, m := range full.Data {
		if m.Type != "model" {
			t.Errorf("row %q has type %q, want \"model\"", m.ID, m.Type)
		}
	}
}

func TestHeadRequestSendsNoBody(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{CatalogMode: discovery.CatalogModeStatic})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodHead, "/v1/models", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("HEAD returned a %d-byte body", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") == "" {
		t.Error("HEAD should still advertise Content-Length")
	}
}

func TestLimitTruncatesAndReportsHasMore(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?limit=2", nil))

	var resp discovery.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Data) != 2 {
		t.Fatalf("len(data) = %d, want 2", len(resp.Data))
	}
	if !resp.HasMore {
		t.Error("has_more should be true when the page was truncated")
	}
	if resp.LastID != resp.Data[1].ID {
		t.Errorf("last_id = %q, want %q", resp.LastID, resp.Data[1].ID)
	}
}

// ---------------------------------------------------------------------------
// The two invariants: every emitted id survives the client's filter, and every
// emitted id routes back through the router.
// ---------------------------------------------------------------------------

func TestEveryEmittedIDPassesTheFilterAndRoutesBack(t *testing.T) {
	cases := []struct {
		name string
		opts discovery.Options
	}{
		{"defaults", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
		}},
		{"alias_template", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			Alias: discovery.AliasOptions{Strategy: discovery.AliasTemplate},
		}},
		{"alias_effort_variants", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			Alias: discovery.AliasOptions{Strategy: discovery.AliasEffortVariants},
		}},
		{"alias_passthrough", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			Alias: discovery.AliasOptions{Strategy: discovery.AliasPassthrough},
		}},
		{"alias_off", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			Alias: discovery.AliasOptions{Strategy: discovery.AliasOff},
		}},
		{"include_hidden", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			Alias: discovery.AliasOptions{IncludeHidden: true},
		}},
		{"custom_template", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			// The suffixed form the plan mentions as the alternative. It passes
			// today's unanchored filter and must still route.
			Alias: discovery.AliasOptions{IDTemplate: "{alias}.anthropic-compat"},
		}},
		{"one_m_disabled", discovery.Options{
			CatalogMode: discovery.CatalogModeStatic, Codex: codexOK(),
			OneM: discovery.OneMOptions{Disabled: true},
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadRegistry(t)
			h := mustHandler(t, tc.opts)
			resp := h.Models(t.Context(), testCred)

			if len(resp.Data) == 0 {
				t.Fatal("no rows emitted; this test proves nothing")
			}
			for _, m := range resp.Data {
				if !discovery.PassesClientFilter(m.ID) {
					t.Errorf("id %q fails /(claude|anthropic)/i; the client would discard it", m.ID)
				}
				dec, err := router.Resolve(m.ID, "")
				if err != nil {
					t.Errorf("id %q does not route: %v", m.ID, err)
					continue
				}
				if !dec.Backend.Valid() {
					t.Errorf("id %q routed to an invalid backend %q", m.ID, dec.Backend)
				}
				if dec.Backend == router.BackendCodex && dec.UpstreamModel == "" {
					t.Errorf("id %q routed to codex with no upstream model", m.ID)
				}
			}
		})
	}
}

func TestEmittedCodexIDsCarryTheRightUpstreamSlug(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
		Alias:       discovery.AliasOptions{Strategy: discovery.AliasEffortVariants},
	})
	h.Models(t.Context(), testCred)

	for _, tc := range []struct {
		id       string
		upstream string
		effort   string
	}{
		{"anthropic-compat.sol", "gpt-5.6-sol", ""},
		{"anthropic-compat.sol-5.6", "gpt-5.6-sol", ""},
		{"anthropic-compat.sol-ultra", "gpt-5.6-sol", "ultra"},
		{"anthropic-compat.sol-5.6-high", "gpt-5.6-sol", "high"},
		{"anthropic-compat.terra", "gpt-5.6-terra", ""},
		{"anthropic-compat.5.5", "gpt-5.5", ""},
		{"anthropic-compat.5.4-mini", "gpt-5.4-mini", ""},
	} {
		dec, err := router.Resolve(tc.id, "")
		if err != nil {
			t.Errorf("Resolve(%q): %v", tc.id, err)
			continue
		}
		if dec.Backend != router.BackendCodex {
			t.Errorf("Resolve(%q).Backend = %q, want codex", tc.id, dec.Backend)
		}
		if dec.UpstreamModel != tc.upstream {
			t.Errorf("Resolve(%q).UpstreamModel = %q, want %q", tc.id, dec.UpstreamModel, tc.upstream)
		}
		if dec.Effort != tc.effort {
			t.Errorf("Resolve(%q).Effort = %q, want %q", tc.id, dec.Effort, tc.effort)
		}
	}
}

func TestRegistrationIsRebuiltEachTime(t *testing.T) {
	reg := loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)
	if len(reg.PickerIDs()) == 0 {
		t.Fatal("no picker routes registered")
	}

	// Every codex/1M row must have a route; plain Anthropic rows need none
	// because the ordinary grammar already places them.
	registered := make(map[string]bool)
	for _, id := range reg.PickerIDs() {
		registered[id] = true
	}
	for _, m := range resp.Data {
		if strings.HasPrefix(m.ID, "anthropic-compat.") || strings.HasSuffix(m.ID, "[1m]") {
			if !registered[strings.ToLower(m.ID)] {
				t.Errorf("emitted id %q was not registered as a picker route", m.ID)
			}
		}
	}

	// Turning Codex rows off must un-route the ids we stop advertising.
	off := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
		Alias:       discovery.AliasOptions{Strategy: discovery.AliasOff},
		OneM:        discovery.OneMOptions{Disabled: true},
	})
	off.Models(t.Context(), testCred)
	if got := reg.PickerIDs(); len(got) != 0 {
		t.Errorf("picker routes = %v, want none after everything was turned off", got)
	}
	if _, err := router.Resolve("anthropic-compat.sol", ""); err != nil {
		t.Errorf("the bare compat form must still route via the grammar: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Alias strategies
// ---------------------------------------------------------------------------

func TestAliasStrategies(t *testing.T) {
	cases := []struct {
		name       string
		alias      discovery.AliasOptions
		want       []string
		wantAbsent []string
	}{
		{
			name:  "off emits no codex rows",
			alias: discovery.AliasOptions{Strategy: discovery.AliasOff},
			wantAbsent: []string{
				"anthropic-compat.sol", "anthropic-compat.sol-5.6", "anthropic-compat.gpt-5.6-sol",
			},
		},
		{
			name:  "template emits rolling and pinned",
			alias: discovery.AliasOptions{Strategy: discovery.AliasTemplate},
			want: []string{
				"anthropic-compat.sol", "anthropic-compat.sol-5.6",
				"anthropic-compat.terra", "anthropic-compat.terra-5.6",
				"anthropic-compat.5.5", "anthropic-compat.5.4-mini",
			},
			wantAbsent: []string{
				"anthropic-compat.gpt-5.6-sol", // raw slug is the passthrough strategy's job
				"anthropic-compat.sol-high",    // effort rows are effort_variants' job
			},
		},
		{
			name:  "effort_variants adds one row per supported effort",
			alias: discovery.AliasOptions{Strategy: discovery.AliasEffortVariants},
			want: []string{
				"anthropic-compat.sol",
				"anthropic-compat.sol-low", "anthropic-compat.sol-ultra",
				"anthropic-compat.sol-5.6", "anthropic-compat.sol-5.6-ultra",
				"anthropic-compat.terra-high",
			},
			wantAbsent: []string{
				// terra tops out at high in the fixture.
				"anthropic-compat.terra-ultra",
			},
		},
		{
			name:  "passthrough emits raw slugs only",
			alias: discovery.AliasOptions{Strategy: discovery.AliasPassthrough},
			want: []string{
				"anthropic-compat.gpt-5.6-sol", "anthropic-compat.gpt-5.6-terra",
				"anthropic-compat.gpt-5.5", "anthropic-compat.gpt-5.4-mini",
			},
			wantAbsent: []string{"anthropic-compat.sol", "anthropic-compat.sol-5.6"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			loadRegistry(t)
			h := mustHandler(t, discovery.Options{
				CatalogMode: discovery.CatalogModeStatic,
				Codex:       codexOK(),
				Alias:       tc.alias,
			})
			resp := h.Models(t.Context(), testCred)

			for _, want := range tc.want {
				if !hasID(resp, want) {
					t.Errorf("missing id %q; got %v", want, ids(resp))
				}
			}
			for _, absent := range tc.wantAbsent {
				if hasID(resp, absent) {
					t.Errorf("id %q should not have been emitted", absent)
				}
			}
		})
	}
}

func TestCodexRowsLeadWithTheHighestPriorityModel(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)

	var codexIDs []string
	for _, m := range resp.Data {
		if strings.HasPrefix(m.ID, "anthropic-compat.") {
			codexIDs = append(codexIDs, m.ID)
		}
	}
	if len(codexIDs) == 0 {
		t.Fatal("no codex rows")
	}
	// sol has the highest catalog priority in the fixture (10).
	if codexIDs[0] != "anthropic-compat.sol" {
		t.Errorf("first codex row = %q, want anthropic-compat.sol; order = %v", codexIDs[0], codexIDs)
	}
	// 5.4-mini has the lowest listed priority (2).
	if last := codexIDs[len(codexIDs)-1]; last != "anthropic-compat.5.4-mini" {
		t.Errorf("last codex row = %q, want anthropic-compat.5.4-mini; order = %v", last, codexIDs)
	}
}

func TestRollingAliasPrecedesPinnedAlias(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)

	rolling, pinned := -1, -1
	for i, m := range resp.Data {
		switch m.ID {
		case "anthropic-compat.sol":
			rolling = i
		case "anthropic-compat.sol-5.6":
			pinned = i
		}
	}
	if rolling < 0 || pinned < 0 {
		t.Fatalf("expected both sol rows, got %v", ids(resp))
	}
	if rolling > pinned {
		t.Errorf("rolling alias at %d should precede the pinned alias at %d", rolling, pinned)
	}
}

func TestCodexDisplayNamesAreUseful(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)

	if got, want := displayOf(resp, "anthropic-compat.sol"), "GPT-5.6-Sol (sol)"; got != want {
		t.Errorf("display for the rolling row = %q, want %q", got, want)
	}
	if got, want := displayOf(resp, "anthropic-compat.sol-5.6"), "GPT-5.6-Sol (sol-5.6)"; got != want {
		t.Errorf("display for the pinned row = %q, want %q", got, want)
	}
}

func TestRejectsATemplateThatCannotPassTheFilter(t *testing.T) {
	_, err := discovery.New(discovery.Options{Alias: discovery.AliasOptions{IDTemplate: "codex.{alias}"}})
	if err == nil {
		t.Fatal("a template that renders filter-failing ids must be rejected at construction")
	}
	if !strings.Contains(err.Error(), "filter") {
		t.Errorf("error should explain the filter, got: %v", err)
	}

	if _, err := discovery.New(discovery.Options{
		Alias: discovery.AliasOptions{IDTemplate: "anthropic-compat-noalias"},
	}); err == nil {
		t.Fatal("a template without {alias} must be rejected")
	}
	if _, err := discovery.New(discovery.Options{Alias: discovery.AliasOptions{Strategy: "sideways"}}); err == nil {
		t.Fatal("an unknown alias strategy must be rejected")
	}
	if _, err := discovery.New(discovery.Options{CatalogMode: "sideways"}); err == nil {
		t.Fatal("an unknown catalog mode must be rejected")
	}
}

// ---------------------------------------------------------------------------
// Hidden-model filtering
// ---------------------------------------------------------------------------

func TestHiddenModelsAreNotAdvertisedByDefault(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)

	for _, m := range resp.Data {
		if strings.Contains(m.ID, "spark") {
			t.Errorf("hidden model leaked into the catalog as %q", m.ID)
		}
	}
}

func TestIncludeHiddenOffersHiddenModelsAndTheyStillRoute(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexOK(),
		Alias:       discovery.AliasOptions{IncludeHidden: true},
	})
	resp := h.Models(t.Context(), testCred)

	// The registry never saw the hidden slug, so it is offered raw — and the
	// picker-route registration is the only thing that makes it resolve.
	const want = "anthropic-compat.gpt-5.3-codex-spark"
	if !hasID(resp, want) {
		t.Fatalf("missing hidden row %q; got %v", want, ids(resp))
	}
	dec, err := router.Resolve(want, "")
	if err != nil {
		t.Fatalf("Resolve(%q): %v", want, err)
	}
	if dec.Backend != router.BackendCodex || dec.UpstreamModel != "gpt-5.3-codex-spark" {
		t.Errorf("Resolve(%q) = %+v, want codex/gpt-5.3-codex-spark", want, dec)
	}
}

// ---------------------------------------------------------------------------
// 1M context rows
// ---------------------------------------------------------------------------

func TestOneMRowsAreEmittedForNativeLongContextModels(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{CatalogMode: discovery.CatalogModeStatic})
	resp := h.Models(t.Context(), testCred)

	for _, base := range []string{"claude-sonnet-5", "claude-fable-5", "claude-opus-5"} {
		want := base + "[1m]"
		if !hasID(resp, want) {
			t.Errorf("missing long-context row %q; got %v", want, ids(resp))
			continue
		}
		if got := displayOf(resp, want); !strings.HasSuffix(got, "(1M context)") {
			t.Errorf("display for %q = %q, want a \"(1M context)\" suffix", want, got)
		}
		// The row must sit next to its base model, not at the end of the list.
		var baseIdx, oneMIdx = -1, -1
		for i, m := range resp.Data {
			switch m.ID {
			case base:
				baseIdx = i
			case want:
				oneMIdx = i
			}
		}
		if oneMIdx != baseIdx+1 {
			t.Errorf("%q is at %d, want immediately after %q at %d", want, oneMIdx, base, baseIdx)
		}
	}

	// A 200K model must not get one.
	if hasID(resp, "claude-haiku-4-5[1m]") {
		t.Error("haiku 4.5 is not natively 1M and must not get a long-context row")
	}
}

func TestOneMRowsRouteToTheAnthropicLeg(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{CatalogMode: discovery.CatalogModeStatic})
	h.Models(t.Context(), testCred)

	dec, err := router.Resolve("claude-sonnet-5[1m]", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Backend != router.BackendAnthropic {
		t.Errorf("Backend = %q, want anthropic", dec.Backend)
	}
	// The decorated id stands for the undecorated model; the client strips the
	// marker itself, and the route records what it stands for.
	if dec.ClientModel != "claude-sonnet-5" {
		t.Errorf("ClientModel = %q, want claude-sonnet-5", dec.ClientModel)
	}
	if dec.UpstreamModel != "" {
		t.Errorf("UpstreamModel = %q, want empty for the anthropic backend", dec.UpstreamModel)
	}
}

func TestOneMRowsCanBeTurnedOff(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		OneM:        discovery.OneMOptions{Disabled: true},
	})
	resp := h.Models(t.Context(), testCred)
	for _, m := range resp.Data {
		if strings.HasSuffix(m.ID, "[1m]") {
			t.Errorf("long-context row %q emitted with OneM disabled", m.ID)
		}
	}
}

func TestOneMModelListIsConfigurable(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		OneM:        discovery.OneMOptions{Models: []string{"claude-haiku-4-5"}},
	})
	resp := h.Models(t.Context(), testCred)
	if !hasID(resp, "claude-haiku-4-5[1m]") {
		t.Errorf("configured model got no long-context row; ids = %v", ids(resp))
	}
	if hasID(resp, "claude-sonnet-5[1m]") {
		t.Error("a model outside the configured list got a long-context row")
	}
}

func TestOneMRowLabelsAModelThatArrivedWithoutADisplayName(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-sonnet-5"}, // no display_name
	))
	h := mustHandler(t, discovery.Options{
		CatalogMode:           discovery.CatalogModeUpstream,
		Anthropic:             cat,
		StaticAnthropicModels: []anthropic.CatalogModel{},
	})
	resp := h.Models(t.Context(), testCred)

	if got, want := displayOf(resp, "claude-sonnet-5"), "claude-sonnet-5"; got != want {
		t.Errorf("base display = %q, want the id as a fallback", got)
	}
	if got, want := displayOf(resp, "claude-sonnet-5[1m]"), "claude-sonnet-5 (1M context)"; got != want {
		t.Errorf("long-context display = %q, want %q", got, want)
	}
}

func TestOneMRowIsNotStackedOnAnUpstreamOneMID(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-sonnet-5[1m]", DisplayName: "Sonnet 5 (1M context)"},
	))
	h := mustHandler(t, discovery.Options{
		CatalogMode:           discovery.CatalogModeUpstream,
		Anthropic:             cat,
		StaticAnthropicModels: []anthropic.CatalogModel{},
	})
	resp := h.Models(t.Context(), testCred)
	if hasID(resp, "claude-sonnet-5[1m][1m]") {
		t.Error("stacked a second [1m] marker on an id that already had one")
	}
	if !hasID(resp, "claude-sonnet-5[1m]") {
		t.Errorf("upstream row was dropped; ids = %v", ids(resp))
	}
}

// ---------------------------------------------------------------------------
// Anthropic catalog modes, deadline, redirects, and total failure
// ---------------------------------------------------------------------------

func TestUpstreamCatalogIsReadWithTheClientsOwnCredential(t *testing.T) {
	loadRegistry(t)
	var gotAuth, gotVersion, gotLimit atomic.Value
	cat, hits := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth.Store(r.Header.Get("Authorization"))
		gotVersion.Store(r.Header.Get("Anthropic-Version"))
		gotLimit.Store(r.URL.Query().Get("limit"))
		upstreamModels(anthropic.CatalogModel{ID: "claude-opus-9", DisplayName: "Opus 9"})(w, r)
	})

	h := mustHandler(t, discovery.Options{Anthropic: cat, Codex: codexOK()})
	resp := h.Models(t.Context(), testCred)

	if hits.Load() != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits.Load())
	}
	if got := gotAuth.Load(); got != testCred.Authorization {
		t.Errorf("Authorization = %v, want the client's own header verbatim", got)
	}
	if got := gotVersion.Load(); got != anthropic.DefaultAnthropicVersion {
		t.Errorf("Anthropic-Version = %v, want %q", got, anthropic.DefaultAnthropicVersion)
	}
	if got := gotLimit.Load(); got != "1000" {
		t.Errorf("limit = %v, want 1000", got)
	}
	if !hasID(resp, "claude-opus-9") {
		t.Errorf("upstream model missing from the merge; ids = %v", ids(resp))
	}
	// Merge mode also keeps the static entries upstream did not mention.
	if !hasID(resp, "claude-opus-5") {
		t.Errorf("static model missing from the merge; ids = %v", ids(resp))
	}
}

func TestNoCredentialSkipsTheUpstreamReadEntirely(t *testing.T) {
	loadRegistry(t)
	cat, hits := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-opus-9", DisplayName: "Opus 9"},
	))
	h := mustHandler(t, discovery.Options{Anthropic: cat})

	resp := h.Models(t.Context(), anthropic.Credential{})
	if hits.Load() != 0 {
		t.Errorf("upstream was contacted %d times without a credential", hits.Load())
	}
	if !hasID(resp, "claude-opus-5") {
		t.Errorf("static fallback missing; ids = %v", ids(resp))
	}
}

func TestUpstreamFailureFallsBackToStaticAndIsNegativeCached(t *testing.T) {
	loadRegistry(t)
	cat, hits := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error"}`))
	})
	h := mustHandler(t, discovery.Options{Anthropic: cat})

	for i := range 3 {
		resp := h.Models(t.Context(), testCred)
		if !hasID(resp, "claude-opus-5") {
			t.Fatalf("call %d: static fallback missing; ids = %v", i, ids(resp))
		}
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream hits = %d, want 1 — the 401 must be negative-cached", got)
	}
	if _, cached := cat.NegativeCachedUntil(); !cached {
		t.Error("expected a live negative-cache window after the 401")
	}
}

func TestUpstreamModeOffersNothingWhenUpstreamFails(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	})
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeUpstream,
		Anthropic:   cat,
		Codex:       codexOK(),
	})
	resp := h.Models(t.Context(), testCred)

	for _, m := range resp.Data {
		if !strings.HasPrefix(m.ID, "anthropic-compat.") {
			t.Errorf("upstream mode served a Claude row %q despite the upstream failure", m.ID)
		}
	}
	if len(resp.Data) == 0 {
		t.Error("the Codex half should still be served")
	}
}

func TestStaticModeNeverContactsUpstream(t *testing.T) {
	loadRegistry(t)
	cat, hits := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-opus-9", DisplayName: "Opus 9"},
	))
	h := mustHandler(t, discovery.Options{CatalogMode: discovery.CatalogModeStatic, Anthropic: cat})

	resp := h.Models(t.Context(), testCred)
	if hits.Load() != 0 {
		t.Errorf("static mode contacted upstream %d times", hits.Load())
	}
	if hasID(resp, "claude-opus-9") {
		t.Error("static mode served an upstream-only model")
	}
}

func TestDeadlineIsHonouredWithASlowUpstream(t *testing.T) {
	loadRegistry(t)
	const deadline = 150 * time.Millisecond

	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cat, _ := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		// Far slower than the deadline, and slower than the client's own 3s
		// budget, so only the internal deadline can save the response.
		select {
		case <-release:
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})

	h := mustHandler(t, discovery.Options{
		Deadline:  deadline,
		Anthropic: cat,
		Codex:     codexOK(),
	})

	start := time.Now()
	resp := h.Models(t.Context(), testCred)
	elapsed := time.Since(start)

	if elapsed > time.Second {
		t.Errorf("Models took %s; the %s deadline was not enforced", elapsed, deadline)
	}
	if !hasID(resp, "claude-opus-5") {
		t.Errorf("slow upstream should have fallen back to the static list; ids = %v", ids(resp))
	}
	if !hasID(resp, "anthropic-compat.sol") {
		t.Error("a slow Anthropic read must not take the Codex half down with it")
	}
}

func TestDeadlineIsHonouredWithASlowCodexCatalog(t *testing.T) {
	loadRegistry(t)
	const deadline = 150 * time.Millisecond

	slowCodex := discovery.CodexCatalogFunc(func(ctx context.Context) ([]schema.Model, error) {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
			return codexFixture(), nil
		}
	})
	h := mustHandler(t, discovery.Options{
		Deadline:    deadline,
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       slowCodex,
	})

	start := time.Now()
	resp := h.Models(t.Context(), testCred)
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Models took %s; the %s deadline was not enforced", elapsed, deadline)
	}
	if !hasID(resp, "claude-opus-5") {
		t.Errorf("a slow Codex read must not take the Claude half down with it; ids = %v", ids(resp))
	}
}

func TestNeverRedirects(t *testing.T) {
	loadRegistry(t)

	// Upstream tries to bounce us somewhere else. Following it would re-send
	// the caller's bearer token to that host, so it is a hard failure — and it
	// must never propagate to our own response either.
	cat, _ := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://example.invalid/v1/models")
		w.WriteHeader(http.StatusFound)
	})
	h := mustHandler(t, discovery.Options{Anthropic: cat, Codex: codexOK()})

	for _, target := range []string{
		"/v1/models",
		"/v1/models?limit=1000",
		"/v1/models?limit=nonsense",
		"/v1/models?limit=-3",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))

		if rec.Code < 200 || rec.Code >= 300 {
			t.Errorf("%s: status = %d, want 2xx — any 3xx is a hard client failure", target, rec.Code)
		}
		if loc := rec.Header().Get("Location"); loc != "" {
			t.Errorf("%s: Location = %q; a redirect must never be emitted", target, loc)
		}
		var resp discovery.Response
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Errorf("%s: body is not JSON: %v", target, err)
		}
		if !hasID(resp, "claude-opus-5") {
			t.Errorf("%s: expected the static fallback after the upstream redirect; ids = %v",
				target, ids(resp))
		}
	}
}

func TestEmptyDataOnTotalFailure(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	h := mustHandler(t, discovery.Options{
		CatalogMode:           discovery.CatalogModeUpstream,
		Anthropic:             cat,
		StaticAnthropicModels: []anthropic.CatalogModel{},
		Codex:                 codexFailing(),
	})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/models?limit=1000", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 — an empty list beats an error", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("a total failure must still not redirect")
	}
	var resp discovery.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("body must still be well-formed JSON: %v (body %q)", err, rec.Body.String())
	}
	if len(resp.Data) != 0 {
		t.Errorf("data = %v, want empty", ids(resp))
	}
	if resp.HasMore {
		t.Error("has_more should be false for an empty list")
	}
	// data must be [] and present, never null: the client's schema wants an array.
	if !strings.Contains(rec.Body.String(), `"data":[]`) {
		t.Errorf("data must serialize as [], got %s", rec.Body.String())
	}
}

func TestCodexFailureLeavesTheClaudeHalfIntact(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{
		CatalogMode: discovery.CatalogModeStatic,
		Codex:       codexFailing(),
	})
	resp := h.Models(t.Context(), testCred)
	if !hasID(resp, "claude-opus-5") {
		t.Errorf("Claude rows missing; ids = %v", ids(resp))
	}
	for _, m := range resp.Data {
		if strings.HasPrefix(m.ID, "anthropic-compat.") {
			t.Errorf("emitted a Codex row %q despite the catalog read failing", m.ID)
		}
	}
}

func TestWrongMethodIsNotARedirect(t *testing.T) {
	loadRegistry(t)
	h := mustHandler(t, discovery.Options{CatalogMode: discovery.CatalogModeStatic})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/models", nil))
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
	if rec.Header().Get("Location") != "" {
		t.Error("405 must not carry a Location")
	}
	var resp discovery.Response
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Errorf("even a 405 body must be well-formed JSON: %v", err)
	}
}

func TestDuplicateIDsAreEmittedOnce(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5 (upstream)"},
		anthropic.CatalogModel{ID: "claude-opus-5", DisplayName: "Opus 5 (dup)"},
	))
	h := mustHandler(t, discovery.Options{Anthropic: cat})
	resp := h.Models(t.Context(), testCred)

	seen := 0
	for _, m := range resp.Data {
		if m.ID == "claude-opus-5" {
			seen++
		}
	}
	if seen != 1 {
		t.Errorf("claude-opus-5 appeared %d times, want 1", seen)
	}
	if got := displayOf(resp, "claude-opus-5"); got != "Opus 5 (upstream)" {
		t.Errorf("upstream display name should win, got %q", got)
	}
}

func TestRowsFailingTheClientFilterAreDropped(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "gpt-4o", DisplayName: "Not ours"},
		anthropic.CatalogModel{ID: "claude-opus-9", DisplayName: "Opus 9"},
	))
	h := mustHandler(t, discovery.Options{
		CatalogMode:           discovery.CatalogModeUpstream,
		Anthropic:             cat,
		StaticAnthropicModels: []anthropic.CatalogModel{},
	})
	resp := h.Models(t.Context(), testCred)

	if hasID(resp, "gpt-4o") {
		t.Error("served a row the client would discard on arrival")
	}
	if !hasID(resp, "claude-opus-9") {
		t.Errorf("dropped a row that passes the filter; ids = %v", ids(resp))
	}
}

func TestConcurrentModelsCallsAreSafe(t *testing.T) {
	loadRegistry(t)
	cat, _ := fakeAnthropic(t, upstreamModels(
		anthropic.CatalogModel{ID: "claude-opus-9", DisplayName: "Opus 9"},
	))
	h := mustHandler(t, discovery.Options{Anthropic: cat, Codex: codexOK()})

	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			if resp := h.Models(context.Background(), testCred); len(resp.Data) == 0 {
				t.Error("empty catalog under concurrency")
			}
		}()
	}
	for range 8 {
		<-done
	}
}
