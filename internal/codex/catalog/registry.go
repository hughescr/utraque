package catalog

import (
	"context"

	"github.com/hughescr/utraque/internal/codex/auth"
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
func ListedEntries(models []schema.Model) []router.CatalogEntry {
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
// Any slug-shape irregularity the grammar can't parse (e.g. a two-token tail
// like gpt-5.3-codex-spark) is handled by an override registered on reg via
// reg.SetOverride before this call; LoadCatalog consults the registered
// overrides on every rebuild.
func PopulateRegistry(reg *router.Registry, models []schema.Model) {
	reg.LoadCatalog(ListedEntries(models))
}

// RefreshRegistry fetches the current catalog and populates reg from it. It is
// the one-call convenience a caller (e.g. the server bootstrap or a periodic
// refresh) uses to keep the router's aliases current with what Codex serves.
// On fetch failure reg is left untouched and the error is returned.
//
// It uses the blocking current-catalog path, not Models: Models serves a stale
// snapshot immediately and revalidates in the background, which would install a
// stale model list into the live router and still report success. A registry
// refresh must publish the actually-current catalog or fail.
func (c *Client) RefreshRegistry(ctx context.Context, cred auth.Credential, reg *router.Registry) error {
	models, err := c.currentModels(ctx, cred)
	if err != nil {
		return err
	}
	PopulateRegistry(reg, models)
	return nil
}
