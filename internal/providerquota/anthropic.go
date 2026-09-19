package providerquota

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	"github.com/hughescr/utraque/internal/leg"
)

const DefaultAnthropicUsageURL = "https://api.anthropic.com/api/oauth/usage"

type AnthropicOptions struct {
	URL              string
	HTTPClient       *http.Client
	Timeout          time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
}

type AnthropicClient struct{ http httpSettings }

// AnthropicCacheScope derives the same non-secret discriminator returned by
// Observation.CacheScope without contacting Anthropic.
func AnthropicCacheScope(oauthToken string) (string, error) {
	return credentialScope(leg.Anthropic, oauthToken)
}

func NewAnthropicClient(opts AnthropicOptions) (*AnthropicClient, error) {
	if opts.URL == "" {
		opts.URL = DefaultAnthropicUsageURL
	}
	h, err := newHTTPSettings(leg.Anthropic, opts.URL, opts.HTTPClient, opts.Timeout, opts.MaxResponseBytes, opts.Now)
	if err != nil {
		return nil, err
	}
	return &AnthropicClient{http: h}, nil
}

type anthropicWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type anthropicScopeLabel struct {
	ID          *string `json:"id"`
	DisplayName *string `json:"display_name"`
}

type anthropicScope struct {
	Model   *anthropicScopeLabel `json:"model"`
	Surface *anthropicScopeLabel `json:"surface"`
}

type anthropicLimit struct {
	Kind     string          `json:"kind"`
	Group    string          `json:"group"`
	Percent  *float64        `json:"percent"`
	ResetsAt *string         `json:"resets_at"`
	Scope    *anthropicScope `json:"scope"`
	IsActive *bool           `json:"is_active"`
}

type anthropicExtra struct {
	Enabled      *bool       `json:"is_enabled"`
	MonthlyLimit json.Number `json:"monthly_limit"`
	UsedCredits  json.Number `json:"used_credits"`
	Utilization  *float64    `json:"utilization"`
	Currency     *string     `json:"currency"`
}

type anthropicResponse struct {
	FiveHour       *anthropicWindow `json:"five_hour"`
	SevenDay       *anthropicWindow `json:"seven_day"`
	SevenDayOAuth  *anthropicWindow `json:"seven_day_oauth_apps"`
	SevenDayOpus   *anthropicWindow `json:"seven_day_opus"`
	SevenDaySonnet *anthropicWindow `json:"seven_day_sonnet"`
	CinderCove     *anthropicWindow `json:"cinder_cove"`
	Limits         []anthropicLimit `json:"limits"`
	ExtraUsage     *anthropicExtra  `json:"extra_usage"`
	recognized     bool
}

func (r *anthropicResponse) UnmarshalJSON(data []byte) error {
	type alias anthropicResponse
	var decoded alias
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	*r = anthropicResponse(decoded)
	for _, name := range []string{"five_hour", "seven_day", "seven_day_oauth_apps", "seven_day_opus", "seven_day_sonnet", "cinder_cove", "limits", "extra_usage"} {
		if _, ok := fields[name]; ok {
			r.recognized = true
			break
		}
	}
	return nil
}

func (c *AnthropicClient) Read(ctx context.Context, oauthToken string) (Observation, error) {
	if c == nil {
		return Observation{}, quotaError(leg.Anthropic, CodeConfiguration, false)
	}
	cacheScope, err := AnthropicCacheScope(oauthToken)
	if err != nil {
		return Observation{}, quotaError(leg.Anthropic, CodeCredential, false)
	}
	var payload anthropicResponse
	err = c.http.getJSON(ctx, leg.Anthropic, cacheScope, map[string]string{
		"Accept":         "application/json",
		"Authorization":  "Bearer " + oauthToken,
		"anthropic-beta": "oauth-2025-04-20",
	}, &payload)
	if err != nil {
		return Observation{}, err
	}
	if !payload.recognized {
		return Observation{}, quotaError(leg.Anthropic, CodeInvalidData, false)
	}
	o := Observation{Source: leg.Anthropic, CollectedAt: c.http.now().UTC(), cacheScope: cacheScope}
	if payload.Limits != nil {
		for i, limit := range payload.Limits {
			q, err := normalizeAnthropicLimit(limit, i, c.http.now())
			if err != nil {
				return Observation{}, err
			}
			o.Quotas = append(o.Quotas, q)
		}
	} else {
		legacy := []struct {
			id       string
			duration int64
			window   *anthropicWindow
		}{
			{"five_hour", 5 * 60 * 60, payload.FiveHour},
			{"seven_day", 7 * 24 * 60 * 60, payload.SevenDay},
			{"seven_day_oauth_apps", 7 * 24 * 60 * 60, payload.SevenDayOAuth},
			{"seven_day_opus", 7 * 24 * 60 * 60, payload.SevenDayOpus},
			{"seven_day_sonnet", 7 * 24 * 60 * 60, payload.SevenDaySonnet},
			{"cinder_cove", 0, payload.CinderCove},
		}
		for _, item := range legacy {
			if item.window == nil {
				continue
			}
			q, err := normalizeAnthropicWindow(item.id, item.duration, item.window, c.http.now())
			if err != nil {
				return Observation{}, err
			}
			o.Quotas = append(o.Quotas, q)
		}
	}
	if payload.ExtraUsage != nil {
		extra, err := normalizeAnthropicExtra(payload.ExtraUsage)
		if err != nil {
			return Observation{}, err
		}
		o.ExtraUsage = extra
		// Schema v1 keeps ExtraUsage on the wire; SpendLimits carries the
		// same facts in the provider-neutral shape — see
		// Observation.SpendLimits.
		o.SpendLimits = append(o.SpendLimits, spendLimitFromExtraUsage(extra))
	}
	return o, nil
}

func normalizeAnthropicWindow(id string, duration int64, w *anthropicWindow, now time.Time) (Quota, error) {
	if w.Utilization == nil || !validPercent(*w.Utilization) {
		return Quota{}, quotaError(leg.Anthropic, CodeInvalidData, false)
	}
	reset, err := parseReset(leg.Anthropic, w.ResetsAt, now)
	if err != nil {
		return Quota{}, err
	}
	q := Quota{ID: id, Bucket: id, Kind: QuotaKind(id), UsedPercent: *w.Utilization, Unit: PercentUnit, ResetsAt: reset}
	if duration > 0 {
		q.DurationSeconds = &duration
	}
	return q, nil
}

func normalizeAnthropicLimit(l anthropicLimit, index int, now time.Time) (Quota, error) {
	if l.Percent == nil || !validPercent(*l.Percent) || !validNonemptyLabel(l.Kind) || !validNonemptyLabel(l.Group) {
		return Quota{}, quotaError(leg.Anthropic, CodeInvalidData, false)
	}
	reset, err := parseReset(leg.Anthropic, l.ResetsAt, now)
	if err != nil {
		return Quota{}, err
	}
	q := Quota{ID: l.Kind, Bucket: l.Kind, Kind: QuotaKind(l.Kind), Group: l.Group, UsedPercent: *l.Percent, Unit: PercentUnit, ResetsAt: reset, Active: l.IsActive}
	var duration int64
	switch {
	case l.Kind == "session" || l.Group == "session":
		duration = 5 * 60 * 60
	case l.Kind == "weekly_all" || l.Kind == "weekly_scoped" || l.Group == "weekly":
		duration = 7 * 24 * 60 * 60
	}
	if duration > 0 {
		q.DurationSeconds = &duration
	}
	if q.ID == "" {
		q.ID = "limit_" + strconv.Itoa(index)
	}
	if l.Scope != nil {
		scope, err := normalizeAnthropicScope(l.Scope)
		if err != nil {
			return Quota{}, err
		}
		q.Scope = scope
		q.ID += anthropicScopeSuffix(scope)
	}
	return q, nil
}

func anthropicScopeSuffix(s *Scope) string {
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

func normalizeAnthropicScope(s *anthropicScope) (*Scope, error) {
	result := &Scope{}
	for in, out := range map[*anthropicScopeLabel]**ScopeLabel{s.Model: &result.Model, s.Surface: &result.Surface} {
		if in == nil {
			continue
		}
		label := &ScopeLabel{}
		if in.ID != nil {
			if !validLabel(*in.ID) {
				return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
			}
			label.ID = *in.ID
		}
		if in.DisplayName != nil {
			if !validLabel(*in.DisplayName) {
				return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
			}
			label.DisplayName = *in.DisplayName
		}
		if label.ID == "" && label.DisplayName == "" {
			return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
		}
		*out = label
	}
	if result.Model == nil && result.Surface == nil {
		return nil, nil
	}
	return result, nil
}

func normalizeAnthropicExtra(e *anthropicExtra) (*ExtraUsage, error) {
	if e.Enabled == nil {
		return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
	}
	result := &ExtraUsage{Enabled: *e.Enabled}
	amounts := []struct {
		in  json.Number
		out **string
	}{{e.MonthlyLimit, &result.MonthlyLimit}, {e.UsedCredits, &result.UsedCredits}}
	for _, amount := range amounts {
		if amount.in == "" {
			continue
		}
		s := amount.in.String()
		if !validDecimal(s) {
			return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
		}
		*amount.out = &s
		result.AmountUnit = "provider_units"
	}
	if e.Utilization != nil {
		if !validPercent(*e.Utilization) {
			return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
		}
		result.UsedPercent = e.Utilization
		result.Unit = PercentUnit
	}
	if e.Currency != nil {
		if !validCurrency(*e.Currency) {
			return nil, quotaError(leg.Anthropic, CodeInvalidData, false)
		}
		result.Currency = *e.Currency
	}
	return result, nil
}

// spendLimitFromExtraUsage projects an already-validated ExtraUsage into the
// provider-neutral SpendLimit shape. Anthropic's extra usage is account-wide,
// so LimitID stays empty; it reports no reset time and no reached flag.
func spendLimitFromExtraUsage(e *ExtraUsage) SpendLimit {
	enabled := e.Enabled
	result := SpendLimit{Enabled: &enabled, Limit: e.MonthlyLimit, Used: e.UsedCredits, AmountUnit: e.AmountUnit, Currency: e.Currency}
	if e.UsedPercent != nil {
		percent := *e.UsedPercent
		result.UsedPercent = &percent
	}
	return result
}
