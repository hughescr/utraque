package deepseek

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/sse"
)

func rewriteRequest(raw []byte, canonical string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, apierr.InvalidRequest("deepseek request body must be a JSON object")
	}
	if err := validateContent(obj, canonical); err != nil {
		return nil, err
	}
	model, _ := json.Marshal(canonical)
	obj["model"] = model
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek request")
	}
	return out, nil
}

func validateContent(obj map[string]json.RawMessage, canonical string) error {
	for _, field := range []string{"container", "mcp_servers", "top_k"} {
		if present(obj[field]) {
			return apierr.InvalidRequest("deepseek ignores %q, so utraque cannot honor that request", field)
		}
	}
	if raw := obj["service_tier"]; present(raw) {
		var tier string
		if err := json.Unmarshal(raw, &tier); err != nil || tier != "auto" {
			return apierr.InvalidRequest("deepseek ignores service_tier=%s; only the default \"auto\" is safe to pass", string(raw))
		}
	}
	if raw := obj["output_config"]; present(raw) {
		var output map[string]json.RawMessage
		if err := json.Unmarshal(raw, &output); err != nil || output == nil {
			return apierr.InvalidRequest("deepseek output_config must be an object")
		}
		for field := range output {
			if field != "effort" {
				return apierr.InvalidRequest("deepseek output_config supports only effort, not %q", field)
			}
		}
	}
	if raw := obj["tool_choice"]; present(raw) {
		var choice struct {
			DisableParallel bool `json:"disable_parallel_tool_use"`
		}
		if err := json.Unmarshal(raw, &choice); err == nil && choice.DisableParallel {
			return apierr.InvalidRequest("deepseek ignores tool_choice.disable_parallel_tool_use=true")
		}
	}
	if raw := obj["system"]; len(raw) > 0 {
		if err := validateContentValue(raw, canonical, "system"); err != nil {
			return err
		}
	}
	if raw := obj["messages"]; len(raw) > 0 {
		var messages []struct {
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(raw, &messages); err != nil {
			return apierr.InvalidRequest("deepseek messages must be an array")
		}
		for i, message := range messages {
			if err := validateContentValue(message.Content, canonical, fmt.Sprintf("messages[%d].content", i)); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateContentValue(raw json.RawMessage, canonical, field string) error {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || raw[0] == '"' {
		return nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return apierr.InvalidRequest("deepseek %s must be a string or content-block array", field)
	}
	for i, block := range blocks {
		var kind string
		if err := json.Unmarshal(block["type"], &kind); err != nil || kind == "" {
			return apierr.InvalidRequest("deepseek %s[%d] has no valid content-block type", field, i)
		}
		switch kind {
		case "text", "thinking", "tool_use", "server_tool_use", "web_search_tool_result":
			if kind == "text" && present(block["citations"]) {
				return apierr.InvalidRequest("deepseek ignores text-block citations")
			}
		case "image":
			if canonical == "deepseek-v4-pro" {
				return apierr.InvalidRequest("deepseek-v4-pro does not support image content")
			}
		case "tool_result":
			var isError bool
			if raw := block["is_error"]; present(raw) && json.Unmarshal(raw, &isError) == nil && isError {
				return apierr.InvalidRequest("deepseek ignores tool_result.is_error=true")
			}
			if nested := block["content"]; len(nested) > 0 {
				if err := validateContentValue(nested, canonical, fmt.Sprintf("%s[%d].content", field, i)); err != nil {
					return err
				}
			}
		case "document", "search_result", "redacted_thinking", "code_execution_tool_result",
			"mcp_tool_use", "mcp_tool_result", "container_upload":
			return apierr.InvalidRequest("deepseek does not support %q content blocks", kind)
		default:
			return apierr.InvalidRequest("deepseek compatibility for %q content blocks is unknown", kind)
		}
	}
	return nil
}

func present(raw json.RawMessage) bool {
	raw = bytes.TrimSpace(raw)
	return len(raw) > 0 && !bytes.Equal(raw, []byte("null"))
}

func rewriteMessageResponse(raw []byte, canonical string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, errorsForResponse(err)
	}
	model, _ := json.Marshal(canonical)
	obj["model"] = model
	return json.Marshal(obj)
}

func rewriteStreamFrame(frame sse.Frame, canonical string) ([]byte, bool, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Data, &envelope); err != nil {
		// Keep non-JSON extension frames byte-for-byte. A message_start frame is
		// different: failing to parse it would lose the canonical-model guarantee.
		if frame.Event == "message_start" {
			return nil, false, err
		}
		return frame.Data, false, nil
	}
	if frame.Event != "message_start" && envelope.Type != "message_start" {
		return frame.Data, false, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(frame.Data, &obj); err != nil || obj == nil {
		return nil, false, errorsForResponse(err)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(obj["message"], &message); err != nil || message == nil {
		return nil, false, fmt.Errorf("message_start has no message object")
	}
	model, _ := json.Marshal(canonical)
	message["model"] = model
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, false, err
	}
	obj["message"] = encoded
	out, err := json.Marshal(obj)
	return out, true, err
}

func errorsForResponse(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("response is not a JSON object")
}
