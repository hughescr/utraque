// Package providerreport assembles live provider quota observations and local
// ccusage history into a loopback-only, cache-safe reporting document.
//
// One collection serves two documents. Report is schema 1: its types are the
// schema-1 wire shape, it is what the collector builds and the cache holds,
// and it is frozen (deprecated: remove once clients migrate). ReportV2
// (v2.go) is schema 2, projected from the cached Report and the Handler's
// cross-attempt memory at serve time.
package providerreport

import (
	"context"
	"time"

	"github.com/hughescr/utraque/internal/codex/auth"
	"github.com/hughescr/utraque/internal/leg"
	"github.com/hughescr/utraque/internal/providerquota"
	"github.com/hughescr/utraque/internal/referenceprice"
	"github.com/hughescr/utraque/internal/usagehistory"
)

// SchemaVersionV1 and SchemaVersionV2 are the schema_version values of the
// two documents Handler serves.
const (
	SchemaVersionV1 = 1
	SchemaVersionV2 = 2
)

// Status is a provider's outcome for one collection attempt: ok when the
// quota and history sections are both present and Errors is empty, partial
// when at least one of the two is present, error otherwise (report.go's
// buildProvider; handler.go's markCodexUnavailable may later downgrade ok to
// partial or error).
type Status string

const (
	StatusOK      Status = "ok"
	StatusPartial Status = "partial"
	StatusError   Status = "error"
)

// Section names the part of a ProviderReport that a ReportError is about.
type Section string

const (
	// SectionQuota is the Section value for the live quota reading as the
	// collector records it and schema 1 serves it: the "quota_after"
	// spelling is a holdover from the discontinued before/after bracket
	// measurement. The Go field it reports on is ProviderReport.Quota. The
	// schema-2 renderer rewrites it to SectionQuotaV2.
	//
	// deprecated: remove with schema 1 (making SectionQuotaV2's value the
	// collector's own), once clients migrate.
	SectionQuota Section = "quota_after"
	// SectionQuotaV2 is the schema-2 spelling of SectionQuota.
	SectionQuotaV2 Section = "quota"
	// SectionHistory is the Section value for local ccusage history.
	SectionHistory Section = "history"
	// SectionReferencePrices is the Section value for the public models.dev
	// price snapshot.
	SectionReferencePrices Section = "reference_prices"
)

// ErrorCode classifies a ReportError. It is an extensible union: the codes a
// section reports are the codes its source package defines
// (providerquota.ErrorCode for the quota section, usagehistory.ErrorKind for
// history, referenceprice.ErrorCode for reference prices), converted by the
// ErrorCodeFrom* functions with their values unchanged, plus the codes this
// package raises itself, declared below.
type ErrorCode string

const (
	// CodeUnavailable is the fallback for an error no section classifies.
	// Its value coincides with providerquota.CodeUnavailable and
	// referenceprice.CodeUnavailable.
	CodeUnavailable ErrorCode = "unavailable"
	// CodeCredentialUnavailable reports errCredentialUnavailable: a quota
	// reader that was not given a credential. Its value coincides with
	// providerquota.CodeCredential.
	CodeCredentialUnavailable ErrorCode = "credential_unavailable"
	// CodeConfigurationError reports errNotConfigured: a quota reader that
	// is not configured at all. Its value coincides with
	// providerquota.CodeConfiguration and referenceprice.CodeConfiguration.
	CodeConfigurationError ErrorCode = "configuration_error"
	// CodeAccountScopeChanged and CodeAccountScopeUnverified are raised by
	// the handler when the Codex credential's account scope changed, or
	// could not be re-read, between collection start and end.
	CodeAccountScopeChanged    ErrorCode = "account_scope_changed"
	CodeAccountScopeUnverified ErrorCode = "account_scope_unverified"
)

// ErrorCodeFromQuota carries a provider quota reader's code into the report
// unchanged.
func ErrorCodeFromQuota(code providerquota.ErrorCode) ErrorCode { return ErrorCode(code) }

// ErrorCodeFromHistory carries a local usage-history collection kind into the
// report unchanged.
func ErrorCodeFromHistory(kind usagehistory.ErrorKind) ErrorCode { return ErrorCode(kind) }

// ErrorCodeFromPrice carries a reference-price reader's code into the report
// unchanged.
func ErrorCodeFromPrice(code referenceprice.ErrorCode) ErrorCode { return ErrorCode(code) }

// UnavailableReason says why a Calibration or RemainingEstimate carries no
// figure.
type UnavailableReason string

const (
	// ReasonPairedMeasurementUnavailable is recorded on every fresh Anthropic
	// Calibration: the routine report never brackets local-history collection
	// with a second live quota read.
	ReasonPairedMeasurementUnavailable UnavailableReason = "paired_quota_measurement_unavailable"
	// ReasonCachedMeasurementExpired replaces a calibration when the report
	// it belongs to is served stale, or when it is a retained
	// last_complete_snapshot.
	ReasonCachedMeasurementExpired UnavailableReason = "cached_measurement_expired"
	// ReasonQuotaWindowReset replaces a calibration when an active quota
	// window's reset time has passed since the reading was taken.
	ReasonQuotaWindowReset UnavailableReason = "quota_window_reset_after_collection"
	// ReasonUnsupportedBalanceCurrency, ReasonHistoricalRateUnavailable and
	// ReasonInvalidBalanceDecimal are the RemainingEstimate reasons: the
	// balance is not in USD, no fully priced historical rate exists for the
	// model, or the balance string is not a finite decimal.
	ReasonUnsupportedBalanceCurrency UnavailableReason = "unsupported_balance_currency"
	ReasonHistoricalRateUnavailable  UnavailableReason = "historical_effective_rate_unavailable"
	ReasonInvalidBalanceDecimal      UnavailableReason = "invalid_balance_decimal"
)

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

// Report is the schema-1 document and the collector's output. Its JSON form
// is frozen: the wire tests pin its bytes. The schema-2 document is a
// projection of it (renderV2).
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
	// Provider is the leg this section reports on.
	Provider leg.ID `json:"provider"`
	Status   Status `json:"status"`
	// LastAttempt is normally this collection's end time (report.go's
	// buildProvider receives it as ended). The one exception: when the quota
	// leg's error carries a non-zero AttemptedAt, LastAttempt is overridden
	// to that value (report.go's buildProvider) so it reflects the most
	// recent real upstream attempt rather than this collection's end time.
	// That AttemptedAt may come from the current live 429 (httpSettings.finish
	// stamps it with the request just made) or from a prior attempt carried
	// through cooldown suppression (a read denied locally without going
	// upstream is stamped with the earlier attempt that started the
	// cooldown) — both cases are covered, not only the suppressed-read one.
	// The same timestamp is also carried on the quota ReportError as
	// AttemptedAt, so a reader need not infer which meaning applies. This is
	// the schema-1 value; schema 2 serves the Handler's cross-attempt memory
	// instead (providerMemory).
	LastAttempt time.Time `json:"last_attempt"`
	// LastSuccess means "at least one section (quota or history) succeeded in
	// this collection attempt" — it is set whenever Status is StatusOK or
	// StatusPartial, not only on a fully-successful attempt (report.go's
	// buildProvider). Contrast ProviderSnapshot.LastSuccess below, which is
	// a stricter predicate. This is the schema-1, per-attempt value; schema
	// 2 serves the Handler's cross-attempt memory instead (providerMemory).
	LastSuccess     *time.Time    `json:"last_success,omitempty"`
	SourceFreshness Freshness     `json:"source_freshness"`
	Errors          []ReportError `json:"errors"`
	// QuotaBefore and Paired are never assigned by the current collector —
	// buildProvider only ever sets Quota, and markCodexUnavailable
	// (handler.go) explicitly nils all three. They exist only so the
	// schema-1 JSON keeps its quota_before and paired_measurement keys;
	// schema 2 has neither. See Quota.
	//
	// deprecated: remove with schema 1, once clients migrate.
	QuotaBefore *providerquota.Observation `json:"quota_before,omitempty"`
	// Quota is the only one of the three quota fields the current collector
	// assigns (report.go's buildProvider). Its schema-1 JSON key
	// "quota_after" is a holdover from a discontinued before/after bracket
	// measurement; schema 2 serves it as "quota". The matching
	// ReportError.Section value is SectionQuota (SectionQuotaV2 at schema 2).
	Quota *providerquota.Observation `json:"quota_after,omitempty"`
	// Paired: see QuotaBefore.
	//
	// deprecated: remove with schema 1, once clients migrate.
	Paired          *PairedMeasurement  `json:"paired_measurement,omitempty"`
	History         *HistorySummary     `json:"history,omitempty"`
	ReferencePrices *PriceSnapshot      `json:"reference_prices,omitempty"`
	Calibration     *Calibration        `json:"calibration,omitempty"`
	Remaining       []RemainingEstimate `json:"conditional_remaining_token_estimates,omitempty"`
	ConfiguredPlan  *ConfiguredPlan     `json:"configured_plan,omitempty"`
	LastComplete    *ProviderSnapshot   `json:"last_complete_snapshot,omitempty"`
	discardPrevious bool
}

// ProviderSnapshot preserves a previously successful, internally coherent
// measurement when the latest attempt is partial or failed.
type ProviderSnapshot struct {
	// LastSuccess here means "the complete measurement succeeded": a
	// ProviderSnapshot is only newly built from an attempt whose top-level
	// Status was StatusOK (handler.go's storeAttempt), so this field
	// (copied from that attempt's ProviderReport.LastSuccess) records when
	// every section succeeded together, not merely one of them. Contrast
	// ProviderReport.LastSuccess above, which is the looser "any section"
	// predicate.
	LastSuccess *time.Time `json:"last_success,omitempty"`
	Freshness   Freshness  `json:"source_freshness"`
	// QuotaBefore and Paired are copied straight from the source
	// ProviderReport and are likewise never non-nil in practice; see
	// ProviderReport.QuotaBefore.
	//
	// deprecated: remove with schema 1, once clients migrate.
	QuotaBefore *providerquota.Observation `json:"quota_before,omitempty"`
	Quota       *providerquota.Observation `json:"quota_after,omitempty"`
	// Paired: see QuotaBefore.
	//
	// deprecated: remove with schema 1, once clients migrate.
	Paired      *PairedMeasurement  `json:"paired_measurement,omitempty"`
	History     *HistorySummary     `json:"history,omitempty"`
	Calibration *Calibration        `json:"calibration,omitempty"`
	Remaining   []RemainingEstimate `json:"conditional_remaining_token_estimates,omitempty"`
}

type Freshness struct {
	Cached     bool    `json:"cached"`
	Stale      bool    `json:"stale"`
	AgeSeconds float64 `json:"age_seconds"`
}

type ReportError struct {
	Section   Section    `json:"section"`
	Code      ErrorCode  `json:"code"`
	Retryable bool       `json:"retryable"`
	RetryAt   *time.Time `json:"retry_at,omitempty"`
	// AttemptedAt is the quota leg's most recent real upstream attempt,
	// copied from providerquota.Error.AttemptedAt when that is set (a live
	// 429, or a cooldown-suppressed read stamped with the attempt that
	// started the cooldown). It is present only on the quota section's
	// error and is the same instant that overrides ProviderReport.LastAttempt.
	AttemptedAt *time.Time `json:"attempted_at,omitempty"`
	Message     string     `json:"message"`
}

// PriceSnapshot is the report's view of a referenceprice.Snapshot: the same
// source, unit and assumptions, restricted to one provider's rows and with
// each row's routing eligibility attached. Field order matches the source
// snapshot so the JSON is unchanged.
type PriceSnapshot struct {
	// Source is the catalog the prices came from ("models.dev"). Schema 2
	// serves it as "catalog".
	Source      string     `json:"source"`
	ObservedAt  time.Time  `json:"observed_at"`
	Stale       bool       `json:"stale,omitempty"`
	Unit        string     `json:"unit"`
	Models      []PriceRow `json:"models"`
	Assumptions []string   `json:"assumptions,omitempty"`
}

// PriceRow is one reference price plus Eligible, which the report derives by
// joining the price against the provider's current routing candidates (see
// Options.EligiblePriceModels). The source package never knows eligibility.
// The embedded ModelPrice's keys are inlined first, so "eligible" follows
// "cache_write" exactly as before the split.
type PriceRow struct {
	referenceprice.ModelPrice
	Eligible bool `json:"eligible"`
}

// PairedMeasurement is the schema-1 shape of a before/after quota bracket
// the collector no longer performs; nothing constructs it.
//
// deprecated: remove with schema 1, once clients migrate.
type PairedMeasurement struct {
	StartedAt time.Time                 `json:"started_at"`
	EndedAt   time.Time                 `json:"ended_at"`
	Before    providerquota.Observation `json:"before"`
	After     providerquota.Observation `json:"after"`
}

type HistorySummary struct {
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	// Source is the collector that produced the history ("ccusage"). Schema
	// 2 serves it as "collector".
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
	// Source is the ccusage agent label of the local log the rows came from
	// ("claude", "codex", "opencode"), never a provider. Schema 2 serves it
	// as "log_source".
	Source                      string                  `json:"source"`
	Model                       string                  `json:"model"`
	Provider                    leg.ID                  `json:"provider"`
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
	UnavailableReason    UnavailableReason          `json:"unavailable_reason,omitempty"`
}

type RemainingEstimate struct {
	Model    string `json:"model"`
	Currency string `json:"currency"`
	// Balance is the funds-on-hand figure the estimate divides
	// (providerquota.Balance.Total). Schema 2 serves it as "remaining", the
	// same key the schema-2 balances[] row uses.
	Balance            string            `json:"balance"`
	Tokens             *float64          `json:"tokens,omitempty"`
	HistoricalUSDToken *float64          `json:"historical_effective_usd_per_token,omitempty"`
	Assumptions        []string          `json:"assumptions,omitempty"`
	UnavailableReason  UnavailableReason `json:"unavailable_reason,omitempty"`
}

type ConfiguredPlan struct {
	Label      string   `json:"label,omitempty"`
	Multiplier *float64 `json:"multiplier,omitempty"`
	// Source says where the plan came from: always "configured" (the
	// operator's UTRAQUE_CLAUDE_PLAN settings), never a provider. Schema 2
	// serves it as "provenance".
	Source string `json:"source"`
}
