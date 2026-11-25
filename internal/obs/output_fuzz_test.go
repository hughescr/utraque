package obs_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/config"
	"github.com/hughescr/utraque/internal/obs"
	"github.com/hughescr/utraque/internal/server"
)

// Exercise the middleware's actual request record and closed trace files. The
// stand-in leg uses the same trace hooks as Codex but never opens a socket.
func FuzzRequestOutputRedaction(f *testing.F) {
	var logs bytes.Buffer
	logger, err := obs.NewLogger(&logs, slog.LevelDebug, obs.FormatJSON)
	if err != nil {
		f.Fatal(err)
	}
	dir := f.TempDir()
	tracer, err := obs.NewTracer(dir, logger)
	if err != nil {
		f.Fatal(err)
	}
	const private = "FuzzPrivateCanary"
	const public = "public-conversation"
	leg := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		trace := obs.TraceFrom(r.Context())
		body, err := io.ReadAll(r.Body)
		if err != nil {
			panic(err) // the status assertion below catches handler failures
		}
		trace.SetBody(body)
		obs.SummaryFrom(r.Context()).SetErr(errors.New("fixture: Bearer " + private + "Error"))
		frame := "data: " + string(body)
		if !r.URL.Query().Has("tail") {
			frame += "\n\n"
		}
		up := trace.TeeUpstream(io.NopCloser(strings.NewReader(frame)))
		defer up.Close()
		if _, err := io.Copy(io.Discard, up); err != nil {
			panic(err)
		}
		if r.URL.Query().Has("stream") {
			down := trace.TeeDownstream(w)
			cut, _ := strconv.Atoi(r.URL.Query().Get("stream"))
			cut %= len(frame) + 1
			_, _ = io.WriteString(down, frame[:cut])
			_, _ = io.WriteString(down, frame[cut:])
		} else {
			trace.WriteDownstream(body)
			_, _ = w.Write(body)
		}
	})
	srv, err := server.New(server.Options{Config: config.Default(), Logger: logger, Tracer: tracer, Routes: server.Routes{Messages: leg}})
	if err != nil {
		f.Fatal(err)
	}
	for i := range 5 {
		f.Add([]byte{byte(i), 0xff}, uint32(0x55555), uint8(i), i%2 == 0, uint16(64+i))
	}
	// Bits 3/4/5 select unterminated SSE, escaped values and raw prose.
	f.Add([]byte(`quote"slash\`), uint32(0), uint8(3|8|16), true, uint16(65))
	f.Add([]byte("raw\n"), uint32(0), uint8(3|8|16|32), true, uint16(65))
	// Expand this compact seed on replay, not in the fuzz corpus: the frame
	// itself exceeds 1 MiB, with no LF anywhere (including at EOF).
	f.Add([]byte("large"), uint32(0), uint8(3|8|16|128), true, uint16(65535))
	f.Fuzz(func(t *testing.T, data []byte, casing uint32, shape uint8, stream bool, cut uint16) {
		// Bearer, JWT and sk- values must stay in their token alphabets.
		// Quoted fields and opaque withheld headers need not.
		tail := hex.EncodeToString(data[:min(len(data), 128)])
		value := private + "Body" + tail
		var field string
		switch (shape & 7) % 5 {
		case 0:
			field = `"text":"Bearer ` + value + `"`
		case 1:
			field = `"text":"eyJabcdefgh.` + value + `.signature"`
		case 2:
			field = `"text":"sk-` + value + `"`
		case 3:
			field = `"refresh_token" : "` + value + `"`
		case 4:
			field = `"text":"?access_token=` + value + `"`
		}
		if shape&16 != 0 && shape&7 == 3 {
			quoted, _ := json.Marshal(value + "\"\\\n" + private + "End")
			field = `"refresh_token" : ` + string(quoted)
		}
		prefix := `{"control":"` + public + `",`
		if shape&128 != 0 && bytes.Equal(data, []byte("large")) {
			prefix += `"padding":"` + strings.Repeat("x", 1<<20) + `",`
		}
		body := prefix + field + `}`
		if shape&32 != 0 {
			// A text dump, not JSON. Cut in an escape at EOF, after an
			// escaped quote and a backslash followed by a physical newline.
			body = public + ` note "secret": "` + value + `\"x\` + "\n" + private + `End\`
		}
		path := "/v1/messages?fixture=1"
		if shape&8 != 0 {
			path += "&tail"
		}
		if stream {
			path += "&stream=" + strconv.Itoa(int(cut))
		}
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		// httptest's default Host is example.com, which the browser guard
		// refuses; this target tests redaction, not the guard.
		req.Host = "localhost"
		const id = "output-fuzz"
		req.Header.Set(obs.RequestIDHeader, id)
		req.Header.Set("Content-Type", "application/json")
		names := []string{"authorization", "proxy-authorization", "cookie", "set-cookie", "x-api-key", "api-key", "anthropic-auth-token", "openai-api-key", "x-utraque-token", "chatgpt-account-id", "access-token", "refresh-token", "id-token", "x-unknown"}
		for _, name := range names {
			key := []byte(name)
			for i, c := range key {
				if c >= 'a' && c <= 'z' && casing&(1<<i) != 0 {
					key[i] -= 'a' - 'A'
				}
			}
			// Direct insertion preserves the spelling Header.Add canonicalizes.
			req.Header[string(key)] = []string{private + "First" + tail, private + "Second" + tail}
		}
		logs.Reset()
		res := httptest.NewRecorder()
		srv.ServeHTTP(res, req)
		if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), value) || !strings.Contains(res.Body.String(), body) {
			t.Fatalf("fixture leg did not deliver the original body (status %d)", res.Code)
		}
		var record struct {
			Msg     string `json:"msg"`
			Err     string `json:"err"`
			Headers struct {
				ContentType string   `json:"content-type"`
				Withheld    []string `json:"headers_withheld"`
			} `json:"headers"`
		}
		if err := json.Unmarshal(logs.Bytes(), &record); err != nil {
			t.Fatalf("expected one JSON access record: %v", err)
		}
		if record.Msg != "request" || record.Err != "fixture: "+obs.Redacted || record.Headers.ContentType != "application/json" || strings.Contains(logs.String(), private) || strings.Contains(logs.String(), public) {
			t.Fatal("request record lost a control field or contains private data or body text")
		}
		// One fixed stem per worker; removing after each read both bounds disk
		// use and prevents a stale dump satisfying the next input's assertions.
		read := func(suffix string) []byte {
			t.Helper()
			path := filepath.Join(dir, obs.TraceStem(id)+suffix)
			b, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(private)) || !bytes.Contains(b, []byte(public)) || !bytes.Contains(b, []byte(obs.Redacted)) {
				t.Fatalf("%s leaked private data or lost the conversation/redaction control", suffix)
			}
			return b
		}
		var meta struct {
			Headers  map[string][]string `json:"headers"`
			Withheld []string            `json:"headers_withheld"`
		}
		if err := json.Unmarshal(read(obs.SuffixRequest), &meta); err != nil {
			t.Fatal(err)
		}
		if len(meta.Headers) != 1 || !slices.Equal(meta.Headers["content-type"], []string{"application/json"}) {
			t.Fatal("unexpected manifest header values")
		}
		for _, name := range names {
			if !slices.Contains(record.Headers.Withheld, name) || !slices.Contains(meta.Withheld, name) {
				t.Fatalf("missing withheld header name %q", name)
			}
		}
		read(obs.SuffixUpstream)
		if stream {
			read(obs.SuffixDownstream)
		} else {
			read(obs.SuffixDownstreamJSON)
		}
	})
}
