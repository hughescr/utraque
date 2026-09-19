package catalog

import (
	"github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
)

// ListedEntries maps the advertised (visibility=="list") models to the minimal
// router.CatalogEntry shape the alias registry derives its tiers from. Hidden
// models are dropped here so they are never advertised, though the router still
// routes any raw slug a client names directly (see Model.Listed and the
// registry's raw tier).
//
// Priority is carried through because it is the tiebreaker in the registry's
// bare-alias collision rule: same codename, same version -> higher priority
// wins the rolling bare name.
func ListedEntries(models []cschema.CatalogModel) []router.CatalogEntry {
	out := make([]router.CatalogEntry, 0, len(models))
	for _, m := range models {
		if !m.Listed() || m.Slug == "" {
			continue
		}
		out = append(out, router.CatalogEntry{Slug: m.Slug, Priority: m.Priority})
	}
	return out
}

// PopulateRegistry derives the multi-tier aliases (raw slug, pinned
// codename-version, rolling bare codename, and — via the picker layer — the
// anthropic-compat.* variants) from the live catalog and loads them into reg,
// replacing whatever it held before. It is the Phase 3 use of the registry's
// LoadCatalog hook: the router's tier-derivation and collision logic are reused
// unchanged; only the source of slugs moves from the static seed to the live
// catalog.
//
// This package only adapts a model list into the registry. The caller (cmd's
// alias loader) owns WHEN to publish: it feeds each Models result through here
// and decides how to treat a stale or empty read. There is deliberately no
// fetch-and-publish convenience in this package.
//
// Any slug-shape irregularity the grammar can't parse (e.g. a two-token tail
// like gpt-5.3-codex-spark) is handled by an override registered on reg via
// reg.SetOverride before this call; LoadCatalog consults the registered
// overrides on every rebuild.
func PopulateRegistry(reg *router.Registry, models []cschema.CatalogModel) {
	reg.LoadCatalog(ListedEntries(models))
}
