package request_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hughescr/utraque/internal/anthropic"
	aschema "github.com/hughescr/utraque/internal/anthropic/schema"
	cschema "github.com/hughescr/utraque/internal/codex/schema"
	"github.com/hughescr/utraque/internal/router"
	"github.com/hughescr/utraque/internal/translate/request"
	"github.com/hughescr/utraque/internal/translate/stream"
)

// TestReasoningSurvivesTheRoundTrip is the whole mechanism in one test: what the
// stream translator emits for a reasoning item must come back through a client
// replay as a reasoning input item carrying the same encrypted content.
//
// The two halves are written by different packages and only ever meet in a
// running session, where getting it wrong is invisible — the request still
// succeeds, it just silently stops matching the backend's prompt cache and
// re-reads the whole conversation on every turn. So they are joined here.
func TestReasoningSurvivesTheRoundTrip(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("../../../testdata/streams", "reasoning_encrypted.codex.sse"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	// 1. Translate the Codex stream into what the client is told.
	var buf bytes.Buffer
	w := stream.NewSSEWriter(&buf)
	tr := stream.New(stream.Options{
		Model: "gpt-5.6-sol", EmitReasoning: "thinking", OnTruncate: "error",
		Heartbeat: -1, UpstreamIdle: -1,
	})
	if _, err := tr.Run(context.Background(), bytes.NewReader(raw), w); err != nil {
		t.Fatalf("stream translate: %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("flush: %v", err)
	}

	sigs := signaturesIn(t, buf.String())
	if len(sigs) != 2 {
		t.Fatalf("expected a signature for each of the fixture's two reasoning items, got %d", len(sigs))
	}

	// 2. Replay those blocks the way a client does on its next turn.
	msgs := []aschema.Message{
		{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Which file holds the port?"))},
		{Role: aschema.RoleAssistant, Content: contentOf(
			aschema.ThinkingBlock("Weighing it up", sigs[0]),
			aschema.ThinkingBlock("", sigs[1]),
			aschema.TextBlock("The answer is 42."),
		)},
		{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Thanks."))},
	}
	out, meta, err := request.Translate(
		&aschema.MessagesRequest{Model: "sol-high", Messages: msgs},
		decisionFor("gpt-5.6-sol"), cschema.Model{}, request.Options{},
	)
	if err != nil {
		t.Fatalf("request translate: %v", err)
	}

	// 3. The encrypted content the backend gave us is what goes back to it.
	var got []string
	for _, item := range out.Input {
		if item.Type == cschema.ItemReasoning {
			got = append(got, item.EncryptedContent)
		}
	}
	want := []string{"ENCRYPTEDSUMMARISED==", "ENCRYPTEDSILENT=="}
	if len(got) != len(want) {
		t.Fatalf("replayed %d reasoning items, want %d: %v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("reasoning item %d: replayed %q, want %q", i, got[i], want[i])
		}
	}
	if meta.ReasoningReplayed != 2 || meta.ReasoningUnreplayable != 0 {
		t.Errorf("metadata: replayed=%d unreplayable=%d, want 2 and 0",
			meta.ReasoningReplayed, meta.ReasoningUnreplayable)
	}

	// A reasoning item must precede the assistant message it reasoned toward:
	// the backend cached the tokens in that order, and a different order is a
	// different prefix.
	if idx := indexOfType(out.Input, cschema.ItemReasoning); idx != 1 {
		t.Errorf("first reasoning item sits at input[%d], want input[1] — directly after the opening user turn", idx)
	}

	// And the request must ask for encrypted content, or the NEXT turn has
	// nothing to replay and the whole mechanism stops after one round.
	if len(out.Include) != 1 || out.Include[0] != cschema.IncludeReasoningEncryptedContent {
		t.Errorf("include = %v, want [%s]", out.Include, cschema.IncludeReasoningEncryptedContent)
	}
}

// TestPromptCacheKeyIsStableAsTheConversationGrows pins the property the key
// exists for. A key that changed per turn would be worse than sending none:
// every turn would claim a prefix nothing had cached under that name.
func TestPromptCacheKeyIsStableAsTheConversationGrows(t *testing.T) {
	base := []aschema.Message{
		{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Which file holds the port?"))},
	}
	grown := append(append([]aschema.Message{}, base...),
		aschema.Message{Role: aschema.RoleAssistant, Content: contentOf(aschema.TextBlock("config.go:41."))},
		aschema.Message{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("And the host?"))},
	)

	sys := contentOf(aschema.TextBlock("You are a helpful assistant."))
	first := keyFor(t, &aschema.MessagesRequest{Model: "sol-high", System: sys, Messages: base}, "gpt-5.6-sol")
	later := keyFor(t, &aschema.MessagesRequest{Model: "sol-high", System: sys, Messages: grown}, "gpt-5.6-sol")
	if first != later {
		t.Errorf("cache key changed as the conversation grew: %q then %q", first, later)
	}
	if !strings.HasPrefix(first, "utq-") {
		t.Errorf("cache key %q does not carry the utq- prefix the session id is derived from", first)
	}

	// A different model is a different cache. Sharing a key across them would
	// point two prefixes at one entry.
	other := keyFor(t, &aschema.MessagesRequest{Model: "terra-high", System: sys, Messages: base}, "gpt-5.6-terra")
	if other == first {
		t.Error("two models produced the same cache key")
	}

	// So is a different opening turn: it is the start of a different prefix.
	changed := keyFor(t, &aschema.MessagesRequest{
		Model:  "sol-high",
		System: sys,
		Messages: []aschema.Message{
			{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Something else entirely."))},
		},
	}, "gpt-5.6-sol")
	if changed == first {
		t.Error("a different opening turn produced the same cache key")
	}
}

// A thinking block utraque did not mint, or one of ours with nothing in it, is
// dropped and counted — never forged into a reasoning item.
func TestUnreplayableThinkingIsDroppedAndCounted(t *testing.T) {
	msgs := []aschema.Message{
		{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Hello."))},
		{Role: aschema.RoleAssistant, Content: contentOf(
			aschema.ThinkingBlock("genuine Claude thinking", "ErUBCkYIBRgCKkDdT8v0aGVudGhpbmc="),
			aschema.ThinkingBlock("ours, from before encrypted content", anthropic.SyntheticThinkingMarker+"resp_9-0"),
			aschema.TextBlock("Hi."),
		)},
	}
	out, meta, err := request.Translate(
		&aschema.MessagesRequest{Model: "sol-high", Messages: msgs},
		decisionFor("gpt-5.6-sol"), cschema.Model{}, request.Options{},
	)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if idx := indexOfType(out.Input, cschema.ItemReasoning); idx != -1 {
		t.Errorf("a reasoning item was forged from an unreplayable block at input[%d]", idx)
	}
	if meta.ReasoningReplayed != 0 || meta.ReasoningUnreplayable != 2 {
		t.Errorf("metadata: replayed=%d unreplayable=%d, want 0 and 2",
			meta.ReasoningReplayed, meta.ReasoningUnreplayable)
	}
}

func keyFor(t *testing.T, req *aschema.MessagesRequest, upstream string) string {
	t.Helper()
	out, meta, err := request.Translate(req, decisionFor(upstream), cschema.Model{}, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if out.PromptCacheKey != meta.PromptCacheKey {
		t.Errorf("request and metadata disagree on the cache key: %q vs %q", out.PromptCacheKey, meta.PromptCacheKey)
	}
	return out.PromptCacheKey
}

func decisionFor(upstream string) router.Decision {
	return router.Decision{Backend: router.BackendCodex, UpstreamModel: upstream}
}

func contentOf(blocks ...aschema.ContentBlock) *aschema.Content {
	return &aschema.Content{Blocks: blocks}
}

func indexOfType(items []cschema.InputItem, typ string) int {
	for i, it := range items {
		if it.Type == typ {
			return i
		}
	}
	return -1
}

// signaturesIn pulls every signature_delta value out of an Anthropic SSE body,
// in order — which is exactly what a client keeps and replays.
func signaturesIn(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		data, found := strings.CutPrefix(line, "data: ")
		if !found {
			continue
		}
		var ev struct {
			Delta struct {
				Type      string `json:"type"`
				Signature string `json:"signature"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(data), &ev); err != nil {
			continue
		}
		if ev.Delta.Type == "signature_delta" {
			out = append(out, ev.Delta.Signature)
		}
	}
	return out
}

// TestBillingHeaderDoesNotMoveTheCacheKey is a regression test for the thing
// that made the cache key useless in a live session: Claude Code prepends a
// billing-metadata system block whose "cch" value changes on every turn. It
// sits first, so leaving it in would change the opening tokens of every prompt
// and invalidate the cached prefix from its first token.
//
// The two requests here differ ONLY in that value, exactly as two turns of one
// real conversation do.
func TestBillingHeaderDoesNotMoveTheCacheKey(t *testing.T) {
	build := func(cch string) *aschema.MessagesRequest {
		return &aschema.MessagesRequest{
			Model: "sol-high",
			System: contentOf(
				aschema.TextBlock("x-anthropic-billing-header: cc_version=2.1.241; cc_entrypoint=sdk-cli; cch="+cch+";"),
				aschema.TextBlock("You are a helpful assistant."),
			),
			Messages: []aschema.Message{
				{Role: aschema.RoleUser, Content: contentOf(aschema.TextBlock("Run the probe command."))},
			},
		}
	}

	first, meta := translateFor(t, build("44384"))
	second, _ := translateFor(t, build("9b852"))

	if first.PromptCacheKey != second.PromptCacheKey {
		t.Errorf("the billing header moved the cache key: %q then %q", first.PromptCacheKey, second.PromptCacheKey)
	}
	if first.Instructions != second.Instructions {
		t.Errorf("the billing header changed the instructions:\n%q\n%q", first.Instructions, second.Instructions)
	}
	if strings.Contains(first.Instructions, "x-anthropic-billing-header") {
		t.Errorf("billing metadata reached the Codex backend as instructions: %q", first.Instructions)
	}
	if first.Instructions != "You are a helpful assistant." {
		t.Errorf("instructions = %q, want the real system prompt with the header removed", first.Instructions)
	}
	if !slicesContains(meta.Dropped, request.DroppedBillingHeader) {
		t.Errorf("the drop was not recorded: Dropped = %v", meta.Dropped)
	}
}

func translateFor(t *testing.T, req *aschema.MessagesRequest) (*cschema.ResponsesRequest, request.Metadata) {
	t.Helper()
	out, meta, err := request.Translate(req, decisionFor("gpt-5.6-sol"), cschema.Model{}, request.Options{})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	return out, meta
}

func slicesContains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
