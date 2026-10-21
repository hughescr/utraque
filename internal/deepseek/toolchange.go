package deepseek

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hughescr/utraque/internal/apierr"
)

// Mid-conversation tool changes (Claude Code's betas
// mid-conversation-tool-changes-2026-07-01 and inline-tools-2026-09-15) arrive
// as blocks inside a role "system" message in messages[]:
//
//	{"type":"tool_addition","tool":{"type":"tool_definition","definition":{"name":"x","description":"...","input_schema":{...}}}}
//	{"type":"tool_addition","tool":{"type":"tool_reference","name":"x"}}
//	{"type":"tool_removal","tool":{"type":"tool_reference","name":"x"}}
//
// DeepSeek's Anthropic-compatible endpoint documents none of them and has no
// per-turn tool list. The mapping that keeps their meaning:
//
//   - an addition with a definition is hoisted into the top-level tools array,
//     after the tools the request declared and in conversation order, so the
//     model can call the tool; a name that is already declared keeps its first
//     declaration;
//   - an addition by reference names a tool the request already declares
//     (deferred, typically); DeepSeek has no deferral, so the declaration loses
//     its defer_loading flag and the tool is callable;
//   - a removal is dropped and the tool stays declared. DeepSeek cannot express
//     "gone", earlier turns may still hold calls to the tool, and the client
//     answers a call to a tool it removed with an error result of its own;
//   - the blocks leave the message, and a message left with no content is
//     dropped, since an empty content array would be refused.
//
// Anything else in the block (an unknown target type, a definition without a
// name, a reference to a tool nothing declares) is an invalid request: the
// alternative is to lose a tool silently.

// hoistToolChanges rewrites obj in place as described above and reports the
// names it hoisted (in order) and how many removals it dropped. A request with
// no such blocks is returned untouched and costs one substring scan.
func hoistToolChanges(obj map[string]json.RawMessage) (hoisted []string, removals int, err error) {
	rawMessages := obj["messages"]
	if !bytes.Contains(rawMessages, []byte(`"tool_addition"`)) && !bytes.Contains(rawMessages, []byte(`"tool_removal"`)) {
		return nil, 0, nil
	}
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return nil, 0, apierr.InvalidRequest("deepseek messages must be an array")
	}
	var tools []map[string]json.RawMessage
	if present(obj["tools"]) {
		if err := json.Unmarshal(obj["tools"], &tools); err != nil {
			return nil, 0, apierr.InvalidRequest("deepseek tools must be an array")
		}
	}
	declared := make(map[string]int, len(tools)) // name -> index in tools
	for i, tool := range tools {
		var name string
		if json.Unmarshal(tool["name"], &name) == nil && name != "" {
			declared[name] = i
		}
	}

	toolsChanged := false
	kept := make([]map[string]json.RawMessage, 0, len(messages))
	for mi, message := range messages {
		rewritten, remaining, changed, err := hoistFromMessage(message, mi, &tools, declared, &hoisted, &removals, &toolsChanged)
		if err != nil {
			return nil, 0, err
		}
		if changed && !remaining {
			continue // nothing left to say: drop the message
		}
		if changed {
			message["content"] = rewritten
		}
		kept = append(kept, message)
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, 0, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek messages")
	}
	obj["messages"] = encoded
	if toolsChanged {
		encoded, err := json.Marshal(tools)
		if err != nil {
			return nil, 0, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek tools")
		}
		obj["tools"] = encoded
	}
	return hoisted, removals, nil
}

// hoistFromMessage strips the tool_addition and tool_removal blocks out of one
// message's content, hoisting definitions into *tools. It reports the new
// content, whether any block remains, and whether the message changed at all.
func hoistFromMessage(message map[string]json.RawMessage, mi int, tools *[]map[string]json.RawMessage,
	declared map[string]int, hoisted *[]string, removals *int, toolsChanged *bool,
) (content json.RawMessage, remaining, changed bool, err error) {
	raw := bytes.TrimSpace(message["content"])
	if len(raw) == 0 || raw[0] != '[' {
		return nil, true, false, nil // a string or absent content carries no blocks
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false, false, apierr.InvalidRequest("deepseek messages[%d].content must be a string or content-block array", mi)
	}
	out := make([]map[string]json.RawMessage, 0, len(blocks))
	for bi, block := range blocks {
		var kind string
		_ = json.Unmarshal(block["type"], &kind)
		if kind != "tool_addition" && kind != "tool_removal" {
			out = append(out, block)
			continue
		}
		changed = true
		field := fmt.Sprintf("messages[%d].content[%d]", mi, bi)
		var target struct {
			Type       string                     `json:"type"`
			Name       string                     `json:"name"`
			Definition map[string]json.RawMessage `json:"definition"`
		}
		if err := json.Unmarshal(block["tool"], &target); err != nil {
			return nil, false, false, apierr.InvalidRequest("deepseek %s has no valid tool object", field)
		}
		switch {
		case kind == "tool_removal" && target.Type == "tool_reference":
			*removals++
		case kind == "tool_addition" && target.Type == "tool_definition":
			var name string
			if json.Unmarshal(target.Definition["name"], &name) != nil || strings.TrimSpace(name) == "" {
				return nil, false, false, apierr.InvalidRequest("deepseek %s defines a tool with no name", field)
			}
			if _, dup := declared[name]; dup {
				continue
			}
			delete(target.Definition, "defer_loading")
			declared[name] = len(*tools)
			*tools = append(*tools, target.Definition)
			*hoisted = append(*hoisted, name)
			*toolsChanged = true
		case kind == "tool_addition" && target.Type == "tool_reference":
			i, ok := declared[target.Name]
			if !ok {
				return nil, false, false, apierr.InvalidRequest("deepseek cannot preserve added tool %q without its top-level input_schema", target.Name)
			}
			if _, deferred := (*tools)[i]["defer_loading"]; deferred {
				delete((*tools)[i], "defer_loading")
				*toolsChanged = true
			}
		default:
			return nil, false, false, apierr.InvalidRequest("deepseek compatibility for %s tool target %q is unknown", kind, target.Type)
		}
	}
	if !changed {
		return nil, true, false, nil
	}
	if len(out) == 0 {
		return nil, false, true, nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, false, false, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek content blocks")
	}
	return encoded, true, true, nil
}
