package discovery

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/hughescr/utraque/internal/anthropic"
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
)

// Catalog modes for the Anthropic half of the merge.
const (
	// CatalogModeMerge (the default) reads Anthropic's own catalog with the
	// caller's credential and unions it with the static list, so a model the
	// upstream read omits is still offered. Upstream display names win.
	CatalogModeMerge = "merge"
	// CatalogModeUpstream offers only what the upstream read returned. If that
	// read fails there are no Claude rows at all — the honest answer when the
	// operator has said "upstream is the only truth".
	CatalogModeUpstream = "upstream"
	// CatalogModeStatic never contacts Anthropic; the static list is served as
	// is. The fastest mode, and the one that cannot leak a picker open into an
	// upstream request.
	CatalogModeStatic = "static"
)

// Alias-emission strategies for the Codex half of the merge.
const (
	// AliasOff emits no Codex rows at all. The proxy still routes GPT names
	// typed by hand or set in agent frontmatter; they just do not appear in the
	// picker.
	AliasOff = "off"
	// AliasTemplate (the default) emits the router's rolling and pinned aliases
	// — "sol" and "sol-5.6" — through IDTemplate.
	AliasTemplate = "template"
	// AliasEffortVariants emits everything AliasTemplate does, plus one row per
	// reasoning effort the model supports ("sol-high", "sol-5.6-high"). Useful,
	// and long: a five-effort model turns two rows into twelve.
	AliasEffortVariants = "effort_variants"
	// AliasPassthrough emits one row per upstream slug ("gpt-5.6-sol") through
	// IDTemplate, with no synthesised aliases. For when you want the picker to
	// name exactly what Codex serves.
	AliasPassthrough = "passthrough"
)

// Defaults for the handler.
const (
	// DefaultDeadline bounds the whole merge. The client gives up at 3s and
	// treats any failure as "no gateway models", so the internal budget is set
	// well inside that: better a static list on time than a perfect one late.
	DefaultDeadline = 1500 * time.Millisecond

	// DefaultIDTemplate is the prefixed compat form. It passes the client's
	// current unanchored /(claude|anthropic)/i test AND a hypothetical future
	// starts-with rule, which the suffixed form ("sol.anthropic-compat") would
	// not.
	DefaultIDTemplate = "anthropic-compat.{alias}"

	// DefaultCodexDisplayTemplate labels a Codex row with the catalog's own
	// display name plus the alias to type, e.g. "GPT-5.6-Sol (sol)".
	DefaultCodexDisplayTemplate = "{display} ({alias})"

	// modelType is the "type" field of a model row in Anthropic's API.
	modelType = "model"
)

// clientFilter is the rule Claude Code applies to every discovered id:
// a case-insensitive, unanchored search for "claude" or "anthropic". Ids that
// fail it are silently dropped by the client, so utraque drops them first and
// says so in the log rather than serving rows that cannot be picked.
//
// Verified against the client binary (2.1.226): `/(claude|anthropic)/i.test(id)`.
var clientFilter = regexp.MustCompile(`(?i)(claude|anthropic)`)

// PassesClientFilter reports whether Claude Code would keep a row with this id.
func PassesClientFilter(id string) bool { return clientFilter.MatchString(id) }

// CodexCatalog is the Codex-side model source. It is deliberately
// credential-free: obtaining and refreshing the Codex OAuth token is the auth
// package's job, and discovery must never be a reason to rotate it. The caller
// adapts its catalog client to this shape (see CodexCatalogFunc).
type CodexCatalog interface {
	Models(ctx context.Context) ([]schema.Model, error)
}

// CodexCatalogFunc adapts a function to CodexCatalog.
type CodexCatalogFunc func(ctx context.Context) ([]schema.Model, error)

// Models implements CodexCatalog.
func (f CodexCatalogFunc) Models(ctx context.Context) ([]schema.Model, error) { return f(ctx) }

// AliasOptions governs the Codex half of the merged catalog.
//
// The zero value means "on, with the default template" — alias emission is on
// by default, per the plan. Set Strategy to AliasOff to turn it off.
type AliasOptions struct {
	// Strategy selects which names become picker rows. Empty means
	// AliasTemplate.
	Strategy string
	// IDTemplate renders the emitted id. Placeholders: {alias}, {slug},
	// {display}, {effort}. Empty means DefaultIDTemplate.
	//
	// Whatever it renders, an id that fails the client's own
	// /(claude|anthropic)/i filter is dropped rather than served — see
	// clientFilter. A template with no filter-passing literal in it therefore
	// yields no Codex rows at all, which New rejects up front.
	IDTemplate string
	// DisplayTemplate renders the picker label. Same placeholders. Empty means
	// DefaultCodexDisplayTemplate.
	DisplayTemplate string
	// IncludeHidden also offers models the Codex catalog marks
	// visibility != "list". Off by default: a hidden model is hidden for a
	// reason, and utraque advertising it is a decision the operator should make
	// explicitly.
	//
	// A hidden slug normally has no aliases in the router registry (the
	// registry is loaded from listed models only), so it is offered under its
	// raw slug. That row still routes, because discovery registers the exact id
	// it emitted.
	IncludeHidden bool
}

func (a AliasOptions) strategy() string {
	if a.Strategy == "" {
		return AliasTemplate
	}
	return a.Strategy
}

func (a AliasOptions) idTemplate() string {
	if a.IDTemplate == "" {
		return DefaultIDTemplate
	}
	return a.IDTemplate
}

func (a AliasOptions) displayTemplate() string {
	if a.DisplayTemplate == "" {
		return DefaultCodexDisplayTemplate
	}
	return a.DisplayTemplate
}

// Options configures a Handler. Every field has a working default; New with a
// zero Options serves the static Claude list plus nothing else, which is a
// valid — if dull — catalog.
type Options struct {
	// Deadline bounds the whole merge, upstream reads included. Zero or
	// negative means DefaultDeadline.
	Deadline time.Duration

	// CatalogMode is merge (default), upstream, or static.
	CatalogMode string

	// Anthropic reads Anthropic's own catalog. Nil behaves like
	// CatalogModeStatic for the Claude half.
	Anthropic anthropic.Catalog

	// StaticAnthropicModels overrides the built-in Claude fallback list. Nil
	// means StaticAnthropicModels(). An explicitly empty (non-nil) slice means
	// "no static fallback", which is how a caller asks for upstream-or-nothing
	// without giving up the merge mode's other behaviour.
	StaticAnthropicModels []anthropic.CatalogModel

	// Codex is the Codex-side model source. Nil means no Codex rows.
	Codex CodexCatalog

	// Alias governs Codex row emission.
	Alias AliasOptions

	// Registry receives a picker route for every emitted id, so a picked row
	// always routes back. Nil means router.DefaultRegistry.
	Registry *router.Registry

	// Logger receives operational logs. Nil discards.
	Logger *slog.Logger

	// Now is the clock. Nil means time.Now.
	Now func() time.Time
}

func (o Options) deadline() time.Duration {
	if o.Deadline <= 0 {
		return DefaultDeadline
	}
	return o.Deadline
}

func (o Options) catalogMode() string {
	if o.CatalogMode == "" {
		return CatalogModeMerge
	}
	return o.CatalogMode
}

func (o Options) staticModels() []anthropic.CatalogModel {
	if o.StaticAnthropicModels == nil {
		return staticClaudeModels
	}
	return o.StaticAnthropicModels
}

// validate reports the first configuration problem.
func (o Options) validate() error {
	switch o.catalogMode() {
	case CatalogModeMerge, CatalogModeUpstream, CatalogModeStatic:
	default:
		return fmt.Errorf("utraque/discovery: catalog_mode %q: want %s|%s|%s",
			o.CatalogMode, CatalogModeMerge, CatalogModeUpstream, CatalogModeStatic)
	}

	strategy := o.Alias.strategy()
	switch strategy {
	case AliasOff:
		return nil
	case AliasTemplate, AliasEffortVariants, AliasPassthrough:
	default:
		return fmt.Errorf("utraque/discovery: alias strategy %q: want %s|%s|%s|%s",
			o.Alias.Strategy, AliasOff, AliasTemplate, AliasEffortVariants, AliasPassthrough)
	}

	tmpl := o.Alias.idTemplate()
	if !strings.Contains(tmpl, "{alias}") {
		return fmt.Errorf("utraque/discovery: alias id_template %q must contain {alias}", tmpl)
	}
	// A template whose literal text carries no "claude"/"anthropic" can only
	// produce filter-passing ids by accident of the alias itself. Reject it at
	// construction rather than silently serving a picker with no GPT rows.
	if !PassesClientFilter(render(tmpl, templateVars{Alias: "probe", Slug: "probe", Display: "probe"})) {
		return fmt.Errorf("utraque/discovery: alias id_template %q renders ids that fail the client's "+
			"/(claude|anthropic)/i filter, so no Codex model would ever reach the picker", tmpl)
	}
	return nil
}

// templateVars are the placeholder values available to IDTemplate and
// DisplayTemplate.
type templateVars struct {
	Alias   string
	Slug    string
	Display string
	Effort  string
}

// render substitutes the placeholders in tmpl. An unknown placeholder is left
// as written, which makes a typo visible in the picker instead of silent.
func render(tmpl string, v templateVars) string {
	return strings.NewReplacer(
		"{alias}", v.Alias,
		"{slug}", v.Slug,
		"{display}", v.Display,
		"{effort}", v.Effort,
	).Replace(tmpl)
}
