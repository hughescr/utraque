package router_test

import (
	"reflect"
	"testing"

	"github.com/hughescr/utraque/internal/router"
)

func TestAliasListCoversEveryTier(t *testing.T) {
	reg := router.NewRegistry()
	reg.LoadCatalog([]router.CatalogEntry{
		{Slug: "gpt-5.6-sol", Priority: 10},
		{Slug: "gpt-5.5", Priority: 5},
	})

	got := reg.AliasList()
	want := []router.Alias{
		{Name: "5.5", Slug: "gpt-5.5", Tier: router.TierBare},
		{Name: "gpt-5.5", Slug: "gpt-5.5", Tier: router.TierRaw},
		{Name: "gpt-5.6-sol", Slug: "gpt-5.6-sol", Tier: router.TierRaw},
		{Name: "sol-5.6", Slug: "gpt-5.6-sol", Tier: router.TierPinned},
		{Name: "sol", Slug: "gpt-5.6-sol", Tier: router.TierBare},
	}
	// Sorted by (Slug, Tier, Name); tier order is raw, pinned, bare.
	wantSorted := []router.Alias{want[1], want[0], want[2], want[3], want[4]}
	if !reflect.DeepEqual(got, wantSorted) {
		t.Errorf("AliasList() =\n  %+v\nwant\n  %+v", got, wantSorted)
	}
}

func TestAliasListOfAnEmptyRegistryIsEmpty(t *testing.T) {
	if got := router.NewRegistry().AliasList(); len(got) != 0 {
		t.Errorf("AliasList() = %+v, want empty", got)
	}
}

func TestSetPickerRoutesDropsUnusableEntries(t *testing.T) {
	reg := router.NewRegistry()
	reg.SetPickerRoutes(map[string]router.PickerRoute{
		"  Anthropic-Compat.Sol  ": {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
		"":                         {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
		"   ":                      {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
		"bogus-backend":            {Backend: "elsewhere", UpstreamModel: "x"},
	})

	if got, want := reg.PickerIDs(), []string{"anthropic-compat.sol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("PickerIDs() = %v, want %v", got, want)
	}
	route, ok := reg.PickerRoute("anthropic-compat.sol")
	if !ok {
		t.Fatal("the trimmed, lowercased id should resolve")
	}
	if route.Backend != router.BackendCodex || route.UpstreamModel != "gpt-5.6-sol" {
		t.Errorf("route = %+v", route)
	}
}

func TestSetPickerRoutesReplacesRatherThanMerges(t *testing.T) {
	reg := router.NewRegistry()
	reg.SetPickerRoutes(map[string]router.PickerRoute{
		"a": {Backend: router.BackendCodex, UpstreamModel: "gpt-5.5"},
	})
	reg.SetPickerRoutes(map[string]router.PickerRoute{
		"b": {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
	})
	if _, ok := reg.PickerRoute("a"); ok {
		t.Error("a route from the previous generation is still registered")
	}
	if _, ok := reg.PickerRoute("b"); !ok {
		t.Error("the new route is missing")
	}

	reg.SetPickerRoutes(nil)
	if got := reg.PickerIDs(); len(got) != 0 {
		t.Errorf("PickerIDs() = %v, want none after a nil reset", got)
	}
}

// LoadCatalog and SetPickerRoutes have different lifetimes — a catalog refresh
// on a timer versus a picker open — so neither may clear the other.
func TestLoadCatalogLeavesPickerRoutesAlone(t *testing.T) {
	reg := router.NewRegistry()
	reg.SetPickerRoutes(map[string]router.PickerRoute{
		"anthropic-compat.sol": {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol"},
	})
	reg.LoadCatalog([]router.CatalogEntry{{Slug: "gpt-5.6-sol"}})

	if _, ok := reg.PickerRoute("anthropic-compat.sol"); !ok {
		t.Error("LoadCatalog cleared the picker tier")
	}
}

func TestPickerRoutesTakePrecedenceInResolve(t *testing.T) {
	reg := router.DefaultRegistry
	t.Cleanup(func() { *reg = *router.NewStaticRegistry() })

	reg.SetPickerRoutes(map[string]router.PickerRoute{
		// An id that "looks Anthropic" but was advertised as a Codex row.
		"claude-compat.sol": {Backend: router.BackendCodex, UpstreamModel: "gpt-5.6-sol", Effort: "high"},
		// A decorated Anthropic id standing for an undecorated model.
		"claude-sonnet-5[1m]": {Backend: router.BackendAnthropic, UpstreamModel: "claude-sonnet-5"},
	})

	dec, err := router.Resolve("Claude-Compat.Sol", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Backend != router.BackendCodex {
		t.Errorf("Backend = %q, want codex — the picker tier is authoritative", dec.Backend)
	}
	if dec.UpstreamModel != "gpt-5.6-sol" || dec.Effort != "high" {
		t.Errorf("decision = %+v", dec)
	}
	if dec.EffortSource != router.EffortSourceSuffix {
		t.Errorf("EffortSource = %q, want %q", dec.EffortSource, router.EffortSourceSuffix)
	}
	if dec.ClientModel != "Claude-Compat.Sol" {
		t.Errorf("ClientModel = %q, want the caller's exact spelling", dec.ClientModel)
	}

	dec, err = router.Resolve("claude-sonnet-5[1m]", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Backend != router.BackendAnthropic {
		t.Errorf("Backend = %q, want anthropic", dec.Backend)
	}
	if dec.ClientModel != "claude-sonnet-5" {
		t.Errorf("ClientModel = %q, want the undecorated id the route names", dec.ClientModel)
	}
	if dec.UpstreamModel != "" {
		t.Errorf("UpstreamModel = %q, want empty for the anthropic backend", dec.UpstreamModel)
	}
}

// Without any registration the grammar still decides, unchanged.
func TestUnregisteredIDsFallThroughToTheGrammar(t *testing.T) {
	reg := router.DefaultRegistry
	t.Cleanup(func() { *reg = *router.NewStaticRegistry() })
	reg.SetPickerRoutes(nil)

	dec, err := router.Resolve("anthropic-compat.sol", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if dec.Backend != router.BackendCodex || dec.UpstreamModel != "gpt-5.6-sol" {
		t.Errorf("decision = %+v", dec)
	}
}
