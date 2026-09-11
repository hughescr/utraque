package providerquota

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultDeepSeekBaseURL = "https://api.deepseek.com"

type DeepSeekOptions struct {
	BaseURL          string
	APIKey           string
	HTTPClient       *http.Client
	Timeout          time.Duration
	MaxResponseBytes int64
	Now              func() time.Time
}

type DeepSeekClient struct {
	http   httpSettings
	apiKey string
}

// DeepSeekCacheScope derives the same non-secret discriminator returned by
// Observation.CacheScope without contacting DeepSeek.
func DeepSeekCacheScope(apiKey string) (string, error) {
	return credentialScope(ProviderDeepSeek, apiKey)
}

func NewDeepSeekClient(opts DeepSeekOptions) (*DeepSeekClient, error) {
	if _, err := DeepSeekCacheScope(opts.APIKey); err != nil {
		return nil, quotaError(ProviderDeepSeek, CodeCredential, false)
	}
	if opts.BaseURL == "" {
		opts.BaseURL = DefaultDeepSeekBaseURL
	}
	u, err := url.Parse(strings.TrimSpace(opts.BaseURL))
	if err != nil {
		return nil, quotaError(ProviderDeepSeek, CodeConfiguration, false)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + "/user/balance"
	h, err := newHTTPSettings(ProviderDeepSeek, u.String(), opts.HTTPClient, opts.Timeout, opts.MaxResponseBytes, opts.Now)
	if err != nil {
		return nil, err
	}
	return &DeepSeekClient{http: h, apiKey: opts.APIKey}, nil
}

type deepSeekBalance struct {
	Currency     string `json:"currency"`
	TotalBalance string `json:"total_balance"`
	Granted      string `json:"granted_balance"`
	ToppedUp     string `json:"topped_up_balance"`
}

type deepSeekResponse struct {
	Available    *bool             `json:"is_available"`
	BalanceInfos []deepSeekBalance `json:"balance_infos"`
}

func (c *DeepSeekClient) Read(ctx context.Context) (Observation, error) {
	if c == nil {
		return Observation{}, quotaError(ProviderDeepSeek, CodeConfiguration, false)
	}
	var payload deepSeekResponse
	if err := c.http.getJSON(ctx, ProviderDeepSeek, map[string]string{
		"Accept":        "application/json",
		"Authorization": "Bearer " + c.apiKey,
	}, &payload); err != nil {
		return Observation{}, err
	}
	if payload.Available == nil {
		return Observation{}, quotaError(ProviderDeepSeek, CodeInvalidData, false)
	}
	cacheScope, err := DeepSeekCacheScope(c.apiKey)
	if err != nil {
		return Observation{}, err
	}
	o := Observation{Source: ProviderDeepSeek, CollectedAt: c.http.now().UTC(), Available: payload.Available, cacheScope: cacheScope}
	for _, info := range payload.BalanceInfos {
		if (info.Currency != "USD" && info.Currency != "CNY") || !validDecimal(info.TotalBalance) || !validDecimal(info.Granted) || !validDecimal(info.ToppedUp) {
			return Observation{}, quotaError(ProviderDeepSeek, CodeInvalidData, false)
		}
		o.Balances = append(o.Balances, Balance{
			Kind:       "account_balance",
			Currency:   info.Currency,
			AmountUnit: "currency",
			Total:      info.TotalBalance,
			Components: []BalanceComponent{
				{Name: "granted", Amount: info.Granted},
				{Name: "topped_up", Amount: info.ToppedUp},
			},
		})
	}
	return o, nil
}
