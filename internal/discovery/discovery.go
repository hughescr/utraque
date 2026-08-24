// Package discovery serves the merged GET /v1/models catalog that populates
// Claude Code's model picker: Anthropic's own models plus the Codex models
// utraque can route to, in one list, under ids the client will keep.
//
// # What the client actually does
//
// Verified against the Claude Code binary (2.1.226), because every constraint
// below is a hard failure rather than a degradation:
//
//   - It requests GET {base}/v1/models?limit=1000 with a 3s timeout and
//     `redirect: "error"`. ANY 3xx is a hard failure, so this handler never
//     redirects — not even the implicit redirect a mux would issue for a
//     non-canonical path.
//   - It reads only {"data":[{"id":..., "display_name":...}]}. No other field is
//     consumed. Extra fields are ignored, not rejected, so the response mirrors
//     the real API's shape anyway.
//   - It DISCARDS every id that fails /(claude|anthropic)/i — case-insensitive,
//     unanchored, a plain substring test. This is why Codex models are
//     advertised as "anthropic-compat.sol" rather than "sol".
//   - It shows the row as display_name and, on selection, sends the id back
//     verbatim as the request's model. So every id emitted here MUST route:
//     discovery registers each one in the shared router registry, and a test
//     resolves every emitted id back through the router.
//   - It only attempts gateway discovery at all when ANTHROPIC_AUTH_TOKEN or an
//     API key is set. A subscription OAuth session sends neither, so in the
//     common case the request arrives here with no credential and the Claude
//     half is served from the static list. That is a designed outcome, not a
//     degradation.
//
// # Failure posture
//
// A picker that opens with a short, stale list beats a picker that errors. The
// whole merge runs under one hard deadline (DefaultDeadline, well inside the
// client's 3s), every upstream failure has a fallback, and the terminal
// behaviour is an empty — but well-formed — data array with HTTP 200. This
// handler has no error path that reaches the client.
package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/router"
)

// Model is one row of the merged catalog. ID and DisplayName are the only
// fields the client reads; Type and CreatedAt are carried so the body matches
// the shape of Anthropic's real response.
type Model struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	Type        string `json:"type"`
	CreatedAt   string `json:"created_at,omitempty"`
}

// Response is the GET /v1/models body.
type Response struct {
	Data    []Model `json:"data"`
	HasMore bool    `json:"has_more"`
	FirstID string  `json:"first_id,omitempty"`
	LastID  string  `json:"last_id,omitempty"`
}

// emptyBody is what we serve when even encoding fails. It is a literal so that
// the last-resort path cannot itself fail.
const emptyBody = `{"data":[],"has_more":false}` + "\n"

// RouteName is what a model-picker request is called on the request line,
// alongside the "anthropic" and "codex" inference legs.
const RouteName = "discovery"

// Handler serves the merged catalog. It is safe for concurrent use.
type Handler struct {
	deadline time.Duration
	mode     string
	anth     anthropic.Catalog
	static   []anthropic.CatalogModel
	codex    CodexCatalog
	alias    AliasOptions
	oneM     OneMOptions
	reg      *router.Registry
	log      *slog.Logger
	now      func() time.Time
}

var _ http.Handler = (*Handler)(nil)

// New validates opts and builds a Handler.
func New(opts Options) (*Handler, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}
	h := &Handler{
		deadline: opts.deadline(),
		mode:     opts.catalogMode(),
		anth:     opts.Anthropic,
		static:   opts.staticModels(),
		codex:    opts.Codex,
		alias:    opts.Alias,
		oneM:     opts.OneM,
		reg:      opts.Registry,
		log:      opts.Logger,
		now:      opts.Now,
	}
	if h.reg == nil {
		h.reg = router.DefaultRegistry
	}
	if h.log == nil {
		h.log = slog.New(slog.DiscardHandler)
	}
	if h.now == nil {
		h.now = time.Now
	}
	return h, nil
}

// logger prefers the request-scoped logger, which carries the request id, and
// falls back to the handler's own. Without this a picker open that quietly
// served no GPT rows logged a warning that could not be tied to the request
// that provoked it.
func (h *Handler) logger(ctx context.Context) *slog.Logger {
	if l := obs.LoggerFrom(ctx); l != nil && l != slog.Default() {
		return l
	}
	return h.log
}

// ServeHTTP answers GET (and HEAD) /v1/models. It always writes HTTP 200 with a
// well-formed JSON body and never sets a Location header: the client treats any
// redirect as a hard failure, and an error body is worth less to it than an
// empty list.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		// The one non-200. It is not a redirect and carries no Location, so the
		// client's redirect:"error" rule is still respected; a wrong method is a
		// caller bug that should be visible rather than answered with a list.
		w.Header().Set("Allow", "GET, HEAD")
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_, _ = w.Write([]byte(emptyBody))
		return
	}

	// Discovery is not an inference leg, but it is a route, and naming it keeps
	// a picker open distinguishable from a request that failed to route at all.
	obs.SummaryFrom(r.Context()).SetRoute(RouteName)

	resp := h.Models(r.Context(), anthropic.CredentialFromRequest(r))
	resp = applyLimit(resp, parseLimit(r.URL.Query().Get("limit")))

	body, err := json.Marshal(resp)
	if err != nil {
		// Unreachable with these types, but the contract is "always well-formed
		// JSON", and a contract with an exception is not a contract.
		h.logger(r.Context()).WarnContext(r.Context(), "discovery: encoding the catalog failed; serving an empty list",
			slog.String("err", err.Error()))
		body = []byte(strings.TrimSuffix(emptyBody, "\n"))
	}
	body = append(body, '\n')

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(body)
}

// Models builds the merged catalog and registers a picker route for every id it
// emits. It is the testable core of ServeHTTP and never returns an error: each
// half degrades to its fallback independently.
//
// cred is the caller's own Anthropic credential, used only to read Anthropic's
// catalog. utraque holds no Anthropic secret of its own.
func (h *Handler) Models(ctx context.Context, cred anthropic.Credential) Response {
	ctx, cancel := context.WithTimeout(ctx, h.deadline)
	defer cancel()

	// Both upstreams are read concurrently: the deadline is a budget for the
	// whole merge, not for each half in turn.
	var (
		wg         sync.WaitGroup
		anthModels []anthropic.CatalogModel
		codexList  []schema.Model
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		anthModels = h.anthropicModels(ctx, cred)
	}()
	go func() {
		defer wg.Done()
		codexList = h.codexModels(ctx)
	}()
	wg.Wait()

	rows := make([]Model, 0, len(anthModels)+len(codexList)*2)
	routes := make(map[string]router.PickerRoute)
	seen := make(map[string]struct{})

	add := func(m Model, route router.PickerRoute, hasRoute bool) {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, dup := seen[key]; dup {
			return
		}
		if !PassesClientFilter(id) {
			// Serving it would be pure noise: the client drops it on arrival.
			h.log.Debug("discovery: dropping a row the client would discard",
				slog.String("id", id))
			return
		}
		seen[key] = struct{}{}
		if m.Type == "" {
			m.Type = modelType
		}
		if m.DisplayName == "" {
			m.DisplayName = id
		}
		rows = append(rows, m)
		if hasRoute {
			routes[key] = route
		}
	}

	for _, m := range anthModels {
		base := Model{ID: m.ID, DisplayName: m.DisplayName, Type: m.Type, CreatedAt: m.CreatedAt}
		// Normalize before deriving the long-context row so it inherits a real
		// label rather than a bare " (1M context)".
		if base.DisplayName == "" {
			base.DisplayName = base.ID
		}
		add(base, router.PickerRoute{}, false)
		if row, route, ok := h.oneMRow(base); ok {
			add(row, route, true)
		}
	}

	for _, row := range h.codexRows(codexList) {
		add(row.model, row.route, true)
	}

	// Registration is the invariant that makes a picked row work. It happens
	// even when a half failed: the routes we did emit must resolve, and a route
	// for a row we no longer serve must stop resolving.
	h.reg.SetPickerRoutes(routes)

	resp := Response{Data: rows}
	if len(rows) > 0 {
		resp.FirstID = rows[0].ID
		resp.LastID = rows[len(rows)-1].ID
	}
	return resp
}

// anthropicModels resolves the Claude half per the configured catalog mode,
// falling back to the static list wherever upstream cannot answer.
func (h *Handler) anthropicModels(ctx context.Context, cred anthropic.Credential) []anthropic.CatalogModel {
	if h.mode == CatalogModeStatic || h.anth == nil {
		return h.static
	}

	upstream, err := h.anth.Models(ctx, cred)
	if err != nil {
		switch {
		case errors.Is(err, anthropic.ErrNoCredential):
			// The ordinary case for a subscription session. Debug, not warn.
			h.logger(ctx).DebugContext(ctx, "discovery: no client credential for the anthropic catalog; using the static list")
		default:
			h.logger(ctx).WarnContext(ctx, "discovery: anthropic catalog read failed; using the static list",
				slog.String("err", err.Error()))
		}
		if h.mode == CatalogModeUpstream {
			return nil
		}
		return h.static
	}

	if h.mode == CatalogModeUpstream {
		return upstream
	}

	// Merge: upstream first (its display names and ids are authoritative), then
	// any static entry upstream did not mention.
	seen := make(map[string]struct{}, len(upstream))
	out := make([]anthropic.CatalogModel, 0, len(upstream)+len(h.static))
	for _, m := range upstream {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		seen[strings.ToLower(m.ID)] = struct{}{}
		out = append(out, m)
	}
	for _, m := range h.static {
		if _, dup := seen[strings.ToLower(m.ID)]; dup {
			continue
		}
		out = append(out, m)
	}
	return out
}

// codexModels reads the Codex catalog, treating any failure as "no GPT rows".
func (h *Handler) codexModels(ctx context.Context) []schema.Model {
	if h.codex == nil || h.alias.strategy() == AliasOff {
		return nil
	}
	models, err := h.codex.Models(ctx)
	if err != nil {
		h.logger(ctx).WarnContext(ctx, "discovery: codex catalog read failed; offering no GPT rows",
			slog.String("err", err.Error()))
		return nil
	}
	return models
}

// oneMRow derives the explicit long-context row for a native-1M Claude model.
// ok=false means this model is not one of them, or the feature is off.
func (h *Handler) oneMRow(base Model) (Model, router.PickerRoute, bool) {
	if h.oneM.Disabled {
		return Model{}, router.PickerRoute{}, false
	}
	suffix := h.oneM.idSuffix()
	lower := strings.ToLower(base.ID)
	if strings.Contains(lower, strings.ToLower(suffix)) {
		// Upstream already advertises the long-context form; don't stack a
		// second marker on it.
		return Model{}, router.PickerRoute{}, false
	}
	matched := false
	for _, prefix := range h.oneM.models() {
		if prefix != "" && strings.HasPrefix(lower, strings.ToLower(prefix)) {
			matched = true
			break
		}
	}
	if !matched {
		return Model{}, router.PickerRoute{}, false
	}

	label := base.DisplayName
	if label == "" {
		label = base.ID
	}
	row := Model{
		ID:          base.ID + suffix,
		DisplayName: label + h.oneM.displaySuffix(),
		Type:        base.Type,
		CreatedAt:   base.CreatedAt,
	}
	// The route carries the undecorated id. The client strips "[1m]" itself
	// before sending, so this is belt-and-braces — but it is what makes the
	// decorated id resolve if anything ever does send it verbatim.
	route := router.PickerRoute{Backend: router.BackendAnthropic, UpstreamModel: base.ID}
	return row, route, true
}

// parseLimit reads the client's ?limit=. A missing, malformed, or non-positive
// value means "no limit" — the client always sends 1000, far above any real
// catalog, so this only ever matters to a hand-written request.
func parseLimit(raw string) int {
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// applyLimit truncates the response to limit rows, keeping has_more and the
// first/last ids honest.
func applyLimit(resp Response, limit int) Response {
	if limit <= 0 || len(resp.Data) <= limit {
		return resp
	}
	resp.Data = resp.Data[:limit]
	resp.HasMore = true
	resp.LastID = resp.Data[len(resp.Data)-1].ID
	return resp
}
