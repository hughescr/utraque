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

	"github.com/hughescr/utraque/internal/leg"
)

// PercentUnit is the Unit a percentage-valued Quota reports.
const PercentUnit = "percent_0_100"

// Observation is a provider snapshot collected at one instant. cacheScope is
// intentionally unexported: it is available for account-safe cache matching,
// but cannot leak through encoding/json.
type Observation struct {
	// Source names the leg whose account produced this observation.
	Source leg.ID `json:"source"`
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
	// SpendControls is populated only by Codex (codex.go:339), one entry per
	// bucket that reports the upstream spendControlReached flag. Nothing in
	// the tree reads it: it exists only so the schema-1 report JSON keeps its
	// spend_controls array. The same flag is carried by SpendLimits.Reached,
	// which is what schema 2 serves.
	//
	// deprecated: remove with schema 1, once clients migrate.
	SpendControls []SpendControl `json:"spend_controls,omitempty"`
	// SpendLimits is the single home for "a ceiling with consumption": one
	// entry per Codex bucket that reports individualLimit and/or
	// spendControlReached (codex.go:338-341,399-407), and one entry for
	// Anthropic's extra_usage block (anthropic.go:157-167). DeepSeek never
	// populates it. The schema-2 report serves it as quota.spend_limits; it
	// is kept off this type's own JSON form because that form is the schema-1
	// quota reading, which instead fans the same facts out into Quotas (Kind
	// QuotaKindSpendControl), Balances (Kind BalanceKindSpendControl),
	// SpendControls and ExtraUsage. Those must keep being populated until
	// schema 1 is retired.
	SpendLimits []SpendLimit `json:"-"`
	// Plan is populated only by Codex, from the account's plan type
	// (codex.go:245).
	Plan *PlanInfo `json:"plan,omitempty"`
	// ExtraUsage is populated only by Anthropic, from its extra_usage block.
	// The same facts are carried by SpendLimits, which is what schema 2
	// serves; this field exists only for the schema-1 report JSON.
	//
	// deprecated: remove with schema 1, once clients migrate.
	ExtraUsage *ExtraUsage `json:"extra_usage,omitempty"`
	// ResetCredits is populated only by Codex, from rateLimitResetCredits
	// (codex.go:414).
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

// QuotaKind is the vocabulary of Quota.Kind. It is open at the wire boundary:
// Anthropic rows carry the upstream-supplied limit kind verbatim (per-limit
// path) or the fixed legacy window id (legacy path), neither of which this
// package enumerates. The constants below are the values utraque itself
// mints; a QuotaKind that is none of them came from upstream. Codex's
// primary/secondary window rows leave Kind empty. The schema-2 report does
// not serve this value: it serves WindowKindOf's closed vocabulary as
// quotas[].kind, with the upstream value still readable as quotas[].bucket.
type QuotaKind string

const (
	// QuotaKindSpendControl marks the Codex entry derived from a bucket's
	// individualLimit (codex.go:397). It is a schema-1 fan-out of the
	// bucket's SpendLimit and is not served at schema 2.
	QuotaKindSpendControl QuotaKind = "spend_control"
)

// WindowKind is utraque's own closed vocabulary for what a Quota row
// measures, served as quotas[].kind at schema 2. Every provider's rows are
// mapped into it by WindowKindOf; the upstream value the mapping started
// from stays readable as Quota.Bucket, so the mapping loses nothing.
type WindowKind string

const (
	// WindowSession is the short rolling window: Anthropic's "session" limit
	// (legacy "five_hour") and Codex's primary window.
	WindowSession WindowKind = "session"
	// WindowWeekly is the long rolling window over all models: Anthropic's
	// "weekly_all" limit (legacy "seven_day") and Codex's secondary window.
	WindowWeekly WindowKind = "weekly"
	// WindowWeeklyScoped is a weekly window that applies to one model or
	// surface: Anthropic's "weekly_scoped" limit and the legacy
	// "seven_day_opus", "seven_day_sonnet" and "seven_day_oauth_apps"
	// windows. Codex reports no scoped windows.
	WindowWeeklyScoped WindowKind = "weekly_scoped"
	// WindowOther is every row the table above does not place: Anthropic's
	// legacy "cinder_cove" window, an upstream limit kind and group this
	// package has never seen, or a Codex row in neither slot.
	WindowOther WindowKind = "other"
)

// WindowKindOf places one provider's Quota row in the WindowKind vocabulary.
//
// Anthropic rows are placed by their upstream limit kind (Quota.Kind, or
// Quota.Bucket when a caller built the row without one) or, failing that,
// their upstream group: "session"/"five_hour" (group "session") is the
// session window; "weekly_all", "weekly_scoped", the legacy "seven_day*"
// ids and group "weekly" are weekly windows, which are weekly_scoped when
// the row carries a Scope or is one of the legacy per-model/per-surface ids
// ("seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps") and weekly
// otherwise. Codex rows are placed by Slot: the Codex payload names its
// windows only by position, "primary" being the short window and
// "secondary" the weekly one, so the mapping is positional rather than a
// claim about DurationSeconds, which the row still carries verbatim. Any
// other provider, and any row the tables do not place, is WindowOther.
func WindowKindOf(provider leg.ID, q Quota) WindowKind {
	switch provider {
	case leg.Anthropic:
		raw := string(q.Kind)
		if raw == "" {
			raw = q.Bucket
		}
		scoped := q.Scope != nil
		switch raw {
		case "session", "five_hour":
			return WindowSession
		case "weekly_all", "seven_day", "weekly_scoped":
		case "seven_day_opus", "seven_day_sonnet", "seven_day_oauth_apps":
			scoped = true
		default:
			switch q.Group {
			case "session":
				return WindowSession
			case "weekly":
			default:
				return WindowOther
			}
		}
		if scoped || raw == "weekly_scoped" {
			return WindowWeeklyScoped
		}
		return WindowWeekly
	case leg.Codex:
		switch q.Slot {
		case "primary":
			return WindowSession
		case "secondary":
			return WindowWeekly
		}
	}
	return WindowOther
}

// QuotaID composes the schema-2 quota identity, bucket[:slot][:scope]: the
// bucket, then ":"+slot when the provider splits a bucket into slots
// (Codex), then the scope suffix when the window is scoped (Anthropic). It is
// unique within one Observation for every provider. The schema-1 Quota.ID
// follows the same rule only on Anthropic; Codex's schema-1 rows carry the
// bare bucket and rely on Slot.
func QuotaID(bucket, slot string, scope *Scope) string {
	id := bucket
	if slot != "" {
		id += ":" + slot
	}
	return id + scopeSuffix(scope)
}

// scopeSuffix renders a Scope as the ":model=<id>:surface=<id>" tail QuotaID
// appends, each label by its id or, failing that, its display name.
func scopeSuffix(s *Scope) string {
	if s == nil {
		return ""
	}
	result := ""
	for _, item := range []struct {
		name  string
		label *ScopeLabel
	}{{"model", s.Model}, {"surface", s.Surface}} {
		if item.label == nil {
			continue
		}
		value := item.label.ID
		if value == "" {
			value = item.label.DisplayName
		}
		result += ":" + item.name + "=" + value
	}
	return result
}

type Quota struct {
	// ID is the schema-1 identity, and its contract differs by provider. On
	// Anthropic it is a fully-qualified window id: Kind (always non-empty —
	// checked at anthropic.go:187), with a scope suffix appended when the
	// window is scoped (anthropic.go:194,214, QuotaID). On Codex it is a
	// bucket id (the upstream limitId, or the map key when absent) shared by
	// the primary and secondary window rows for the same bucket, which Slot
	// disambiguates (codex.go:362); the derived spend-control entry reuses
	// that bucket id with a ":spend_control" suffix (codex.go:397). The
	// schema-2 report does not serve this value: it recomputes every row's
	// id with QuotaID from Bucket, Slot and Scope.
	ID string `json:"id"`
	// Bucket is the upstream identity the ID is derived from, without any
	// slot or scope decoration: on Codex the limitId (or the map key when
	// absent — codex.go:326-329,362,397), on Anthropic the wire limit kind
	// (anthropic.go:194) or the legacy window key (anthropic.go:179). The
	// schema-2 report serves it as quotas[].bucket and derives quotas[].id
	// from it; it is kept off this type's own JSON form because that form is
	// the schema-1 quota reading.
	Bucket string `json:"-"`
	// Name is populated only by Codex, from the upstream limitName
	// (codex.go:364).
	Name string `json:"name,omitempty"`
	// Kind's value space differs by producer — see QuotaKind. On Anthropic's
	// legacy window path (no "limits" in the payload) it is set to the same
	// fixed internal id as ID, one of "five_hour", "seven_day",
	// "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", or
	// "cinder_cove" (anthropic.go:139-146,179); on Anthropic's newer
	// per-limit path it is the upstream limit kind verbatim, e.g.
	// "weekly_all"/"weekly_scoped"/"session" (anthropic.go:194) — the
	// upstream value space is not enumerated by this package, only "session"
	// and "weekly*"/group "weekly" are given special handling
	// (anthropic.go:197-200); on Codex's primary/secondary window rows it is
	// left empty (codex.go:362); on Codex's derived spend-control entry it is
	// QuotaKindSpendControl (codex.go:397). The JSON value is the plain
	// string.
	Kind QuotaKind `json:"kind,omitempty"`
	// Group is populated only by Anthropic's per-limit path, from the
	// upstream group (anthropic.go:194).
	Group string `json:"group,omitempty"`
	// Slot is populated only by Codex, "primary" or "secondary", and is what
	// disambiguates the two window rows that share one bucket ID
	// (codex.go:362).
	Slot string `json:"slot,omitempty"`
	// UsedPercent is populated by every producer: Anthropic's legacy window
	// (anthropic.go:179) and per-limit (anthropic.go:194) paths, and Codex's
	// primary/secondary window rows (codex.go:362) and derived
	// spend-control entry (codex.go:397, computed as 100 minus the upstream
	// remaining percent).
	UsedPercent float64 `json:"used_percent"`
	// Unit is always PercentUnit ("percent_0_100"); every producer of a
	// Quota sets it that way (anthropic.go:179,194; codex.go:362,397).
	Unit string `json:"unit"`
	// DurationSeconds is populated by Anthropic's legacy window path from a
	// fixed table keyed by window id (anthropic.go:139-144) and by
	// Anthropic's per-limit path from a fixed table keyed by Kind/Group
	// (anthropic.go:196-201, left unset when neither matches); by Codex's
	// primary/secondary window rows from the upstream windowDurationMinutes
	// (codex.go:354,362). Left unset on Codex's derived spend-control entry.
	DurationSeconds *int64 `json:"duration_seconds,omitempty"`
	// ResetsAt is populated by every producer that has a reset time:
	// Anthropic's legacy window and per-limit paths (anthropic.go:175,190)
	// and Codex's primary/secondary window rows and derived spend-control
	// entry (codex.go:358,392-397), all from an upstream reset timestamp.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Scope is populated only by Anthropic's per-limit path, when the
	// upstream limit carries a model/surface scope (anthropic.go:207-214).
	Scope *Scope `json:"scope,omitempty"`
	// Active is populated only by Anthropic's per-limit path, from the
	// upstream isActive flag (anthropic.go:194).
	Active *bool `json:"active,omitempty"`
	// Plan is populated only by Codex, per window, from the upstream
	// planType (codex.go:367).
	Plan *PlanInfo `json:"plan,omitempty"`
	// ReachedType is populated only by Codex, from the upstream
	// rateLimitReachedType (codex.go:370).
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

// BalanceKind is the vocabulary of Balance.Kind. The two funds-on-hand kinds
// are what the schema-2 report serves as balances[]; BalanceKindSpendControl
// is a schema-1 fan-out of a Codex SpendLimit and is dropped there.
type BalanceKind string

const (
	// BalanceKindAccount is DeepSeek's prepaid account balance: Total is the
	// sum of its granted and topped_up Components and is money on hand.
	BalanceKindAccount BalanceKind = "account_balance"
	// BalanceKindWorkspaceCredits is a Codex bucket's credit balance: Total
	// is money on hand, never broken into Components.
	BalanceKindWorkspaceCredits BalanceKind = "workspace_credits"
	// BalanceKindSpendControl is the schema-1 rendering of a Codex bucket's
	// individualLimit: Total is the ceiling, not money on hand, and the one
	// "used" Component is consumption against it.
	//
	// deprecated: remove with schema 1, once clients migrate.
	BalanceKindSpendControl BalanceKind = "spend_control"
)

// Balance's Total means a different accounting equation depending on Kind,
// because the three producers populate it from three different upstream
// shapes:
//
//   - DeepSeek, Kind "account_balance" (deepseek.go:93-101): Total is the sum
//     of Components (granted + topped_up) and is spendable — money on hand.
//   - Codex, Kind "workspace_credits" (codex.go:378-383): Total is spendable
//     with no Components — also money on hand, but never broken down.
//   - Codex, Kind "spend_control" (codex.go:398): Total is a ceiling (the
//     upstream spend limit), not money on hand, and the "used" Components
//     entry is consumption against that ceiling, not a component of Total.
//
// A caller that treats every Total as spendable funds (dividing by a price
// rate, for instance) is correct only for the first two kinds.
type Balance struct {
	// Kind selects the accounting equation for this row — see BalanceKind
	// and the type doc above.
	Kind BalanceKind `json:"kind"`
	// LimitID is populated only by Codex, both for "workspace_credits" and
	// "spend_control" rows, from the upstream limitId — the same bucket id as
	// the paired Quota.Bucket (codex.go:378,398). DeepSeek never sets it. The
	// schema-1 JSON key is "scope_id", although the value is a limit bucket
	// id and never refers to a Scope; schema 2 serves it as limit_id.
	LimitID string `json:"scope_id,omitempty"`
	// Currency is populated only by DeepSeek, from the upstream balance
	// entry's currency, "USD" or "CNY" (deepseek.go:89,94). Codex never sets
	// it (it reports in provider-defined units instead — see AmountUnit).
	Currency string `json:"currency,omitempty"`
	// AmountUnit is populated by every producer with a fixed literal, not an
	// upstream value: "currency" (DeepSeek, deepseek.go:95), "credits"
	// (Codex workspace_credits, codex.go:378), or "provider_units" (Codex
	// spend_control, codex.go:398).
	AmountUnit string `json:"amount_unit,omitempty"`
	// Total is populated by DeepSeek (deepseek.go:96, the upstream
	// total_balance), Codex workspace_credits (codex.go:380-383, the
	// upstream credits balance, optional — omitted when upstream omits it),
	// and Codex spend_control (codex.go:398, the upstream spend limit). See
	// the type doc above for what Total means under each Kind.
	Total string `json:"total,omitempty"`
	// Components is populated by DeepSeek, always exactly "granted" and
	// "topped_up" (deepseek.go:97-100), and by Codex spend_control, always
	// exactly one "used" entry (codex.go:398). Codex workspace_credits never
	// sets it.
	Components []BalanceComponent `json:"components,omitempty"`
	// Available is populated only by Codex workspace_credits, from the
	// upstream hasCredits flag (codex.go:378). Do not confuse with
	// Observation.Available, which is a separate, DeepSeek-only top-level
	// flag. Neither DeepSeek nor Codex spend_control sets this field.
	Available *bool `json:"available,omitempty"`
	// Unlimited is populated only by Codex workspace_credits, from the
	// upstream unlimited flag (codex.go:378). Neither DeepSeek nor Codex
	// spend_control sets this field.
	Unlimited *bool `json:"unlimited,omitempty"`
}

type BalanceComponent struct {
	Name   string `json:"name"`
	Amount string `json:"amount"`
}

// SpendControl preserves the backend's independent per-bucket restriction
// state. A missing backend flag produces no entry; Reached=false is retained.
// Populated only by Codex (codex.go:339, from spendControlReached); no Go code
// reads it back — see Observation.SpendControls.
//
// deprecated: remove with schema 1, once clients migrate.
type SpendControl struct {
	// LimitID is the same bucket id used for the paired Quota/Balance rows
	// for that bucket (codex.go:339). The schema-1 JSON key is "scope_id".
	LimitID string `json:"scope_id"`
	// Reached is the upstream spendControlReached flag verbatim (codex.go:339).
	Reached bool `json:"reached"`
}

// SpendLimit is a spending ceiling with consumption against it — the one
// shape for what Codex reports per bucket as individualLimit plus
// spendControlReached, and Anthropic reports account-wide as extra_usage.
// The two payloads carry different subsets of these facts (Codex has a reset
// time and a reached flag but no enabled flag or currency; Anthropic the
// reverse), so every provider-dependent field is a pointer whose nil means
// "not reported". Amounts stay in the provider's decimal representation. The
// JSON tags are the schema-2 quota.spend_limits[] shape — see
// Observation.SpendLimits for why the Observation itself does not carry it.
type SpendLimit struct {
	// LimitID is the Codex bucket id, the same value as the paired
	// Quota.Bucket, Balance.LimitID and SpendControl.LimitID
	// (codex.go:341,400). Empty on Anthropic, whose extra usage is
	// account-wide and tied to no bucket.
	LimitID string `json:"limit_id,omitempty"`
	// Enabled is Anthropic's is_enabled flag (anthropic.go:274). Codex never
	// reports it.
	Enabled *bool `json:"enabled,omitempty"`
	// Limit is the ceiling: Codex individualLimit.limit (codex.go:403),
	// Anthropic monthly_limit when present (anthropic.go:278-287).
	Limit *string `json:"limit,omitempty"`
	// Used is consumption against Limit: Codex individualLimit.used
	// (codex.go:403), Anthropic used_credits when present
	// (anthropic.go:278-287).
	Used *string `json:"used,omitempty"`
	// AmountUnit is the fixed literal "provider_units" whenever Limit or
	// Used is set (codex.go:403; anthropic.go:288); empty otherwise.
	AmountUnit string `json:"amount_unit,omitempty"`
	// Currency is Anthropic's currency when present and valid
	// (anthropic.go:297-301). Codex never reports one.
	Currency string `json:"currency,omitempty"`
	// UsedPercent is on the PercentUnit scale: Codex 100 minus
	// individualLimit.remainingPercent (codex.go:396,404), Anthropic
	// utilization when present (anthropic.go:290-295).
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// Unit is PercentUnit whenever UsedPercent is set; empty otherwise.
	Unit string `json:"unit,omitempty"`
	// ResetsAt is Codex individualLimit.resetsAt (codex.go:392,404).
	// Anthropic never reports one.
	ResetsAt *time.Time `json:"resets_at,omitempty"`
	// Reached is Codex spendControlReached when present (codex.go:340-341). It
	// is independent of Limit/Used: a bucket can report either fact without
	// the other. Anthropic never reports it.
	Reached *bool `json:"reached,omitempty"`
}

// ExtraUsage retains only the documented, non-identifying fields returned by
// Anthropic. Amounts remain in the provider's original decimal representation.
// Every field is populated only by Anthropic's normalizeAnthropicExtra
// (anthropic.go:270-300); no other provider sets ExtraUsage at all. The same
// facts are also projected into a SpendLimit (spendLimitFromExtraUsage),
// which is what schema 2 serves; this shape exists for the schema-1 JSON.
//
// deprecated: remove with schema 1, once clients migrate.
type ExtraUsage struct {
	// Enabled is the upstream is_enabled flag verbatim (anthropic.go:274);
	// required, always set.
	Enabled bool `json:"enabled"`
	// MonthlyLimit is set when upstream's monthly_limit is present and
	// decimal (anthropic.go:278,280-287); AmountUnit is then also set to
	// "provider_units".
	MonthlyLimit *string `json:"monthly_limit,omitempty"`
	// UsedCredits is set when upstream's used_credits is present and decimal
	// (anthropic.go:278,280-287); AmountUnit is then also set to
	// "provider_units".
	UsedCredits *string `json:"used_credits,omitempty"`
	// AmountUnit is set to the fixed literal "provider_units" whenever
	// either MonthlyLimit or UsedCredits is set (anthropic.go:288); left
	// empty when neither upstream amount is present.
	AmountUnit string `json:"amount_unit,omitempty"`
	// UsedPercent is set when upstream's utilization is present
	// (anthropic.go:290-295); Unit is then also set to PercentUnit.
	UsedPercent *float64 `json:"used_percent,omitempty"`
	// Unit is set to the fixed literal PercentUnit ("percent_0_100") when
	// UsedPercent is set (anthropic.go:295); left empty otherwise.
	Unit string `json:"unit,omitempty"`
	// Currency is set when upstream's currency is present and valid
	// (anthropic.go:297-301).
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
	Provider  leg.ID
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

func quotaError(provider leg.ID, code ErrorCode, retryable bool) error {
	return &Error{Provider: provider, Code: code, Retryable: retryable}
}

func rateLimitError(provider leg.ID, retryAt, attemptedAt time.Time) error {
	retry := retryAt.UTC()
	return &Error{Provider: provider, Code: CodeRateLimited, Retryable: true, RetryAt: &retry, AttemptedAt: attemptedAt.UTC()}
}

func scopeHash(provider leg.ID, secret string) string {
	sum := sha256.Sum256([]byte(string(provider) + "\x00" + secret))
	return string(provider) + ":" + hex.EncodeToString(sum[:])
}

func credentialScope(provider leg.ID, secret string) (string, error) {
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
