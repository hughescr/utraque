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
	Source       Provider      `json:"source"`
	CollectedAt  time.Time     `json:"collected_at"`
	Quotas       []Quota       `json:"quotas,omitempty"`
	Balances     []Balance     `json:"balances,omitempty"`
	Plan         *PlanInfo     `json:"plan,omitempty"`
	ExtraUsage   *ExtraUsage   `json:"extra_usage,omitempty"`
	ResetCredits *ResetCredits `json:"reset_credits,omitempty"`
	Available    *bool         `json:"available,omitempty"`
	cacheScope   string
}

// CacheScope returns a non-secret, provider-scoped discriminator suitable for
// preventing observations from different credentials or accounts being paired.
func (o Observation) CacheScope() string { return o.cacheScope }

type PlanInfo struct {
	Type string `json:"type"`
}

type Quota struct {
	ID              string     `json:"id"`
	Name            string     `json:"name,omitempty"`
	Kind            string     `json:"kind,omitempty"`
	Group           string     `json:"group,omitempty"`
	Slot            string     `json:"slot,omitempty"`
	UsedPercent     float64    `json:"used_percent"`
	Unit            string     `json:"unit"`
	DurationSeconds *int64     `json:"duration_seconds,omitempty"`
	ResetsAt        *time.Time `json:"resets_at,omitempty"`
	Scope           *Scope     `json:"scope,omitempty"`
	Active          *bool      `json:"active,omitempty"`
	Plan            *PlanInfo  `json:"plan,omitempty"`
	ReachedType     string     `json:"reached_type,omitempty"`
}

type Scope struct {
	Model   *ScopeLabel `json:"model,omitempty"`
	Surface *ScopeLabel `json:"surface,omitempty"`
}

type ScopeLabel struct {
	ID          string `json:"id,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

type Balance struct {
	Kind       string             `json:"kind"`
	ScopeID    string             `json:"scope_id,omitempty"`
	Currency   string             `json:"currency,omitempty"`
	AmountUnit string             `json:"amount_unit,omitempty"`
	Total      string             `json:"total,omitempty"`
	Components []BalanceComponent `json:"components,omitempty"`
	Available  *bool              `json:"available,omitempty"`
	Unlimited  *bool              `json:"unlimited,omitempty"`
}

type BalanceComponent struct {
	Name   string `json:"name"`
	Amount string `json:"amount"`
}

// ExtraUsage retains only the documented, non-identifying fields returned by
// Anthropic. Amounts remain in the provider's original decimal representation.
type ExtraUsage struct {
	Enabled      bool     `json:"enabled"`
	MonthlyLimit *string  `json:"monthly_limit,omitempty"`
	UsedCredits  *string  `json:"used_credits,omitempty"`
	AmountUnit   string   `json:"amount_unit,omitempty"`
	UsedPercent  *float64 `json:"used_percent,omitempty"`
	Unit         string   `json:"unit,omitempty"`
	Currency     string   `json:"currency,omitempty"`
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
