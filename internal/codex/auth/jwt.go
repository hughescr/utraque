package auth

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"time"
)

// accessTokenExpiry determines when the access token expires. It first reads
// the JWT "exp" claim (without verifying the signature — utraque is not the
// audience and only needs the timing). If that cannot be decoded it falls back
// to last_refresh + fallbackTTL. If neither is available it reports ok=false,
// and the caller treats the token as already expired so a refresh is forced.
func accessTokenExpiry(accessToken, lastRefresh string, fallback time.Duration) (time.Time, bool) {
	if exp, ok := jwtExp(accessToken); ok {
		return exp, true
	}
	if lastRefresh != "" {
		if t, err := time.Parse(time.RFC3339, lastRefresh); err == nil {
			return t.Add(fallback), true
		}
	}
	return time.Time{}, false
}

// jwtExp decodes the "exp" claim from a JWT's payload segment. It performs no
// signature verification. exp is read as a number of seconds since the epoch.
func jwtExp(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return time.Time{}, false
	}
	payload, err := decodeJWTSegment(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	// exp is a JSON number; decode as float64 to tolerate a fractional or
	// exponent-formatted value before truncating to whole seconds.
	var claims struct {
		Exp float64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return time.Time{}, false
	}
	if claims.Exp <= 0 {
		return time.Time{}, false
	}
	return time.Unix(int64(claims.Exp), 0), true
}

// decodeJWTSegment base64url-decodes a JWT segment, tolerating both the
// canonical unpadded form and a padded variant.
func decodeJWTSegment(seg string) ([]byte, error) {
	if b, err := base64.RawURLEncoding.DecodeString(seg); err == nil {
		return b, nil
	}
	if m := len(seg) % 4; m != 0 {
		seg += strings.Repeat("=", 4-m)
	}
	return base64.URLEncoding.DecodeString(seg)
}
