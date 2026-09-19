// Package providerreport assembles live provider quota observations and local
// ccusage history into a loopback-only, cache-safe reporting document.
package providerreport

import (
	"context"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

const SchemaVersion = 1

// SectionQuota is the ReportError.Section value for the live quota reading.
// Its value is the schema-v1 "quota_after" spelling, a holdover from the
// discontinued before/after bracket measurement; the Go field it reports on
// is ProviderReport.Quota. Keep the value until schema v2.
const SectionQuota = "quota_after"

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

type ReferencePriceReader interface {
	Read(context.Context) (referenceprice.Snapshot, error)
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
	Provider string `json:"provider"`
	Status   string `json:"status"`
	// LastAttempt is normally this collection's end time (report.go's
	// buildProvider receives it as ended). The one exception: when the quota
	// leg's error carries a non-zero AttemptedAt, LastAttempt is overridden
	// to that value (report.go:114-118) so it reflects the most recent real
	// upstream attempt rather than this collection's end time. That
	// AttemptedAt may come from the current live 429 (httpSettings.finish
	// stamps it with the request just made) or from a prior attempt carried
	// through cooldown suppression (a read denied locally without going
	// upstream is stamped with the earlier attempt that started the
	// cooldown) — both cases are covered, not only the suppressed-read one.
	LastAttempt time.Time `json:"last_attempt"`
	// LastSuccess means "at least one section (quota or history) succeeded in
	// this collection attempt" — it is set whenever Status is "ok" or
	// "partial", not only on a fully-successful attempt (report.go:148-161).
	// Contrast ProviderSnapshot.LastSuccess below, which is a stricter
	// predicate.
	LastSuccess     *time.Time    `json:"last_success,omitempty"`
	SourceFreshness Freshness     `json:"source_freshness"`
	Errors          []ReportError `json:"errors"`
	// QuotaBefore and Paired are never assigned by the current collector —
	// buildProvider only ever sets Quota (report.go:121), and
	// markCodexUnavailable (handler.go:186) explicitly nils all three. Their
	// JSON names are kept as-is for schema v1 compatibility; see Quota.
	QuotaBefore *providerquota.Observation `json:"quota_before,omitempty"`
	// Quota is the only one of the three quota fields the current collector
	// assigns (report.go:121). Its JSON key "quota_after" is a holdover from
	// a discontinued before/after bracket measurement and is kept for schema
	// v1 compatibility; the matching ReportError.Section value is
	// SectionQuota.
	Quota           *providerquota.Observation `json:"quota_after,omitempty"`
	Paired          *PairedMeasurement         `json:"paired_measurement,omitempty"`
	History         *HistorySummary            `json:"history,omitempty"`
	ReferencePrices *referenceprice.Snapshot   `json:"reference_prices,omitempty"`
	Calibration     *Calibration               `json:"calibration,omitempty"`
	Remaining       []RemainingEstimate        `json:"conditional_remaining_token_estimates,omitempty"`
	ConfiguredPlan  *ConfiguredPlan            `json:"configured_plan,omitempty"`
	LastComplete    *ProviderSnapshot          `json:"last_complete_snapshot,omitempty"`
	discardPrevious bool
}

// ProviderSnapshot preserves a previously successful, internally coherent
// measurement when the latest attempt is partial or failed.
type ProviderSnapshot struct {
	// LastSuccess here means "the complete measurement succeeded": a
	// ProviderSnapshot is only newly built from an attempt whose top-level
	// Status was "ok" (handler.go's storeAttempt, ~250-266), so this field
	// (copied from that attempt's ProviderReport.LastSuccess) records when
	// every section succeeded together, not merely one of them. Contrast
	// ProviderReport.LastSuccess above, which is the looser "any section"
	// predicate.
	LastSuccess *time.Time `json:"last_success,omitempty"`
	Freshness   Freshness  `json:"source_freshness"`
	// QuotaBefore and Paired are copied straight from the source
	// ProviderReport and are likewise never non-nil in practice; see
	// ProviderReport.QuotaBefore.
	QuotaBefore *providerquota.Observation `json:"quota_before,omitempty"`
	Quota       *providerquota.Observation `json:"quota_after,omitempty"`
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
	Section   string     `json:"section"`
	Code      string     `json:"code"`
	Retryable bool       `json:"retryable"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
	Message   string     `json:"message"`
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
	Source                      string                  `json:"source"`
	Model                       string                  `json:"model"`
	Provider                    usagehistory.Provider   `json:"provider"`
	InputTokens                 uint64                  `json:"input_tokens"`
	OutputTokens                uint64                  `json:"output_tokens"`
	CacheCreationTokens         uint64                  `json:"cache_creation_tokens"`
	CacheReadTokens             uint64                  `json:"cache_read_tokens"`
	TotalTokens                 uint64                  `json:"total_tokens"`
	CostUSD                     *float64                `json:"cost_usd"`
	CostStatus                  usagehistory.CostStatus `json:"cost_status"`
	HistoricalEffectiveUSDToken *float64                `json:"historical_effective_usd_per_token"`
	UnitPriceUnavailableReason  string                  `json:"unit_price_unavailable_reason,omitempty"`
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
