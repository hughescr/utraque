// Package fixtures defines the translator testdata filename grammar.
package fixtures

const (
	// RequestInputSuffix identifies an Anthropic Messages request fixture.
	RequestInputSuffix = ".anthropic.input.json"
	// RequestGoldenSuffix identifies its translated Responses API golden JSON.
	RequestGoldenSuffix = ".responses.golden.json"
	// StreamInputSuffix identifies a Responses API stream fixture.
	StreamInputSuffix = ".responses.input.sse"
	// StreamGoldenSuffix identifies its translated Anthropic SSE golden.
	StreamGoldenSuffix = ".anthropic.golden.sse"
)
