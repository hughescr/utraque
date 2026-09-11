package providerquota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultHTTPTimeout      = 10 * time.Second
	defaultMaxResponseBytes = int64(1 << 20)
)

type httpSettings struct {
	client   *http.Client
	endpoint string
	maxBody  int64
	now      func() time.Time
}

func newHTTPSettings(provider Provider, rawURL string, client *http.Client, timeout time.Duration, maxBody int64, now func() time.Time) (httpSettings, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return httpSettings{}, quotaError(provider, CodeConfiguration, false)
	}
	if timeout <= 0 {
		timeout = defaultHTTPTimeout
	}
	if maxBody <= 0 {
		maxBody = defaultMaxResponseBytes
	}
	if maxBody > 64<<20 {
		return httpSettings{}, quotaError(provider, CodeConfiguration, false)
	}
	if now == nil {
		now = time.Now
	}
	if client == nil {
		client = &http.Client{}
	}
	copyClient := *client
	copyClient.Timeout = timeout
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return httpSettings{client: &copyClient, endpoint: u.String(), maxBody: maxBody, now: now}, nil
}

func (s httpSettings) getJSON(ctx context.Context, provider Provider, headers map[string]string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return quotaError(provider, CodeConfiguration, false)
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return quotaError(provider, CodeTimeout, true)
		}
		return quotaError(provider, CodeUnavailable, true)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s.maxBody))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			return quotaError(provider, CodeUnauthorized, resp.StatusCode == http.StatusUnauthorized)
		case http.StatusTooManyRequests:
			return quotaError(provider, CodeRateLimited, true)
		default:
			return quotaError(provider, CodeUnavailable, resp.StatusCode >= 500)
		}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBody+1))
	if err != nil {
		return quotaError(provider, CodeUnavailable, true)
	}
	if int64(len(body)) > s.maxBody {
		return quotaError(provider, CodeTooLarge, false)
	}
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	if err := dec.Decode(out); err != nil {
		return quotaError(provider, CodeInvalidData, false)
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return quotaError(provider, CodeInvalidData, false)
	}
	return nil
}

func parseReset(provider Provider, raw *string, now time.Time) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339Nano, *raw)
	if err != nil || !validReset(t, now) {
		return nil, quotaError(provider, CodeInvalidData, false)
	}
	u := t.UTC()
	return &u, nil
}

func parseUnixReset(provider Provider, raw *int64, now time.Time) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	t := time.Unix(*raw, 0).UTC()
	if !validReset(t, now) {
		return nil, quotaError(provider, CodeInvalidData, false)
	}
	return &t, nil
}

func validReset(t, now time.Time) bool {
	return !t.Before(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)) && !t.After(now.AddDate(20, 0, 0))
}

func validPercent(n float64) bool { return n >= 0 && n <= 100 }

func durationSeconds(provider Provider, minutes *int64) (*int64, error) {
	if minutes == nil {
		return nil, nil
	}
	const maxMinutes = int64((10 * 365 * 24 * time.Hour) / time.Minute)
	if *minutes <= 0 || *minutes > maxMinutes {
		return nil, quotaError(provider, CodeInvalidData, false)
	}
	seconds := *minutes * 60
	return &seconds, nil
}
