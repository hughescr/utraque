package deepseek

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/hughescr/utraque/internal/anthropic/schema"
	"github.com/hughescr/utraque/internal/apierr"
	"github.com/hughescr/utraque/internal/sse"
)

const toolErrorMarker = "[tool error]"

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
	toolSchemas := declaredToolSchemas(obj["tools"])
	referencedTools := make(map[string]struct{})
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
		rewritten, changed, err := rewriteContentValue(raw, canonical, "system", toolSchemas, referencedTools, false)
		if err != nil {
			return err
		}
		if changed {
			obj["system"] = rewritten
		}
	}
	if raw := obj["messages"]; len(raw) > 0 {
		var messages []map[string]json.RawMessage
		if err := json.Unmarshal(raw, &messages); err != nil {
			return apierr.InvalidRequest("deepseek messages must be an array")
		}
		changed := false
		for i, message := range messages {
			rewritten, contentChanged, err := rewriteContentValue(message["content"], canonical, fmt.Sprintf("messages[%d].content", i), toolSchemas, referencedTools, false)
			if err != nil {
				return err
			}
			if contentChanged {
				message["content"] = rewritten
				changed = true
			}
		}
		if changed {
			encoded, err := json.Marshal(messages)
			if err != nil {
				return apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek messages")
			}
			obj["messages"] = encoded
		}
	}
	if len(referencedTools) > 0 {
		rewritten, changed, err := enableReferencedTools(obj["tools"], referencedTools)
		if err != nil {
			return err
		}
		if changed {
			obj["tools"] = rewritten
		}
	}
	return nil
}

func declaredToolSchemas(raw json.RawMessage) map[string]struct{} {
	var tools []map[string]json.RawMessage
	if !present(raw) || json.Unmarshal(raw, &tools) != nil {
		return nil
	}
	declared := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		var name string
		var inputSchema map[string]json.RawMessage
		if json.Unmarshal(tool["name"], &name) != nil || strings.TrimSpace(name) == "" ||
			json.Unmarshal(tool["input_schema"], &inputSchema) != nil || inputSchema == nil {
			continue
		}
		declared[name] = struct{}{}
	}
	return declared
}

func enableReferencedTools(raw json.RawMessage, referenced map[string]struct{}) (json.RawMessage, bool, error) {
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return nil, false, apierr.InvalidRequest("deepseek tools must be an array")
	}
	changed := false
	for _, tool := range tools {
		var name string
		if json.Unmarshal(tool["name"], &name) != nil {
			continue
		}
		if _, ok := referenced[name]; !ok {
			continue
		}
		if _, ok := tool["defer_loading"]; ok {
			delete(tool, "defer_loading")
			changed = true
		}
	}
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(tools)
	if err != nil {
		return nil, false, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek tools")
	}
	return encoded, true, nil
}

func rewriteContentValue(raw json.RawMessage, canonical, field string, toolSchemas, referencedTools map[string]struct{}, inToolResult bool) (json.RawMessage, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) || raw[0] == '"' {
		return raw, false, nil
	}
	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, false, apierr.InvalidRequest("deepseek %s must be a string or content-block array", field)
	}
	changed := false
	for i, block := range blocks {
		var kind string
		if err := json.Unmarshal(block["type"], &kind); err != nil || kind == "" {
			return nil, false, apierr.InvalidRequest("deepseek %s[%d] has no valid content-block type", field, i)
		}
		switch kind {
		case "text", "thinking", "tool_use", "server_tool_use", "web_search_tool_result":
			if kind == "text" && present(block["citations"]) {
				return nil, false, apierr.InvalidRequest("deepseek ignores text-block citations")
			}
		case "image":
			if canonical == "deepseek-v4-pro" {
				return nil, false, apierr.InvalidRequest("deepseek-v4-pro does not support image content")
			}
		case "tool_result":
			if isErrorRaw, hasIsError := block["is_error"]; hasIsError {
				var isError bool
				if present(isErrorRaw) {
					if err := json.Unmarshal(isErrorRaw, &isError); err != nil {
						return nil, false, apierr.InvalidRequest("deepseek %s[%d].is_error must be a boolean", field, i)
					}
				}
				if isError {
					marked, err := markToolErrorContent(block["content"], fmt.Sprintf("%s[%d].content", field, i))
					if err != nil {
						return nil, false, err
					}
					block["content"] = marked
				}
				delete(block, "is_error")
				changed = true
			}
			if nested := block["content"]; len(nested) > 0 {
				rewritten, nestedChanged, err := rewriteContentValue(nested, canonical, fmt.Sprintf("%s[%d].content", field, i), toolSchemas, referencedTools, true)
				if err != nil {
					return nil, false, err
				}
				if nestedChanged {
					block["content"] = rewritten
					changed = true
				}
			}
		case "tool_reference":
			// Anthropic uses this block to load a client-supplied deferred schema into
			// the model context. DeepSeek documents ordinary tool schemas but neither
			// this block nor deferral, so retain the result as text and make the
			// independently supplied schema non-deferred.
			if !inToolResult {
				return nil, false, apierr.InvalidRequest("deepseek compatibility for %q content blocks is unknown", kind)
			}
			var toolName string
			if err := json.Unmarshal(block["tool_name"], &toolName); err != nil || strings.TrimSpace(toolName) == "" {
				return nil, false, apierr.InvalidRequest("deepseek %s[%d] has no valid tool_name", field, i)
			}
			if _, ok := toolSchemas[toolName]; !ok {
				return nil, false, apierr.InvalidRequest("deepseek cannot preserve discovered tool %q without its top-level input_schema", toolName)
			}
			referencedTools[toolName] = struct{}{}
			marker, _ := json.Marshal(fmt.Sprintf("[tool available: %q]", toolName))
			blockType, _ := json.Marshal("text")
			blocks[i] = map[string]json.RawMessage{"type": blockType, "text": marker}
			changed = true
		case "document", "search_result", "redacted_thinking", "code_execution_tool_result",
			"mcp_tool_use", "mcp_tool_result", "container_upload":
			return nil, false, apierr.InvalidRequest("deepseek does not support %q content blocks", kind)
		default:
			return nil, false, apierr.InvalidRequest("deepseek compatibility for %q content blocks is unknown", kind)
		}
	}
	if !changed {
		return raw, false, nil
	}
	encoded, err := json.Marshal(blocks)
	if err != nil {
		return nil, false, apierr.Wrap(err, apierr.TypeInvalidRequest, "encode deepseek content blocks")
	}
	return encoded, true, nil
}

func markToolErrorContent(raw json.RawMessage, field string) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		encoded, _ := json.Marshal(toolErrorMarker)
		return encoded, nil
	}
	if raw[0] == '"' {
		var text string
		if err := json.Unmarshal(raw, &text); err != nil {
			return nil, apierr.InvalidRequest("deepseek %s must be a string or content-block array", field)
		}
		encoded, _ := json.Marshal(markToolError(text))
		return encoded, nil
	}

	var blocks []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil, apierr.InvalidRequest("deepseek %s must be a string or content-block array", field)
	}
	for _, block := range blocks {
		var kind string
		if json.Unmarshal(block["type"], &kind) != nil || kind != "text" {
			continue
		}
		var text string
		if err := json.Unmarshal(block["text"], &text); err != nil {
			return nil, apierr.InvalidRequest("deepseek %s has a text block with no valid text", field)
		}
		block["text"], _ = json.Marshal(markToolError(text))
		return json.Marshal(blocks)
	}
	marker, _ := json.Marshal(map[string]string{"type": "text", "text": toolErrorMarker})
	var markerBlock map[string]json.RawMessage
	_ = json.Unmarshal(marker, &markerBlock)
	blocks = append([]map[string]json.RawMessage{markerBlock}, blocks...)
	return json.Marshal(blocks)
}

func markToolError(text string) string {
	if text == "" {
		return toolErrorMarker
	}
	if text == toolErrorMarker || strings.HasPrefix(text, toolErrorMarker+"\n\n") {
		return text
	}
	return toolErrorMarker + "\n\n" + text
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
	modelName, err := responseModel(obj["model"], canonical)
	if err != nil {
		return nil, err
	}
	model, _ := json.Marshal(modelName)
	obj["model"] = model
	return json.Marshal(obj)
}

func rewriteStreamFrame(frame sse.Frame, canonical string) ([]byte, string, error) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(frame.Data, &envelope); err != nil {
		// Keep non-JSON extension frames byte-for-byte. Core lifecycle events are
		// different: accepting a named event with a truncated payload can make an
		// incomplete stream appear successful.
		if isCoreStreamEvent(frame.Event) {
			return nil, "", err
		}
		return frame.Data, frame.Event, nil
	}
	eventType := envelope.Type
	if frame.Event != "" {
		eventType = frame.Event
	}
	if isCoreStreamEvent(frame.Event) || isCoreStreamEvent(envelope.Type) {
		if frame.Event != "" && frame.Event != envelope.Type {
			return nil, "", fmt.Errorf("SSE event %q contains payload type %q", frame.Event, envelope.Type)
		}
	}

	switch eventType {
	case schema.EventMessageStop:
		return frame.Data, eventType, nil
	case schema.EventError:
		var event schema.ErrorEvent
		if err := json.Unmarshal(frame.Data, &event); err != nil || event.Error.Type == "" || event.Error.Message == "" {
			return nil, "", fmt.Errorf("error event has no valid error object")
		}
		return frame.Data, eventType, nil
	case schema.EventMessageStart:
		// Continue below to normalize and validate the response identity.
	default:
		return frame.Data, eventType, nil
	}

	var obj map[string]json.RawMessage
	if err := json.Unmarshal(frame.Data, &obj); err != nil || obj == nil {
		return nil, "", errorsForResponse(err)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(obj["message"], &message); err != nil || message == nil {
		return nil, "", fmt.Errorf("message_start has no message object")
	}
	modelName, err := responseModel(message["model"], canonical)
	if err != nil {
		return nil, "", err
	}
	model, _ := json.Marshal(modelName)
	message["model"] = model
	encoded, err := json.Marshal(message)
	if err != nil {
		return nil, "", err
	}
	obj["message"] = encoded
	out, err := json.Marshal(obj)
	return out, eventType, err
}

func isCoreStreamEvent(event string) bool {
	return event == schema.EventMessageStart || event == schema.EventMessageStop || event == schema.EventError
}

func responseModel(raw json.RawMessage, requested string) (string, error) {
	var served string
	if present(raw) {
		if err := json.Unmarshal(raw, &served); err != nil {
			return "", fmt.Errorf("response model is not a string")
		}
	}
	servedCanonical, known := canonicalResponseModel(served)
	if !known {
		return requested, nil
	}
	if servedCanonical != requested {
		return "", fmt.Errorf("deepseek served model %q contradicts requested model %q", served, requested)
	}
	return servedCanonical, nil
}

func canonicalResponseModel(model string) (string, bool) {
	switch model {
	case "deepseek-flash", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp":
		return "deepseek-flash", true
	case "deepseek-v4-pro":
		return "deepseek-v4-pro", true
	default:
		return "", false
	}
}

func errorsForResponse(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("response is not a JSON object")
}
