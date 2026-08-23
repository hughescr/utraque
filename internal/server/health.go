package server

import (
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/hughescr/utraque/internal/apierr"
)

// StatusOK is the healthy value of the /healthz "status" field.
const StatusOK = "ok"

// HealthResponse is the phase-0 /healthz body. Later phases add fields via
// Options.HealthExtra; the three fields here are always present and can never
// be overridden. Nothing here is a secret.
type HealthResponse struct {
	Status  string  `json:"status"`
	Version string  `json:"version"`
	UptimeS float64 `json:"uptime_s"`
}

// reservedHealthKeys may not be shadowed by HealthExtra.
var reservedHealthKeys = map[string]struct{}{
	"status": {}, "version": {}, "uptime_s": {},
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		_ = apierr.Write(w, apierr.WithStatus(http.StatusMethodNotAllowed,
			apierr.TypeInvalidRequest, "method %s is not allowed on %s", r.Method, HealthPath))
		return
	}

	body := map[string]any{
		"status":   StatusOK,
		"version":  s.version,
		"uptime_s": roundSeconds(s.Uptime().Seconds()),
	}
	if s.healthExtra != nil {
		for k, v := range s.healthExtra(r.Context()) {
			if _, reserved := reservedHealthKeys[k]; reserved {
				continue
			}
			body[k] = v
		}
	}

	buf, err := json.Marshal(body)
	if err != nil {
		_ = apierr.Write(w, apierr.API("encoding the health response: %v", err))
		return
	}
	buf = append(buf, '\n')

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(buf)))
	w.WriteHeader(http.StatusOK)
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(buf)
}

// roundSeconds keeps uptime to millisecond precision so the JSON stays short.
func roundSeconds(s float64) float64 { return math.Round(s*1000) / 1000 }
