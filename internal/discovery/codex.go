package discovery

import (
	"sort"
	"strings"

	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
)

// codexRow pairs an emitted picker row with the route that makes it work.
type codexRow struct {
	model Model
	route router.PickerRoute
}

// codexRows turns the Codex catalog into picker rows under the configured alias
// strategy.
//
// The names come from the router registry, not from a parser of our own: the
// registry is what actually resolves a model string, so advertising anything it
// does not hold would be advertising a dead row. The one exception is a catalog
// slug the registry has never seen — a hidden model, or one that arrived
// between catalog refreshes — which is offered under its raw slug and made
// routable by the picker-route registration.
func (h *Handler) codexRows(models []schema.Model) []codexRow {
	strategy := h.alias.strategy()
	if strategy == AliasOff || len(models) == 0 {
		return nil
	}

	// Eligible catalog models, keyed by slug.
	bySlug := make(map[string]schema.Model, len(models))
	slugs := make([]string, 0, len(models))
	for _, m := range models {
		slug := strings.ToLower(strings.TrimSpace(m.Slug))
		if slug == "" {
			continue
		}
		if !m.Listed() && !h.alias.IncludeHidden {
			continue
		}
		if _, dup := bySlug[slug]; dup {
			continue
		}
		bySlug[slug] = m
		slugs = append(slugs, slug)
	}
	// Highest catalog priority first — that is the ordering hint Codex itself
	// publishes, and a picker should lead with the model the backend considers
	// foremost rather than with whichever slug sorts first alphabetically.
	// Equal priorities fall back to the slug so the order is deterministic.
	sort.Slice(slugs, func(i, j int) bool {
		pi, pj := bySlug[slugs[i]].Priority, bySlug[slugs[j]].Priority
		if pi != pj {
			return pi > pj
		}
		return slugs[i] < slugs[j]
	})

	// Registry aliases grouped by the slug they resolve to. Raw-tier names are
	// kept separately: AliasPassthrough wants exactly those, and the other
	// strategies want exactly the derived ones.
	derived := make(map[string][]string)
	for _, a := range h.reg.AliasList() {
		if a.Tier == router.TierRaw {
			continue
		}
		derived[a.Slug] = append(derived[a.Slug], a.Name)
	}

	idTmpl, displayTmpl := h.alias.idTemplate(), h.alias.displayTemplate()
	out := make([]codexRow, 0, len(slugs)*2)

	for _, slug := range slugs {
		model := bySlug[slug]
		display := model.DisplayName
		if display == "" {
			display = slug
		}

		var names []string
		switch strategy {
		case AliasPassthrough:
			names = []string{slug}
		default:
			names = orderAliases(derived[slug])
			if len(names) == 0 {
				// Unknown to the registry: offer the slug so the model is at
				// least reachable rather than silently absent.
				names = []string{slug}
			}
		}

		for _, name := range names {
			out = append(out, h.codexRow(idTmpl, displayTmpl, name, slug, display, ""))

			if strategy != AliasEffortVariants {
				continue
			}
			for _, effort := range model.SupportedEfforts() {
				if effort == "" {
					continue
				}
				// Only efforts the router can split back off a model name. An
				// "<alias>-<effort>" id whose effort token the grammar does not
				// know is routable ONLY while the in-memory picker tier that
				// recorded it survives; after a restart — or after one picker
				// open hits a catalog error and rebuilds the tier without it —
				// the same id hard-404s. A row that dies like that is worse than
				// a row we never offered.
				if !router.KnownEffort(effort) {
					continue
				}
				out = append(out, h.codexRow(idTmpl, displayTmpl,
					name+"-"+effort, slug, display, effort))
			}
		}
	}
	return out
}

// codexRow renders one row and its route.
func (h *Handler) codexRow(idTmpl, displayTmpl, alias, slug, display, effort string) codexRow {
	vars := templateVars{Alias: alias, Slug: slug, Display: display, Effort: effort}
	return codexRow{
		model: Model{
			ID:          render(idTmpl, vars),
			DisplayName: render(displayTmpl, vars),
			Type:        modelType,
		},
		route: router.PickerRoute{
			Backend:       router.BackendCodex,
			UpstreamModel: slug,
			Effort:        effort,
		},
	}
}

// orderAliases puts the rolling name before the pinned one — "sol" reads as the
// obvious choice and "sol-5.6" as the deliberate pin, and a picker should offer
// them in that order. Within a length, ties break lexically so the output is
// deterministic.
//
// Length is the proxy for "rolling": a pinned alias is its bare alias plus a
// version, so it is always longer. Comparing tiers directly would be more
// direct but would mean carrying the tier through the grouping for a rule with
// exactly one bit of information in it.
func orderAliases(in []string) []string {
	if len(in) < 2 {
		return in
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) < len(out[j])
		}
		return out[i] < out[j]
	})
	return out
}
