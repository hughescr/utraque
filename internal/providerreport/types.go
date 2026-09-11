// Package providerreport assembles live provider quota observations and local
// ccusage history into an authenticated, cache-safe reporting document.
package providerreport

import (
	"context"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/usagehistory"
)

const SchemaVersion = 1

type HistoryCollector interface {
	Collect(context.Context, time.Time, time.Time) (usagehistory.Report, error)
}

type AnthropicReader interface {
	Read(context.Context, string) (providerquota.Observation, error)
}

type DeepSeekReader interface {
	Read(context.Context) (providerquota.Observation, error)
}

type CodexReader interface {
	ReadCredential(context.Context, auth.CredentialSource, auth.Credential) (providerquota.Observation, error)
}

type Report struct {
	SchemaVersion       int              `json:"schema_version"`
	GeneratedAt         time.Time        `json:"generated_at"`
	CollectionStartedAt time.Time        `json:"collection_started_at"`
	CollectionEndedAt   time.Time        `json:"collection_ended_at"`
	HistoryRange        DateRange        `json:"history_range"`
	Providers           []ProviderReport `json:"providers"`
	UnattributedHistory []ModelStats     `json:"unattributed_history,omitempty"`
}

type DateRange struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

type ProviderReport struct {
	Provider        string                     `json:"provider"`
	Status          string                     `json:"status"`
	LastAttempt     time.Time                  `json:"last_attempt"`
	LastSuccess     *time.Time                 `json:"last_success,omitempty"`
	SourceFreshness Freshness                  `json:"source_freshness"`
	Errors          []ReportError              `json:"errors"`
	QuotaBefore     *providerquota.Observation `json:"quota_before,omitempty"`
	QuotaAfter      *providerquota.Observation `json:"quota_after,omitempty"`
	Paired          *PairedMeasurement         `json:"paired_measurement,omitempty"`
	History         *HistorySummary            `json:"history,omitempty"`
	Calibration     *Calibration               `json:"calibration,omitempty"`
	Remaining       []RemainingEstimate        `json:"conditional_remaining_token_estimates,omitempty"`
	ConfiguredPlan  *ConfiguredPlan            `json:"configured_plan,omitempty"`
	LastComplete    *ProviderSnapshot          `json:"last_complete_snapshot,omitempty"`
}

// ProviderSnapshot preserves a previously successful, internally coherent
// measurement when the latest attempt is partial or failed.
type ProviderSnapshot struct {
	LastSuccess *time.Time                 `json:"last_success,omitempty"`
	Freshness   Freshness                  `json:"source_freshness"`
	QuotaBefore *providerquota.Observation `json:"quota_before,omitempty"`
	QuotaAfter  *providerquota.Observation `json:"quota_after,omitempty"`
	Paired      *PairedMeasurement         `json:"paired_measurement,omitempty"`
	History     *HistorySummary            `json:"history,omitempty"`
	Calibration *Calibration               `json:"calibration,omitempty"`
	Remaining   []RemainingEstimate        `json:"conditional_remaining_token_estimates,omitempty"`
}

type Freshness struct {
	Cached     bool    `json:"cached"`
	Stale      bool    `json:"stale"`
	AgeSeconds float64 `json:"age_seconds"`
}

type ReportError struct {
	Section   string `json:"section"`
	Code      string `json:"code"`
	Retryable bool   `json:"retryable"`
	Message   string `json:"message"`
}

type PairedMeasurement struct {
	StartedAt time.Time                 `json:"started_at"`
	EndedAt   time.Time                 `json:"ended_at"`
	Before    providerquota.Observation `json:"before"`
	After     providerquota.Observation `json:"after"`
}

type HistorySummary struct {
	StartedAt                  time.Time                   `json:"started_at"`
	FinishedAt                 time.Time                   `json:"finished_at"`
	Source                     string                      `json:"source"`
	Coverage                   string                      `json:"coverage"`
	CostBasis                  string                      `json:"cost_basis"`
	ToolVersion                string                      `json:"tool_version,omitempty"`
	InvocationMode             usagehistory.InvocationMode `json:"invocation_mode"`
	UnitPricesAvailable        bool                        `json:"unit_prices_available"`
	UnitPriceUnavailableReason string                      `json:"unit_price_unavailable_reason"`
	SevenDays                  PeriodSummary               `json:"seven_days"`
	ThirtyDays                 PeriodSummary               `json:"thirty_days"`
	Blocks                     []usagehistory.BlockSummary `json:"blocks"`
	Issues                     []usagehistory.Issue        `json:"issues"`
}

type PeriodSummary struct {
	Since  time.Time    `json:"since"`
	Until  time.Time    `json:"until"`
	Models []ModelStats `json:"models"`
}

type ModelStats struct {
	Source                      string                `json:"source"`
	Model                       string                `json:"model"`
	Provider                    usagehistory.Provider `json:"provider"`
	InputTokens                 uint64                `json:"input_tokens"`
	OutputTokens                uint64                `json:"output_tokens"`
	CacheCreationTokens         uint64                `json:"cache_creation_tokens"`
	CacheReadTokens             uint64                `json:"cache_read_tokens"`
	TotalTokens                 uint64                `json:"total_tokens"`
	CostUSD                     *float64              `json:"cost_usd"`
	CostStatus                  string                `json:"cost_status"`
	HistoricalEffectiveUSDToken *float64              `json:"historical_effective_usd_per_token"`
	UnitPriceUnavailableReason  string                `json:"unit_price_unavailable_reason,omitempty"`
}

type Calibration struct {
	QuotaID              string                     `json:"quota_id,omitempty"`
	Block                *usagehistory.BlockSummary `json:"block,omitempty"`
	UsedPercent          *float64                   `json:"used_percent,omitempty"`
	ObservedTokens       *uint64                    `json:"observed_tokens,omitempty"`
	ConditionalRemaining *float64                   `json:"conditional_remaining_tokens,omitempty"`
	Model                string                     `json:"model,omitempty"`
	Assumptions          []string                   `json:"assumptions,omitempty"`
	UnavailableReason    string                     `json:"unavailable_reason,omitempty"`
}

type RemainingEstimate struct {
	Model              string   `json:"model"`
	Currency           string   `json:"currency"`
	Balance            string   `json:"balance"`
	Tokens             *float64 `json:"tokens,omitempty"`
	HistoricalUSDToken *float64 `json:"historical_effective_usd_per_token,omitempty"`
	Assumptions        []string `json:"assumptions,omitempty"`
	UnavailableReason  string   `json:"unavailable_reason,omitempty"`
}

type ConfiguredPlan struct {
	Label      string   `json:"label,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	Source     string   `json:"source"`
}
