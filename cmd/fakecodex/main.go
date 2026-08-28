// Command fakecodex is a stand-in for the ChatGPT Codex backend, for testing
// the ONE thing that cannot be tested without a live client: whether Claude Code
// replays the synthetic thinking blocks utraque mints, with their signatures
// intact, on the next turn.
//
// That round trip is the whole design of the reasoning replay. utraque is
// stateless per request, so a reasoning item's encrypted content only survives
// to the next turn if the client carries it there in a thinking block's
// signature. The trip is client → utraque → client and never touches OpenAI, so
// this stub answers the question at no cost to a subscription quota.
//
// Run it, point utraque at it, and drive a normal Claude Code session:
//
//	go run ./cmd/fakecodex -listen 127.0.0.1:8399
//	UTRAQUE_CODEX_BASE_URL=http://127.0.0.1:8399 utraque
//	# then, in another terminal, a session against any gpt-* route
//
// Every request is reported on stdout with the verdict that matters: how many
// reasoning items the request replayed, and whether their encrypted content
// came back byte-for-byte. A run whose second and later turns report
// "replayed=N" with matching content is a working round trip; one that reports
// "replayed=0" turn after turn means the client is not carrying the blocks back,
// and the stateless design cannot work as it stands.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
)

// issued records every encrypted blob this stub has handed out, so a replay can
// be checked against what was actually sent rather than merely looking plausible.
var issued sync.Map // string (blob) -> struct{}

var (
	blobSize  = 3000
	toolTurns = 1
)

// makeBlob builds a blob shaped like real encrypted_content: base64 characters,
// n bytes long, unique per response so a replay traces back to the turn that
// issued it.
func makeBlob(seq, n int) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	head := fmt.Sprintf("blob%04d:", seq)
	if n <= len(head) {
		return head
	}
	b := make([]byte, 0, n)
	b = append(b, head...)
	// A cheap deterministic filler. The bytes only have to be stable and
	// distinctive, not random: what is being tested is whether they come back.
	x := uint32(seq*2654435761 + 1)
	for len(b) < n {
		x = x*1664525 + 1013904223
		b = append(b, alphabet[x>>26])
	}
	return string(b)
}

func main() {
	listen := flag.String("listen", "127.0.0.1:8399", "address to listen on")
	// Real encrypted_content runs 1-11KB (measured across Codex CLI rollouts;
	// mean ~2.5KB). A stub that issues a short blob proves nothing about whether
	// a client replays a realistic one intact, so the default is a realistic size
	// and the flag exists to push past it.
	blobBytes := flag.Int("blob-bytes", 3000, "size of each synthetic encrypted_content blob")
	// How many tool calls to issue before answering. Each one costs the client a
	// turn, so this is how many reasoning items pile up in one conversation.
	turns := flag.Int("tool-turns", 1, "tool calls to issue before answering")
	flag.Parse()
	blobSize, toolTurns = *blobBytes, *turns

	mux := http.NewServeMux()
	mux.HandleFunc("/responses", handleResponses)
	// The catalog endpoint utraque fetches on startup. A single model is enough
	// to make one route resolvable.
	mux.HandleFunc("/models", handleModels)

	log.SetFlags(0)
	log.Printf("fakecodex listening on %s", *listen)
	log.Printf("point utraque at it with UTRAQUE_CODEX_BASE_URL=http://%s", *listen)
	if err := http.ListenAndServe(*listen, mux); err != nil {
		log.Fatal(err)
	}
}

func handleModels(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	fmt.Fprint(w, `{"models":[{"id":"gpt-5.6-sol","supported_reasoning_levels":["low","medium","high","xhigh"],"default_reasoning_summary":"auto"}]}`)
}

// responsesRequest is only the part of the request this stub judges.
type responsesRequest struct {
	Model          string   `json:"model"`
	Include        []string `json:"include"`
	PromptCacheKey string   `json:"prompt_cache_key"`
	Input          []struct {
		Type             string `json:"type"`
		Role             string `json:"role"`
		EncryptedContent string `json:"encrypted_content"`
	} `json:"input"`
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	var req responsesRequest
	if err := json.Unmarshal(body, &req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	report(r, &req)

	// One reasoning item with encrypted content, then a short answer. The blob
	// is unique per response so a replay can be traced back to the turn that
	// issued it.
	seq := nextSeq()
	blob := makeBlob(seq, blobSize)
	issued.Store(blob, struct{}{})

	// The first turn issues a tool call, so the client runs the tool and comes
	// BACK with the assistant turn in its history — which is the only way to see
	// whether it carried the thinking block's signature along with it. Later
	// turns just answer, ending the session.
	var toolResults int
	for _, item := range req.Input {
		if item.Type == "function_call_output" {
			toolResults++
		}
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	events := answerScript(blob)
	if toolResults < toolTurns {
		events = toolCallScript(blob, seq)
	}
	for _, ev := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
		if flusher != nil {
			flusher.Flush()
		}
	}
}

// report prints the verdict for one request.
func report(r *http.Request, req *responsesRequest) {
	var replayed, unknown, bytesBack int
	var damaged []string
	for _, item := range req.Input {
		if item.Type != "reasoning" {
			continue
		}
		replayed++
		bytesBack += len(item.EncryptedContent)
		if _, ok := issued.Load(item.EncryptedContent); ok {
			continue
		}
		unknown++
		// Name HOW it failed. A blob that came back shortened is a truncation
		// limit somewhere in the chain; one that came back the right length but
		// different is a mangling. The two have completely different fixes, and
		// "did not match" alone would not separate them.
		if head, _, found := strings.Cut(item.EncryptedContent, ":"); found {
			if want, ok := issuedByHead(head); ok {
				switch {
				case len(item.EncryptedContent) < len(want):
					damaged = append(damaged, fmt.Sprintf("%s TRUNCATED: came back %d of %d bytes", head, len(item.EncryptedContent), len(want)))
				case len(item.EncryptedContent) > len(want):
					damaged = append(damaged, fmt.Sprintf("%s GREW: came back %d bytes, issued %d", head, len(item.EncryptedContent), len(want)))
				default:
					damaged = append(damaged, fmt.Sprintf("%s ALTERED: same %d bytes long, different content", head, len(want)))
				}
				continue
			}
		}
		damaged = append(damaged, "a blob this stub never issued")
	}
	verdict := "no reasoning replayed"
	_ = bytesBack
	switch {
	case replayed > 0 && unknown == 0:
		verdict = "ROUND TRIP OK — every replayed blob matches one this stub issued"
	case unknown > 0:
		verdict = fmt.Sprintf("MISMATCH — %d of %d replayed blob(s) did not come back verbatim", unknown, replayed)
	}
	kinds := map[string]int{}
	for _, item := range req.Input {
		kinds[item.Type]++
	}
	fmt.Printf("turn: items=%d reasoning_replayed=%d reasoning_bytes_returned=%d include=%v prompt_cache_key=%q session_id=%q\n  types=%v\n  %s\n",
		len(req.Input), replayed, bytesBack, req.Include, req.PromptCacheKey, r.Header.Get("session_id"), kinds, verdict)
	for _, d := range damaged {
		fmt.Printf("  %s\n", d)
	}
	if replayed == 0 && len(req.Input) > 2 {
		fmt.Println("  NOTE: a conversation this long with no replayed reasoning is the failure this stub exists to catch.")
	}
	os.Stdout.Sync()
}

type event struct{ name, data string }

// answerScript is one complete Responses stream: a reasoning item carrying blob,
// then a text answer. It mirrors the real backend event order, including the
// encrypted content arriving on output_item.done rather than any earlier event.
func answerScript(blob string) []event {
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []event{
		{"response.created", `{"type":"response.created","response":{"id":"resp_fake","model":"gpt-5.6-sol"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_fake"}}`},
		{"response.reasoning_summary_part.added", `{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"Considering the request"}`},
		{"response.reasoning_summary_text.done", `{"type":"response.reasoning_summary_text.done","output_index":0,"text":"Considering the request"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_fake","encrypted_content":` + q(blob) + `}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","id":"msg_fake","role":"assistant"}}`},
		{"response.content_part.added", `{"type":"response.content_part.added","output_index":1,"content_index":0,"part":{"type":"output_text","text":""}}`},
		{"response.output_text.delta", `{"type":"response.output_text.delta","output_index":1,"delta":` + q("Acknowledged ("+blob+"). Ask me again to test the replay.") + `}`},
		{"response.output_text.done", `{"type":"response.output_text.done","output_index":1,"text":` + q("Acknowledged ("+blob+"). Ask me again to test the replay.") + `}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"message","id":"msg_fake","role":"assistant"}}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_fake","status":"completed","usage":{"input_tokens":100,"output_tokens":12,"input_tokens_details":{"cached_tokens":0}}}}`},
	}
}

var (
	seqMu sync.Mutex
	seq   int
)

func nextSeq() int {
	seqMu.Lock()
	defer seqMu.Unlock()
	seq++
	return seq
}

// toolCallScript is a reasoning item followed by a tool call. It is what makes
// the client take another turn, which is the only turn that can prove anything:
// the second request is the one that either carries the reasoning back or does
// not.
func toolCallScript(blob string, seq int) []event {
	callID := fmt.Sprintf("call_fake_%d", seq)
	q := func(s string) string { b, _ := json.Marshal(s); return string(b) }
	return []event{
		{"response.created", `{"type":"response.created","response":{"id":"resp_fake","model":"gpt-5.6-sol"}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_fake"}}`},
		{"response.reasoning_summary_part.added", `{"type":"response.reasoning_summary_part.added","output_index":0,"summary_index":0,"part":{"type":"summary_text","text":""}}`},
		{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","output_index":0,"delta":"I should run the probe command"}`},
		{"response.reasoning_summary_text.done", `{"type":"response.reasoning_summary_text.done","output_index":0,"text":"I should run the probe command"}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":0,"item":{"type":"reasoning","id":"rs_fake","encrypted_content":` + q(blob) + `}}`},
		{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_fake_` + strconv.Itoa(seq) + `","call_id":"` + callID + `","name":"Bash","arguments":""}}`},
		{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":1,"delta":` + q(`{"command":"echo round-trip-probe","description":"probe"}`) + `}`},
		{"response.function_call_arguments.done", `{"type":"response.function_call_arguments.done","output_index":1,"arguments":` + q(`{"command":"echo round-trip-probe","description":"probe"}`) + `}`},
		{"response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_fake_` + strconv.Itoa(seq) + `","call_id":"` + callID + `","name":"Bash","arguments":` + q(`{"command":"echo round-trip-probe","description":"probe"}`) + `}}`},
		{"response.completed", `{"type":"response.completed","response":{"id":"resp_fake","status":"completed","usage":{"input_tokens":100,"output_tokens":20,"input_tokens_details":{"cached_tokens":0}}}}`},
	}
}

// issuedByHead finds an issued blob by its "blobNNNN:" prefix, so a blob that
// came back damaged can still be matched to what was sent and compared.
func issuedByHead(head string) (string, bool) {
	var found string
	issued.Range(func(k, _ any) bool {
		s, _ := k.(string)
		if strings.HasPrefix(s, head+":") {
			found = s
			return false
		}
		return true
	})
	return found, found != ""
}
