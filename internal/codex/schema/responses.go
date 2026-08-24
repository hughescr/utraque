package schema

import "encoding/json"

// This file holds the OpenAI Responses API REQUEST types utraque emits to the
// Codex backend (POST {base}/responses). Like the rest of this package it is a
// pure type package — nothing beyond the standard library — so both the request
// translator and (later) the stream translator can share it without an import
// cycle. Only the request shape lives here; the streaming response events are a
// later phase.

// Input item types on a Responses request.
const (
	ItemMessage            = "message"
	ItemFunctionCall       = "function_call"
	ItemFunctionCallOutput = "function_call_output"
)

// Content part types inside a message input item. The explicit
// "type":"message" tag selects the strict item form, where the ROLE picks which
// content union applies: user and developer messages take input_text (alongside
// input_image/input_file), while an assistant message is an output message and
// takes output_text or refusal. Sending input_text under role assistant is
// rejected with "Invalid value: 'input_text'. Supported values are:
// 'output_text' and 'refusal'." Images (base64 data URLs or passed-through
// URLs) map onto input_image, which belongs to the input union and so is valid
// only on a non-assistant role; the Messages API does not accept an image block
// in an assistant turn, so that combination cannot arise from a well-formed
// request.
const (
	PartInputText  = "input_text"
	PartInputImage = "input_image"
	PartOutputText = "output_text"
)

// Reasoning effort levels the backend accepts, lowest to highest. This mirrors
// the catalog's supported_reasoning_levels ordering and is the canonical order
// the request translator clamps against.
const (
	EffortLow    = "low"
	EffortMedium = "medium"
	EffortHigh   = "high"
	EffortXHigh  = "xhigh"
	EffortMax    = "max"
	EffortUltra  = "ultra"
)

// Tool-choice string modes on a Responses request. Anthropic's auto/any/none
// map onto auto/required/none; a specific {type:tool,name} maps onto the
// FunctionToolChoice object form instead.
const (
	ToolChoiceAuto     = "auto"
	ToolChoiceRequired = "required"
	ToolChoiceNone     = "none"
)

// ToolTypeFunction is the only tool type utraque emits.
const ToolTypeFunction = "function"

// ContentPart is one element of a message item's content array. Only the field
// belonging to the part Type is populated: Text for input_text/output_text,
// ImageURL for input_image.
type ContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// InputText builds an input_text part (user-authored text).
func InputText(text string) ContentPart {
	return ContentPart{Type: PartInputText, Text: text}
}

// OutputText builds an output_text part (assistant-authored text replayed as
// conversation history).
func OutputText(text string) ContentPart {
	return ContentPart{Type: PartOutputText, Text: text}
}

// InputImage builds an input_image part from an already-formed URL (a
// "data:<media>;base64,<data>" URL for inline images, or a passed-through
// remote URL).
func InputImage(url string) ContentPart {
	return ContentPart{Type: PartInputImage, ImageURL: url}
}

// InputItem is one element of a Responses request's input[] array, in the flat
// tagged form: Type discriminates and only that type's fields are populated.
//
//   - message:              Role + Content
//   - function_call:        CallID + Name + Arguments (arguments is a JSON string)
//   - function_call_output: CallID + Output (output is flattened text)
type InputItem struct {
	Type string `json:"type"`

	// message
	Role    string        `json:"role,omitempty"`
	Content []ContentPart `json:"content,omitempty"`

	// function_call
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`

	// function_call and function_call_output
	CallID string `json:"call_id,omitempty"`

	// function_call_output. A pointer so a genuinely empty tool result still
	// serialises the required "output":"" — the Responses API rejects a
	// function_call_output that omits output — while message/function_call items
	// (nil here) leave the field off.
	Output *string `json:"output,omitempty"`
}

// MessageItem builds a message input item with the given role and parts.
// RoleDeveloper is the Responses input role that carries instruction-like
// content positioned inside the conversation. The ChatGPT Codex backend
// rejects role "system" on input items outright ("System messages are not
// allowed"); developer is the role it accepts for the same purpose.
const RoleDeveloper = "developer"

func MessageItem(role string, parts ...ContentPart) InputItem {
	return InputItem{Type: ItemMessage, Role: role, Content: parts}
}

// FunctionCall builds a function_call item. arguments is the JSON-encoded
// argument object as a string (never a nested object).
func FunctionCall(callID, name, arguments string) InputItem {
	return InputItem{Type: ItemFunctionCall, CallID: callID, Name: name, Arguments: arguments}
}

// FunctionCallOutput builds a function_call_output item carrying a tool's
// flattened text result. output is always set (even when empty) so the required
// output field is present on the wire.
func FunctionCallOutput(callID, output string) InputItem {
	return InputItem{Type: ItemFunctionCallOutput, CallID: callID, Output: &output}
}

// Tool is a function tool declaration on a Responses request. Parameters is the
// JSON Schema of the tool's inputs, carried through verbatim from the
// Anthropic tool's input_schema.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// FunctionTool builds a function tool.
func FunctionTool(name, description string, parameters json.RawMessage) Tool {
	return Tool{Type: ToolTypeFunction, Name: name, Description: description, Parameters: parameters}
}

// ToolChoice is the string-or-object union the Responses API accepts for
// tool_choice. A non-empty Function selects the object form
// ({type:"function", name:...}); otherwise Mode is emitted as a bare string
// ("auto"|"required"|"none").
type ToolChoice struct {
	Mode     string
	Function string
}

// functionToolChoice is the object wire form for a pinned function choice. A
// struct (not a map) keeps the field order stable for golden files.
type functionToolChoice struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

// MarshalJSON emits the string form for a mode, or the object form for a
// specific function.
func (tc ToolChoice) MarshalJSON() ([]byte, error) {
	if tc.Function != "" {
		return json.Marshal(functionToolChoice{Type: ToolTypeFunction, Name: tc.Function})
	}
	return json.Marshal(tc.Mode)
}

// AutoChoice, RequiredChoice, NoneChoice, and FunctionChoice build the four
// tool_choice forms.
func AutoChoice() *ToolChoice             { return &ToolChoice{Mode: ToolChoiceAuto} }
func RequiredChoice() *ToolChoice         { return &ToolChoice{Mode: ToolChoiceRequired} }
func NoneChoice() *ToolChoice             { return &ToolChoice{Mode: ToolChoiceNone} }
func FunctionChoice(n string) *ToolChoice { return &ToolChoice{Function: n} }

// Reasoning is the Responses request reasoning block: the effort level and an
// optional summary mode. Summary is omitted when empty (the backend then emits
// no reasoning summary).
type Reasoning struct {
	Effort  string `json:"effort,omitempty"`
	Summary string `json:"summary,omitempty"`
}

// ResponsesRequest is POST {base}/responses. Field order is fixed for stable
// golden output. Store and Stream are non-pointer bools that always serialise:
// utraque always sends store:false, stream:true.
type ResponsesRequest struct {
	Model             string      `json:"model"`
	Instructions      string      `json:"instructions,omitempty"`
	Input             []InputItem `json:"input"`
	Tools             []Tool      `json:"tools,omitempty"`
	ToolChoice        *ToolChoice `json:"tool_choice,omitempty"`
	ParallelToolCalls *bool       `json:"parallel_tool_calls,omitempty"`
	Reasoning         *Reasoning  `json:"reasoning,omitempty"`
	Store             bool        `json:"store"`
	Stream            bool        `json:"stream"`
}
