// Package models is the one home for the DeepSeek model catalog: the
// upstream ids utraque accepts on its DeepSeek leg, the retired aliases it
// canonicalises before a request leaves, the picker display names and the
// per-model capabilities the request rewriter gates on. The router, the
// DeepSeek leg, discovery and the provider report all derive their DeepSeek
// vocabulary from it instead of each keeping a literal table.
//
// The catalog is intentionally exact and closed. DeepSeek's
// Anthropic-compatible API silently maps unknown model names to
// deepseek-flash, which would make a typo spend money on a different model,
// so utraque accepts only the names listed here.
//
// Dependency contract: models imports only the standard library; in
// particular it must never import internal/deepseek or internal/router,
// because router imports this package and internal/deepseek imports router.
package models

// Model describes one DeepSeek model utraque can route to.
type Model struct {
	// ID is the canonical upstream model id, the value sent in the request
	// body and expected back in the response's model field.
	ID string
	// Aliases are retired upstream spellings that canonicalise to ID. They
	// are accepted from callers and from upstream responses but never sent.
	Aliases []string
	// DisplayName is the human-readable name the picker row shows.
	DisplayName string
	// SupportsImages reports whether the model accepts image content blocks.
	// The request rewriter refuses images for a model that does not.
	SupportsImages bool
}

// catalog is the closed list, in the order the picker and the 404 body list
// them.
var catalog = []Model{
	{
		ID:             "deepseek-flash",
		Aliases:        []string{"deepseek-v4-flash", "deepseek-v4-flash-vision-exp"},
		DisplayName:    "DeepSeek V4.1 Flash",
		SupportsImages: true,
	},
	{
		ID:             "deepseek-v4-pro",
		DisplayName:    "DeepSeek V4 Pro 0813",
		SupportsImages: false,
	},
}

// Models returns the catalog in listing order. The result is a copy, so a
// caller may append to or reorder it without disturbing the catalog.
func Models() []Model {
	out := make([]Model, len(catalog))
	for i, m := range catalog {
		out[i] = m
		out[i].Aliases = append([]string(nil), m.Aliases...)
	}
	return out
}

// Lookup finds the model whose ID or one of whose Aliases is exactly name.
// Matching is byte-exact: the router lowercases caller input before asking,
// and an upstream response names the model in its canonical spelling.
func Lookup(name string) (Model, bool) {
	for _, m := range catalog {
		if m.ID == name {
			return m, true
		}
		for _, alias := range m.Aliases {
			if alias == name {
				return m, true
			}
		}
	}
	return Model{}, false
}

// Canonical maps a model id or retired alias to the canonical upstream id.
// ok is false for a name the catalog does not list, which callers treat as
// "not a DeepSeek model" rather than forwarding it.
func Canonical(name string) (id string, ok bool) {
	m, ok := Lookup(name)
	if !ok {
		return "", false
	}
	return m.ID, true
}
