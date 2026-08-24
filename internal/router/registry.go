// Package router resolves a client-supplied model string to a routing
// Decision (Anthropic passthrough vs. Codex alias), and holds the
// multi-tier alias registry that maps short Codex names to upstream slugs.
package router

import (
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// CatalogEntry is the minimal shape router needs from a live model-catalog
// entry. Phase 3's internal/codex/catalog will supply these; router itself
// never fetches anything — LoadCatalog is the seam.
//
// Priority is the catalog's own ordering hint for the model. It only ever
// matters as the tiebreaker in the bare-alias collision rule: when two slugs
// carry the same codename *and* the same version, the higher Priority wins
// the rolling bare name. When versions differ, the newer version wins outright
// and Priority is irrelevant. A zero Priority (the default) is fine — it just
// means "no preference", and equal-version/equal-priority ties fall to the
// deterministic slug order LoadCatalog already imposes.
type CatalogEntry struct {
	Slug     string
	Priority int
}

// slugOverride pins the parse result for a slug the grammar can't be
// trusted to parse, per the plan's routing.alias_overrides config (Phase
// 3/4). Set via Registry.SetOverride.
type slugOverride struct {
	Codename string
	Version  string
	Modifier string
}

// parsedSlug is the decomposition of one upstream model slug into the
// pieces the alias tiers are built from.
type parsedSlug struct {
	Slug     string
	Version  string
	Codename string
	Modifier string
}

// modifiers are trailing slug tokens that are size/variant modifiers, not
// codenames (e.g. "mini"), so they never win a bare codename alias.
var modifiers = map[string]bool{
	"mini": true,
}

// slugPattern matches "gpt-<version>[-<tail>]" where <tail> is a single
// trailing token (a codename or a modifier). Slugs with more than one
// trailing token (e.g. "gpt-5.3-codex-spark") don't match this pattern and
// must be resolved via an override instead.
var slugPattern = regexp.MustCompile(`^gpt-(\d+(?:\.\d+)*)(?:-([a-z0-9]+))?$`)

// parseSlug splits a raw slug into version/codename/modifier. Overrides are
// consulted first since they exist specifically for slugs the regular
// grammar parses wrong (or not at all).
func parseSlug(slug string, overrides map[string]slugOverride) (parsedSlug, bool) {
	lower := strings.ToLower(slug)
	if ov, ok := overrides[lower]; ok {
		return parsedSlug{Slug: lower, Version: ov.Version, Codename: ov.Codename, Modifier: ov.Modifier}, true
	}
	m := slugPattern.FindStringSubmatch(lower)
	if m == nil {
		return parsedSlug{}, false
	}
	version, tail := m[1], m[2]
	p := parsedSlug{Slug: lower, Version: version}
	if tail == "" {
		return p, true
	}
	if modifiers[tail] {
		p.Modifier = tail
	} else {
		p.Codename = tail
	}
	return p, true
}

// bareAlias is the alias key a parsed slug wants for the "bare" (rolling,
// newest-wins) tier: the codename if it has one, else "<version>[-<modifier>]".
func (p parsedSlug) bareAlias() string {
	if p.Codename != "" {
		return p.Codename
	}
	if p.Modifier != "" {
		return p.Version + "-" + p.Modifier
	}
	return p.Version
}

// pinnedAlias is the alias key for the "pinned" tier: "<codename>-<version>".
// Codename-less (version-only) slugs have no separate pinned form — their
// bare alias already names the exact version.
func (p parsedSlug) pinnedAlias() string {
	if p.Codename == "" {
		return ""
	}
	return p.Codename + "-" + p.Version
}

// Registry is the multi-tier alias table: raw slugs always resolve to
// themselves; pinned aliases name one exact version; bare aliases float to
// the newest version carrying that codename (or are the version itself, for
// codename-less slugs). All three tiers point at the same upstream slugs.
//
// Seeded statically for Phase 1/2 via NewStaticRegistry; Phase 3
// repopulates it from the live Codex catalog via LoadCatalog, using the
// same tier-derivation logic so behaviour doesn't change when the source
// does.
type Registry struct {
	mu        sync.RWMutex
	raw       map[string]string
	pinned    map[string]string
	bare      map[string]string
	overrides map[string]slugOverride

	// picker is the fourth tier: whole, decorated ids that internal/discovery
	// actually advertised, mapped to the backend each must reach. It is
	// maintained by SetPickerRoutes and is deliberately untouched by
	// LoadCatalog — see SetPickerRoutes for why.
	picker map[string]PickerRoute
}

// bareHold records which (version, priority) currently owns a bare alias, so
// the collision rule can decide whether a later slug outranks the incumbent.
type bareHold struct {
	version  string
	priority int
}

// NewRegistry returns an empty registry (no seed data, no overrides).
// Exposed for tests and for callers that want to seed only from a catalog.
func NewRegistry() *Registry {
	return &Registry{
		raw:       map[string]string{},
		pinned:    map[string]string{},
		bare:      map[string]string{},
		overrides: map[string]slugOverride{},
		picker:    map[string]PickerRoute{},
	}
}

// staticSlugs is the Phase 1/2 fallback catalog: the live slugs the plan
// recorded from models_cache.json. Phase 3 replaces this *source*, not the
// tiering logic, by calling LoadCatalog with live entries instead.
var staticSlugs = []string{
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.5",
	"gpt-5.4",
	"gpt-5.4-mini",
	"gpt-5.3-codex-spark",
}

// staticOverrides seeds the one irregular slug the grammar can't parse:
// "gpt-5.3-codex-spark" has two trailing tokens ("codex", "spark"); the
// codename is "spark", not "codex".
var staticOverrides = map[string]slugOverride{
	"gpt-5.3-codex-spark": {Codename: "spark", Version: "5.3"},
}

// NewStaticRegistry returns a Registry seeded from the Phase 1/2 static slug
// table above. DefaultRegistry (in resolve.go) is one of these; Phase 3
// swaps the source by calling LoadCatalog on it with live catalog entries.
func NewStaticRegistry() *Registry {
	r := NewRegistry()
	for slug, ov := range staticOverrides {
		r.overrides[slug] = ov
	}
	entries := make([]CatalogEntry, len(staticSlugs))
	for i, s := range staticSlugs {
		entries[i] = CatalogEntry{Slug: s}
	}
	r.LoadCatalog(entries)
	return r
}

// SetOverride registers (or replaces) a config-driven alias override for a
// slug the grammar parses wrong or not at all. This is the
// routing.alias_overrides hook Phase 3/4's config loader will call. Call it
// before LoadCatalog (or call LoadCatalog again afterward) so the override
// takes effect — LoadCatalog re-derives every alias from scratch each call.
func (r *Registry) SetOverride(slug string, codename, version, modifier string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.overrides[strings.ToLower(slug)] = slugOverride{Codename: codename, Version: version, Modifier: modifier}
}

// LoadCatalog replaces the registry's raw/pinned/bare tiers by re-deriving
// aliases from the given slugs, applying whatever overrides are already
// registered. This is the Phase 3 entry point: fetch the live Codex
// catalog, map each model to a CatalogEntry, call LoadCatalog. A slug that
// disappears from the catalog stops being routable (tiers are rebuilt from
// scratch, not merged), which is the intended behaviour — router should
// only route to models Codex currently serves.
func (r *Registry) LoadCatalog(entries []CatalogEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()

	raw := map[string]string{}
	pinned := map[string]string{}
	bare := map[string]string{}
	held := map[string]bareHold{} // alias -> (version, priority) currently held

	// Stable order so equal-version/equal-priority collisions resolve
	// deterministically, independent of catalog iteration order.
	sorted := make([]CatalogEntry, len(entries))
	copy(sorted, entries)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Slug < sorted[j].Slug })

	for _, e := range sorted {
		slug := strings.ToLower(e.Slug)
		raw[slug] = slug

		p, ok := parseSlug(slug, r.overrides)
		if !ok {
			// Grammar can't place it and no override exists: still
			// routable by raw slug, just no derived aliases. A future
			// config override can add the missing shape.
			continue
		}

		if pa := p.pinnedAlias(); pa != "" {
			pinned[pa] = slug
		}

		ba := p.bareAlias()
		// Collision rule: the newest (version, priority) wins the bare
		// (rolling) name; both versions keep their own pinned alias
		// regardless. Version dominates; priority only breaks a same-version
		// tie.
		cand := bareHold{version: p.Version, priority: e.Priority}
		if cur, exists := held[ba]; !exists || bareOutranks(cand, cur) {
			bare[ba] = slug
			held[ba] = cand
		}
	}

	r.raw, r.pinned, r.bare = raw, pinned, bare
}

// bareOutranks reports whether candidate should take the bare alias from the
// incumbent: a newer version wins outright; an equal version defers to the
// higher priority. Equal on both keeps the incumbent (LoadCatalog's stable
// slug sort makes that deterministic).
func bareOutranks(candidate, incumbent bareHold) bool {
	switch versionCmp(candidate.version, incumbent.version) {
	case 1:
		return true
	case -1:
		return false
	default:
		return candidate.priority > incumbent.priority
	}
}

// versionCmp compares dotted numeric versions ("5.6" vs "5.10") segment by
// segment, numerically, so "5.10" ranks newer than "5.6". It returns -1, 0, or
// 1. Malformed/non-numeric segments fall back to a lexical compare — good
// enough for the collision rule without a semver dependency (the module has
// none).
func versionCmp(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) || i < len(bs); i++ {
		var av, bv string
		if i < len(as) {
			av = as[i]
		}
		if i < len(bs) {
			bv = bs[i]
		}
		an, aerr := strconv.Atoi(av)
		bn, berr := strconv.Atoi(bv)
		if aerr == nil && berr == nil {
			switch {
			case an < bn:
				return -1
			case an > bn:
				return 1
			}
			continue
		}
		switch {
		case av < bv:
			return -1
		case av > bv:
			return 1
		}
	}
	return 0
}

// Resolve looks up name (expected already lowercased and effort-suffix-
// stripped by the caller) against raw, then pinned, then bare tiers — raw
// slugs are never shadowed by a derived alias, and a pinned exact version
// always beats the rolling bare name.
func (r *Registry) Resolve(name string) (upstream string, ok bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if u, ok := r.raw[name]; ok {
		return u, true
	}
	if u, ok := r.pinned[name]; ok {
		return u, true
	}
	if u, ok := r.bare[name]; ok {
		return u, true
	}
	return "", false
}

// Families lists the known *bare* (rolling) route family names — e.g. "sol",
// "spark", "5.4-mini" — sorted, for the unknown-model 404 message. Raw and
// pinned keys are deliberately excluded to keep that message short; the
// wildcard families (claude-*, anthropic-*, gpt-*) are prepended by the
// caller in resolve.go, not here.
func (r *Registry) Families() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.bare))
	for k := range r.bare {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// AliasTier names which of the registry's three derivation tiers an alias came
// from. Discovery uses it to choose which names to advertise: the raw tier is
// the upstream slug itself, pinned names one exact version, and bare is the
// rolling newest-wins name.
type AliasTier string

// The alias tiers, in the order Resolve consults them.
const (
	TierRaw    AliasTier = "raw"
	TierPinned AliasTier = "pinned"
	TierBare   AliasTier = "bare"
)

// Alias is one registered routable name and the upstream slug it resolves to.
type Alias struct {
	Name string
	Slug string
	Tier AliasTier
}

// AliasList returns every alias the registry currently resolves, across all
// three derivation tiers, sorted by (Slug, Tier, Name) so callers get a stable
// order. Tier ordering is the constant order above (raw, pinned, bare), not
// lexical.
//
// It is the seam internal/discovery advertises from: a name absent from this
// list is not routable, so discovery must never invent one.
func (r *Registry) AliasList() []Alias {
	r.mu.RLock()
	defer r.mu.RUnlock()

	out := make([]Alias, 0, len(r.raw)+len(r.pinned)+len(r.bare))
	for name, slug := range r.raw {
		out = append(out, Alias{Name: name, Slug: slug, Tier: TierRaw})
	}
	for name, slug := range r.pinned {
		out = append(out, Alias{Name: name, Slug: slug, Tier: TierPinned})
	}
	for name, slug := range r.bare {
		out = append(out, Alias{Name: name, Slug: slug, Tier: TierBare})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Slug != out[j].Slug {
			return out[i].Slug < out[j].Slug
		}
		if out[i].Tier != out[j].Tier {
			return tierRank(out[i].Tier) < tierRank(out[j].Tier)
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func tierRank(t AliasTier) int {
	switch t {
	case TierRaw:
		return 0
	case TierPinned:
		return 1
	case TierBare:
		return 2
	default:
		return 3
	}
}

// PickerRoute is the routing target of one explicitly advertised picker id.
//
// Discovery emits decorated ids ("anthropic-compat.sol", "claude-sonnet-5[1m]")
// that the alias grammar alone would place wrongly, or not at all, so it
// registers the exact id it advertised together with the backend that id must
// reach. Without this a user could pick a row utraque itself served and get a
// 404 back.
//
// For BackendCodex, UpstreamModel is the catalog slug to request. For
// BackendAnthropic it is the undecorated Anthropic model id the decorated
// picker id stands for (e.g. "claude-sonnet-5" behind "claude-sonnet-5[1m]").
type PickerRoute struct {
	Backend       Backend
	UpstreamModel string
	Effort        string
}

// SetPickerRoutes replaces the whole picker-id tier with routes. Discovery
// rebuilds it every time it serves a catalog, so a row that stops being
// advertised stops being routable by its decorated id (the underlying alias is
// unaffected). A nil or empty map clears the tier. Entries with an empty id or
// an invalid backend are dropped.
//
// The picker tier is deliberately NOT rebuilt by LoadCatalog: the two have
// different lifetimes — the catalog refreshes on a timer, discovery on a picker
// open — and clearing one from the other would silently un-route ids already
// showing in a user's model picker.
func (r *Registry) SetPickerRoutes(routes map[string]PickerRoute) {
	next := make(map[string]PickerRoute, len(routes))
	for id, route := range routes {
		key := strings.ToLower(strings.TrimSpace(id))
		if key == "" || !route.Backend.Valid() {
			continue
		}
		next[key] = route
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.picker = next
}

// PickerRoute looks up an explicitly advertised picker id. id is expected to be
// already lowercased and trimmed.
func (r *Registry) PickerRoute(id string) (PickerRoute, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	route, ok := r.picker[id]
	return route, ok
}

// PickerIDs lists the registered picker ids, sorted.
func (r *Registry) PickerIDs() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.picker))
	for id := range r.picker {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
