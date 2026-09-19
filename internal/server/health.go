package server

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http"
	"strconv"

	"github.com/hughescr/utraque/internal/apierr"
)

// StatusOK is the healthy value of the /healthz "status" field.
const StatusOK = "ok"

// HealthResponse is the fixed part of the /healthz body and the single
// definition of its field set: handleHealth marshals it, and any key it
// produces is reserved — Options.HealthExtra may add fields but can never
// override one of these. Nothing here is a secret.
type HealthResponse struct {
	Status  string  `json:"status"`
	Version string  `json:"version"`
	UptimeS float64 `json:"uptime_s"`
}

// healthFields renders h as the key/value map handleHealth merges extras
// into. Numbers are kept as json.Number so the re-encoded body carries
// exactly the text encoding/json produced for the struct.
func healthFields(h HealthResponse) (map[string]any, error) {
	encoded, err := json.Marshal(h)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(encoded))
	dec.UseNumber()
	var fields map[string]any
	if err := dec.Decode(&fields); err != nil {
		return nil, err
	}
	return fields, nil
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		_ = apierr.Write(w, apierr.WithStatus(http.StatusMethodNotAllowed,
			apierr.TypeInvalidRequest, "method %s is not allowed on %s", r.Method, HealthPath))
		return
	}

	body, err := healthFields(HealthResponse{
		Status:  StatusOK,
		Version: s.version,
		UptimeS: roundSeconds(s.Uptime().Seconds()),
	})
	if err != nil {
		_ = apierr.Write(w, apierr.API("encoding the health response: %v", err))
		return
	}
	if s.healthExtra != nil {
		for k, v := range s.healthExtra(r.Context()) {
			if _, reserved := body[k]; reserved {
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
