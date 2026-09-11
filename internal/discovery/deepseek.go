package discovery

import "github.com/hughescr/utraque/internal/router"

// DeepSeek picker ids must contain "anthropic" to survive Claude Code's
// discovery filter. Router.Resolve understands the same namespace without any
// in-memory registration, so a selection remains routable after a restart.
var deepSeekPickerModels = []struct {
	model Model
	route router.PickerRoute
}{
	{
		model: Model{
			ID:          "anthropic-compat.deepseek-flash",
			DisplayName: "DeepSeek V4.1 Flash",
			Type:        modelType,
		},
		route: router.PickerRoute{
			Backend:       router.BackendDeepSeek,
			UpstreamModel: "deepseek-flash",
		},
	},
	{
		model: Model{
			ID:          "anthropic-compat.deepseek-v4-pro",
			DisplayName: "DeepSeek V4 Pro 0813",
			Type:        modelType,
		},
		route: router.PickerRoute{
			Backend:       router.BackendDeepSeek,
			UpstreamModel: "deepseek-v4-pro",
		},
	},
}
