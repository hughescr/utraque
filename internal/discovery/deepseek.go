package discovery

import (
	"github.com/hughescr/utraque/internal/deepseek/models"
	"github.com/hughescr/utraque/internal/router"
)

// deepSeekPickerPrefix namespaces the DeepSeek picker ids. They must contain
// "anthropic" to survive Claude Code's discovery filter, and Router.Resolve
// understands the same namespace without any in-memory registration, so a
// selection remains routable after a restart.
const deepSeekPickerPrefix = "anthropic-compat."

// deepSeekPickerRow pairs a picker row with the route it resolves to.
type deepSeekPickerRow struct {
	model PickerRow
	route router.PickerRoute
}

// deepSeekPickerModels derives the DeepSeek picker rows from the model
// catalog, in catalog order: one row per canonical id, never one per alias.
func deepSeekPickerModels() []deepSeekPickerRow {
	catalog := models.Models()
	rows := make([]deepSeekPickerRow, 0, len(catalog))
	for _, m := range catalog {
		rows = append(rows, deepSeekPickerRow{
			model: PickerRow{
				ID:          deepSeekPickerPrefix + m.ID,
				DisplayName: m.DisplayName,
				Type:        modelType,
			},
			route: router.PickerRoute{
				Backend:       router.BackendDeepSeek,
				UpstreamModel: m.ID,
			},
		})
	}
	return rows
}
