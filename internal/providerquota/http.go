package providerquota

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultHTTPTimeout      = 10 * time.Second
	defaultMaxResponseBytes = int64(1 << 20)
	defaultRateLimitBackoff = time.Minute
	maxRateLimitBackoff     = 15 * time.Minute
	maxHTTPStateEntries     = 64
)

type httpFlight struct {
	done    chan struct{}
	body    []byte
	err     error
	waiters int
}

type httpStateEntry struct {
	flight         *httpFlight
	retryAt        time.Time
	lastAttempt    time.Time
	consecutive429 uint
	touchedAt      time.Time
}

type httpState struct {
	mu      sync.Mutex
	entries map[string]*httpStateEntry
}

type httpSettings struct {
	client   *http.Client
	endpoint string
	maxBody  int64
	now      func() time.Time
	state    *httpState
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
	return httpSettings{client: &copyClient, endpoint: u.String(), maxBody: maxBody, now: now,
		state: &httpState{entries: make(map[string]*httpStateEntry)}}, nil
}

func (s httpSettings) getJSON(ctx context.Context, provider Provider, cacheScope string, headers map[string]string, out any) error {
	flight, leader, err := s.acquire(ctx, provider, cacheScope)
	if err != nil {
		return err
	}
	if !leader {
		return decodeHTTPResult(provider, flight.body, flight.err, out)
	}

	attemptedAt := s.now().UTC()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		err = quotaError(provider, CodeConfiguration, false)
		s.finish(provider, cacheScope, flight, nil, err, attemptedAt, false, "")
		return err
	}
	for name, value := range headers {
		req.Header.Set(name, value)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			err = quotaError(provider, CodeTimeout, true)
		} else {
			err = quotaError(provider, CodeUnavailable, true)
		}
		s.finish(provider, cacheScope, flight, nil, err, attemptedAt, false, "")
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, s.maxBody))
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden:
			err = quotaError(provider, CodeUnauthorized, resp.StatusCode == http.StatusUnauthorized)
		case http.StatusTooManyRequests:
			// finish parses Retry-After while holding the provider client's state
			// lock, so all waiters observe the same cooldown before they can retry.
			s.finish(provider, cacheScope, flight, nil, nil, attemptedAt, true, resp.Header.Get("Retry-After"))
			return decodeHTTPResult(provider, flight.body, flight.err, out)
		default:
			err = quotaError(provider, CodeUnavailable, resp.StatusCode >= 500)
		}
		s.finish(provider, cacheScope, flight, nil, err, attemptedAt, false, "")
		return err
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, s.maxBody+1))
	if err != nil {
		err = quotaError(provider, CodeUnavailable, true)
		s.finish(provider, cacheScope, flight, nil, err, attemptedAt, false, "")
		return err
	}
	if int64(len(body)) > s.maxBody {
		err = quotaError(provider, CodeTooLarge, false)
		s.finish(provider, cacheScope, flight, nil, err, attemptedAt, false, "")
		return err
	}
	s.finish(provider, cacheScope, flight, body, nil, attemptedAt, false, "")
	return decodeHTTPResult(provider, body, nil, out)
}

func decodeHTTPResult(provider Provider, body []byte, err error, out any) error {
	if err != nil {
		return err
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

func (s httpSettings) acquire(ctx context.Context, provider Provider, scope string) (*httpFlight, bool, error) {
	if s.state == nil || scope == "" {
		return nil, false, quotaError(provider, CodeConfiguration, false)
	}
	for {
		now := s.now().UTC()
		s.state.mu.Lock()
		entry := s.state.entries[scope]
		if entry != nil && now.Before(entry.retryAt) {
			err := rateLimitError(provider, entry.retryAt, entry.lastAttempt)
			s.state.mu.Unlock()
			return nil, false, err
		}
		if entry != nil && entry.flight != nil {
			flight := entry.flight
			flight.waiters++
			s.state.mu.Unlock()
			// The leader owns the upstream request context. If it is canceled,
			// current followers share that classified failure; the entry is then
			// removed so a later call can retry immediately.
			select {
			case <-flight.done:
				s.waiterDone(flight)
				return flight, false, nil
			case <-ctx.Done():
				s.waiterDone(flight)
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					return nil, false, quotaError(provider, CodeTimeout, true)
				}
				return nil, false, quotaError(provider, CodeUnavailable, true)
			}
		}
		if entry == nil {
			if !s.makeRoomLocked(now) {
				s.state.mu.Unlock()
				return nil, false, quotaError(provider, CodeUnavailable, true)
			}
			entry = &httpStateEntry{}
			s.state.entries[scope] = entry
		}
		flight := &httpFlight{done: make(chan struct{})}
		entry.flight = flight
		entry.touchedAt = now
		s.state.mu.Unlock()
		return flight, true, nil
	}
}

func (s httpSettings) waiterDone(flight *httpFlight) {
	s.state.mu.Lock()
	flight.waiters--
	s.state.mu.Unlock()
}

func (s httpSettings) makeRoomLocked(now time.Time) bool {
	if len(s.state.entries) < maxHTTPStateEntries {
		return true
	}
	var oldestScope string
	var oldest time.Time
	for scope, entry := range s.state.entries {
		if entry.flight != nil || now.Before(entry.retryAt) {
			continue
		}
		if oldestScope == "" || entry.touchedAt.Before(oldest) {
			oldestScope, oldest = scope, entry.touchedAt
		}
	}
	if oldestScope == "" {
		return false
	}
	delete(s.state.entries, oldestScope)
	return true
}

func (s httpSettings) finish(provider Provider, scope string, flight *httpFlight, body []byte, err error, attemptedAt time.Time, rateLimited bool, retryAfter string) {
	s.state.mu.Lock()
	entry := s.state.entries[scope]
	if entry != nil && entry.flight == flight {
		entry.lastAttempt = attemptedAt
		entry.touchedAt = s.now().UTC()
		if rateLimited {
			entry.consecutive429++
			retryAt, ok := parseRetryAfterDeadline(retryAfter, entry.touchedAt)
			if !ok {
				retryAt = entry.touchedAt.Add(fallbackBackoff(entry.consecutive429))
			}
			entry.retryAt = retryAt
			err = rateLimitError(provider, retryAt, attemptedAt)
			flight.err = err
			entry.flight = nil
		} else {
			flight.body = append([]byte(nil), body...)
			flight.err = err
			delete(s.state.entries, scope)
		}
		close(flight.done)
	}
	s.state.mu.Unlock()
}

func fallbackBackoff(consecutive uint) time.Duration {
	backoff := defaultRateLimitBackoff
	for i := uint(1); i < consecutive && backoff < maxRateLimitBackoff; i++ {
		if backoff > maxRateLimitBackoff/2 {
			return maxRateLimitBackoff
		}
		backoff *= 2
	}
	if backoff > maxRateLimitBackoff {
		return maxRateLimitBackoff
	}
	return backoff
}

// parseRetryAfterDeadline accepts both RFC 9110 forms. Large delta-seconds are
// rejected before integer conversion; valid future values are never shortened.
func parseRetryAfterDeadline(raw string, now time.Time) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	if secs, err := strconv.ParseUint(raw, 10, 64); err == nil {
		if secs > uint64(math.MaxInt64)-uint64(max(now.Unix(), 0)) {
			return time.Time{}, false
		}
		deadline := time.Unix(now.Unix()+int64(secs), int64(now.Nanosecond())).UTC()
		if deadline.Year() > 9999 {
			return time.Time{}, false
		}
		return deadline, true
	}
	deadline, err := http.ParseTime(raw)
	if err != nil {
		return time.Time{}, false
	}
	deadline = deadline.UTC()
	if deadline.Before(now) {
		return time.Time{}, false
	}
	return deadline, true
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
