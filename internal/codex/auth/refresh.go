package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"

	"github.com/hughescr/utraque/internal/apierr"
)

// maxRefreshBody caps how much of the token endpoint's response we read, so a
// misbehaving endpoint cannot make us buffer without bound.
const maxRefreshBody = 1 << 20 // 1 MiB

// refreshResult holds the tokens returned by a successful refresh. Any field
// may be empty; an empty refresh_token means the endpoint did not rotate it and
// the existing one is kept.
type refreshResult struct {
	AccessToken  string `json:"access_token"`
	IDToken      string `json:"id_token"`
	RefreshToken string `json:"refresh_token"`
}

// requestRefresh exchanges a refresh token for a fresh access token. It POSTs a
// JSON body matching the Codex CLI's own refresh request (client_id, grant_type,
// refresh_token — no scope: the Codex CLI sends none, and adding one risks a
// downscoped or rejected grant). Token values are never logged and never placed
// in the returned error; only the HTTP status is surfaced.
//
// Failures are classified so a caller can tell a bad credential (terminal, "run
// codex login") from a transient upstream/transport problem (retryable). A 4xx
// other than 429 means the refresh token itself is no good; a 429, a 5xx, a
// cancelled context, or a transport error is transient and must NOT be reported
// as a terminal authentication failure that stops the client retrying.
func (s *Source) requestRefresh(ctx context.Context, refreshToken string) (refreshResult, error) {
	reqBody, err := json.Marshal(map[string]string{
		"client_id":     s.clientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return refreshResult{}, apierr.Wrap(err, apierr.TypeAPI, "codex: encode refresh request")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, bytes.NewReader(reqBody))
	if err != nil {
		return refreshResult{}, apierr.Wrap(err, apierr.TypeAPI, "codex: build refresh request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		// A cancelled/expired context or a transport failure is transient, not a
		// bad credential: surface it as retryable and carry no token material.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return refreshResult{}, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeOverloaded,
				"codex token refresh was interrupted before it completed; please retry")
		}
		return refreshResult{}, apierr.WithStatus(http.StatusServiceUnavailable, apierr.TypeOverloaded,
			"codex token refresh could not reach the auth endpoint; please retry (run `codex login` if it persists)")
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxRefreshBody))
		_ = resp.Body.Close()
	}()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxRefreshBody))
	if resp.StatusCode != http.StatusOK {
		// Do not echo the body: an OAuth error body can contain the presented
		// refresh token. Surface only the status, classified by kind.
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return refreshResult{}, apierr.WithStatus(resp.StatusCode, apierr.TypeRateLimit,
				"codex token refresh was rate limited (HTTP 429); please retry shortly")
		case resp.StatusCode >= 500:
			return refreshResult{}, apierr.WithStatus(resp.StatusCode, apierr.TypeForStatus(resp.StatusCode),
				"codex token refresh hit a transient upstream error (HTTP %d); please retry", resp.StatusCode)
		default:
			// 4xx (e.g. invalid_grant): the refresh token itself is no good.
			return refreshResult{}, apierr.Authentication("codex token refresh was rejected (HTTP %d); run `codex login` to re-authenticate", resp.StatusCode)
		}
	}

	var out refreshResult
	if err := json.Unmarshal(body, &out); err != nil {
		return refreshResult{}, apierr.Authentication("codex token refresh returned an unreadable response; run `codex login`")
	}
	if out.AccessToken == "" {
		return refreshResult{}, apierr.Authentication("codex token refresh returned no access token; run `codex login`")
	}
	return out, nil
}
