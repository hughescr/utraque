package request

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/toolschema"
)

func TestTranslateToolsDropsNestedEmailLookaround(t *testing.T) {
	const emailPattern = `^(?!\.)(?!.*\.\.)[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9.-]+$`
	raw := json.RawMessage(`{
		"type": "object",
		"properties": {
			"to": {
				"anyOf": [{
					"anyOf": [
						{"type": "string", "description": "Recipient address.", "pattern": "^(?!\\.)(?!.*\\.\\.)[A-Za-z0-9.!#$%&'*+/=?^_{|}~-]+@[A-Za-z0-9.-]+$"},
						{"type": "string", "pattern": "^[0-9]+$"}
					]
				}]
			}
		}
	}`)
	before := string(raw)
	tools, rewritten, dropped := translateTools([]aschema.Tool{{
		Name:        "send_email",
		InputSchema: raw,
	}})
	if got, want := rewritten, []string(nil); !reflect.DeepEqual(got, want) {
		t.Errorf("rewritten = %v, want %v", got, want)
	}
	if got, want := dropped, []string{"send_email.properties.to.anyOf.0.anyOf.0"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dropped = %v, want %v", got, want)
	}
	if len(tools) != 1 {
		t.Fatalf("tools length = %d, want 1", len(tools))
	}
	if string(raw) != before {
		t.Errorf("input schema was mutated:\n got %s\nwant %s", raw, before)
	}

	var got map[string]any
	if err := json.Unmarshal(tools[0].Parameters, &got); err != nil {
		t.Fatalf("translated parameters are not JSON: %v\n%s", err, tools[0].Parameters)
	}
	to := got["properties"].(map[string]any)["to"].(map[string]any)
	nested := to["anyOf"].([]any)[0].(map[string]any)["anyOf"].([]any)
	email := nested[0].(map[string]any)
	if _, ok := email["pattern"]; ok {
		t.Error("nested email pattern was not removed")
	}
	if got, want := email["description"], "Recipient address. "+toolschema.PatternNote+emailPattern; got != want {
		t.Errorf("nested email description = %q, want %q", got, want)
	}
	if got, want := nested[1].(map[string]any)["pattern"], "^[0-9]+$"; got != want {
		t.Errorf("sibling pattern = %q, want %q", got, want)
	}
}
