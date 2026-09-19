// Package providerquota reads live subscription quota and account-balance
// snapshots from providers. It deliberately owns no credential discovery:
// callers supply the exact credential (or Codex CredentialSource) used by the
// corresponding inference leg.
package providerquota

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"
)

type Provider string

const (
	ProviderAnthropic Provider = "anthropic"
	ProviderCodex     Provider = "codex"
	ProviderDeepSeek  Provider = "deepseek"

	PercentUnit = "percent_0_100"
)

// Observation is a provider snapshot collected at one instant. cacheScope is
// intentionally unexported: it is available for account-safe cache matching,
// but cannot leak through encoding/json.
type Observation struct {
	// Source names the provider that produced this observation.
	Source Provider `json:"source"`
	// CollectedAt is when this snapshot was taken.
	CollectedAt time.Time `json:"collected_at"`
	// Quotas contains Anthropic and Codex entries: Anthropic's session/weekly
	// limits (anthropic.go), and Codex's primary/secondary windows plus its
	// derived "spend_control" entry (codex.go). DeepSeek never populates
	// Quotas.
	Quotas []Quota `json:"quotas,omitempty"`
	// Balances contains DeepSeek and Codex entries. Kind selects which of
	// several different accounting equations applies — see the Balance doc.
	// Anthropic never populates Balances.
	Balances []Balance `json:"balances,omitempty"`
	// SpendControls is populated only by Codex (codex.go:334), one entry per
	// bucket that reports the upstream spendControlReached flag. Nothing in
	// the tree reads it today: its former reader, providerreport's DeepSeek
	// remaining-value estimator, was removed because DeepSeek never populates
	// this field. It is emitted in the report JSON as-is.
	SpendControls []SpendControl `json:"spend_controls,omitempty"`
	// Plan is populated only by Codex, from the account's plan type
	// (codex.go:245).
	Plan *PlanInfo `json:"plan,omitempty"`
	// ExtraUsage is populated only by Anthropic, from its extra_usage block.
	ExtraUsage *ExtraUsage `json:"extra_usage,omitempty"`
	// ResetCredits is populated only by Codex, from rateLimitResetCredits
	// (codex.go:398).
	ResetCredits *ResetCredits `json:"reset_credits,omitempty"`
	// Available is populated only by DeepSeek, from its balance payload's
	// Available flag (deepseek.go:87). Do not confuse with Balance.Available,
	// which is a separate, Codex-only per-balance flag.
	Available  *bool `json:"available,omitempty"`
	cacheScope string
}

// CacheScope returns a non-secret, provider-scoped discriminator suitable for
// preventing observations from different credentials or accounts being paired.
func (o Observation) CacheScope() string { return o.cacheScope }

type PlanInfo struct {
	Type string `json:"type"`
}

type Quota struct {
	// ID's identity contract differs by provider. On Anthropic it is a
	// fully-qualified window id: Kind (always non-empty — checked at
	// anthropic.go:183), with a scope suffix appended when the window is
	// scoped (anthropic.go:190,210, anthropicScopeSuffix). On Codex it is a
	// bucket id (the upstream limitId, or the map key when absent) shared by
	// the primary and secondary window rows for the same bucket, which Slot
	// disambiguates
	// (codex.go:355); the derived spend-control entry reuses that bucket id
	// with a ":spend_control" suffix (codex.go:390).
	ID string `json:"id"`
	// Name is populated only by Codex, from the upstream limitName
	// (codex.go:357).
	Name string `json:"name,omitempty"`
	// Kind's value space differs by producer: on Anthropic's legacy window
	// path (no "limits" in the payload) it is set to the same fixed internal
	// id as ID, one of "five_hour", "seven_day", "seven_day_oauth_apps",
	// "seven_day_opus", "seven_day_sonnet", or "cinder_cove"
	// (anthropic.go:139-146,175); on Anthropic's newer per-limit path it is
	// the upstream limit kind verbatim, e.g. "weekly_all"/"weekly_scoped"/
	// "session" (anthropic.go:190) — the upstream value space is not
	// enumerated by this package, only "session" and "weekly*"/group
	// "weekly" are given special handling (anthropic.go:193-196); on Codex's
	// primary/secondary window rows it is left empty (codex.go:355); on
	// Codex's derived spend-control entry it is the utraque-invented literal
	// "spend_control" (codex.go:390).
	Kind string `json:"kind,omitempty"`
	// Group is populated only by Anthropic's per-limit path, from the
	// upstream group (anthropic.go:190).
	Group string `json:"group,omitempty"`
	// Slot is populated only by Codex, "primary" or "secondary", and is what
	// disambiguates the two window rows that share one bucket ID
	// (codex.go:355).
	Slot string `json:"slot,omitempty"`
	// UsedPercent is populated by every producer: Anthropic's legacy window
	// (anthropic.go:175) and per-limit (anthropic.go:190) paths, and Codex's
	// primary/secondary window rows (codex.go:355) and derived
	// spend-control entry (codex.go:390, computed as 100 minus the upstream
	// remaining percent).
	UsedPercent float64 `json:"used_percent"`
	// Unit is always PercentUnit ("percent_0_100"); every producer of a
	// Quota sets it that way (anthropic.go:175,190; codex.go:355,390).
	Unit string `json:"unit"`
	// DurationSeconds is populated by Anthropic's legacy window path from a
	// fixed table keyed by window id (anthropic.go:139-144) and by
	// Anthropic's per-limit path from a fixed table keyed by Kind/Group
	// (anthropic.go:192-197, left unset when neither matches); by Codex's
	// primary/secondary window rows from the upstream windowDurationMinutes
	// (codex.go:347,355). Left unset on Codex's derived spend-control entry.
	DurationSeconds *int64 `json:"duration_seconds,omitempty"`
	// ResetsAt is populated by every producer that has a reset time:
	// Anthropic's legacy window and per-limit paths (anthropic.go:171,186)
	// and Codex's primary/secondary window rows and derived spend-control
	// entry (codex.go:351,385-390), all from an upstream reset timestamp.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Scope is populated only by Anthropic's per-limit path, when the
	// upstream limit carries a model/surface scope (anthropic.go:203-210).
	Scope *Scope `json:"scope,omitempty"`
	// Active is populated only by Anthropic's per-limit path, from the
	// upstream isActive flag (anthropic.go:190).
	Active *bool `json:"active,omitempty"`
	// Plan is populated only by Codex, per window, from the upstream
	// planType (codex.go:360).
	Plan *PlanInfo `json:"plan,omitempty"`
	// ReachedType is populated only by Codex, from the upstream
	// rateLimitReachedType (codex.go:363).
	ReachedType string `json:"reached_type,omitempty"`
}

type Scope struct {
	Model   *ScopeLabel `json:"model,omitempty"`
	Surface *ScopeLabel `json:"surface,omitempty"`
}

type ScopeLabel struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// Balance's Total means a different accounting equation depending on Kind,
// because the three producers populate it from three different upstream
// shapes:
//
//   - DeepSeek, Kind "account_balance" (deepseek.go:93-101): Total is the sum
//     of Components (granted + topped_up) and is spendable — money on hand.
//   - Codex, Kind "workspace_credits" (codex.go:371-376): Total is spendable
//     with no Components — also money on hand, but never broken down.
//   - Codex, Kind "spend_control" (codex.go:391): Total is a ceiling (the
//     upstream spend limit), not money on hand, and the "used" Components
//     entry is consumption against that ceiling, not a component of Total.
//
// A caller that treats every Total as spendable funds (dividing by a price
// rate, for instance) is correct only for the first two kinds.
type Balance struct {
	// Kind selects the accounting equation for this row: "account_balance"
	// (DeepSeek, deepseek.go:93), "workspace_credits" (Codex,
	// codex.go:371), or "spend_control" (Codex, codex.go:391) — see the
	// type doc above.
	Kind string `json:"kind"`
	// LimitID is populated only by Codex, both for "workspace_credits" and
	// "spend_control" rows, from the upstream limitId — the same bucket id as
	// the paired Quota.ID (codex.go:371,391). DeepSeek never sets it. The
	// JSON key is still "scope_id" for schema v1 compatibility, although the
	// value is a limit bucket id and never refers to a Scope.
	LimitID string `json:"scope_id,omitempty"`
	// Currency is populated only by DeepSeek, from the upstream balance
	// entry's currency, "USD" or "CNY" (deepseek.go:89,94). Codex never sets
	// it (it reports in provider-defined units instead — see AmountUnit).
	Currency string `json:"currency,omitempty"`
	// AmountUnit is populated by every producer with a fixed literal, not an
	// upstream value: "currency" (DeepSeek, deepseek.go:95), "credits"
	// (Codex workspace_credits, codex.go:371), or "provider_units" (Codex
	// spend_control, codex.go:391).
	AmountUnit string `json:"amount_unit,omitempty"`
	// Total is populated by DeepSeek (deepseek.go:96, the upstream
	// total_balance), Codex workspace_credits (codex.go:373-376, the
	// upstream credits balance, optional — omitted when upstream omits it),
	// and Codex spend_control (codex.go:391, the upstream spend limit). See
	// the type doc above for what Total means under each Kind.
	Total string `json:"total,omitempty"`
	// Components is populated by DeepSeek, always exactly "granted" and
	// "topped_up" (deepseek.go:97-100), and by Codex spend_control, always
	// exactly one "used" entry (codex.go:391). Codex workspace_credits never
	// sets it.
	Components []BalanceComponent `json:"components,omitempty"`
	// Available is populated only by Codex workspace_credits, from the
	// upstream hasCredits flag (codex.go:371). Do not confuse with
	// Observation.Available, which is a separate, DeepSeek-only top-level
	// flag. Neither DeepSeek nor Codex spend_control sets this field.
	Available *bool `json:"available,omitempty"`
	// Unlimited is populated only by Codex workspace_credits, from the
	// upstream unlimited flag (codex.go:371). Neither DeepSeek nor Codex
	// spend_control sets this field.
	Unlimited *bool `json:"unlimited,omitempty"`
}

type BalanceComponent struct {
	Name   string `json:"name"`
	Amount string `json:"amount"`
}

// SpendControl preserves the backend's independent per-bucket restriction
// state. A missing backend flag produces no entry; Reached=false is retained.
// Populated only by Codex (codex.go:334, from spendControlReached); no Go code
// reads it back — see Observation.SpendControls.
type SpendControl struct {
	// LimitID is the same bucket id used for the paired Quota/Balance rows
	// for that bucket (codex.go:334). The JSON key is still "scope_id" for
	// schema v1 compatibility.
	LimitID string `json:"scope_id"`
	// Reached is the upstream spendControlReached flag verbatim (codex.go:334).
	Reached bool `json:"reached"`
}

// ExtraUsage retains only the documented, non-identifying fields returned by
// Anthropic. Amounts remain in the provider's original decimal representation.
// Every field is populated only by Anthropic's normalizeAnthropicExtra
// (anthropic.go:266-296); no other provider sets ExtraUsage at all.
type ExtraUsage struct {
	// Enabled is the upstream is_enabled flag verbatim (anthropic.go:270);
	// required, always set.
	Enabled bool `json:"enabled"`
	// MonthlyLimit is set when upstream's monthly_limit is present and
	// decimal (anthropic.go:274,276-283); AmountUnit is then also set to
	// "provider_units".
	MonthlyLimit *string `json:"monthly_limit,omitempty"`
	// UsedCredits is set when upstream's used_credits is present and decimal
	// (anthropic.go:274,276-283); AmountUnit is then also set to
	// "provider_units".
	UsedCredits *string `json:"used_credits,omitempty"`
	// AmountUnit is set to the fixed literal "provider_units" whenever
	// either MonthlyLimit or UsedCredits is set (anthropic.go:283); left
	// empty when neither upstream amount is present.
	AmountUnit string `json:"amount_unit,omitempty"`
	// UsedPercent is set when upstream's utilization is present
	// (anthropic.go:285-289); Unit is then also set to PercentUnit.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// Unit is set to the fixed literal PercentUnit ("percent_0_100") when
	// UsedPercent is set (anthropic.go:289); left empty otherwise.
	Unit string `json:"unit,omitempty"`
	// Currency is set when upstream's currency is present and valid
	// (anthropic.go:291-295).
	Currency string `json:"currency,omitempty"`
}

type ResetCredits struct {
	AvailableCount int64 `json:"available_count"`
}

type ErrorCode string

const (
	CodeConfiguration ErrorCode = "configuration_error"
	CodeCredential    ErrorCode = "credential_unavailable"
	CodeUnauthorized  ErrorCode = "unauthorized"
	CodeRateLimited   ErrorCode = "rate_limited"
	CodeUnavailable   ErrorCode = "unavailable"
	CodeInvalidData   ErrorCode = "invalid_response"
	CodeTooLarge      ErrorCode = "response_too_large"
	CodeTimeout       ErrorCode = "timeout"
	CodeProtocol      ErrorCode = "unsupported_protocol"
)

// Error contains classification only. Provider bodies, URLs, subprocess
// stderr, credentials, and account identifiers are never retained.
type Error struct {
	Provider  Provider
	Code      ErrorCode
	Retryable bool
	// RetryAt is populated for rate-limit responses and locally enforced
	// cooldowns. AttemptedAt records the most recent real upstream attempt, so a
	// suppressed read does not look like fresh network activity.
	RetryAt     *time.Time
	AttemptedAt time.Time
}

func (e *Error) Error() string {
	if e == nil {
		return "provider quota error"
	}
	return fmt.Sprintf("provider quota: %s: %s", e.Provider, e.Code)
}

func quotaError(provider Provider, code ErrorCode, retryable bool) error {
	return &Error{Provider: provider, Code: code, Retryable: retryable}
}

func rateLimitError(provider Provider, retryAt, attemptedAt time.Time) error {
	retry := retryAt.UTC()
	return &Error{Provider: provider, Code: CodeRateLimited, Retryable: true, RetryAt: &retry, AttemptedAt: attemptedAt.UTC()}
}

func scopeHash(provider Provider, secret string) string {
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + secret))
	return string(provider) + ":" + hex.EncodeToString(sum[:])
}

func credentialScope(provider Provider, secret string) (string, error) {
	if strings.TrimSpace(secret) == "" || len(secret) > 256<<10 {
		return "", quotaError(provider, CodeCredential, false)
	}
	return scopeHash(provider, secret), nil
}

var decimalPattern = regexp.MustCompile(`^(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)

func validDecimal(s string) bool {
	return len(s) > 0 && len(s) <= 128 && decimalPattern.MatchString(s)
}

func validCurrency(s string) bool {
	if len(s) != 3 {
		return false
	}
	for _, r := range s {
		if r < 'A' || r > 'Z' {
			return false
		}
	}
	return true
}

func validLabel(s string) bool {
	if len(strings.TrimSpace(s)) > 256 {
		return false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func validNonemptyLabel(s string) bool { return strings.TrimSpace(s) != "" && validLabel(s) }
