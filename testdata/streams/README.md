# Stream fixtures

Each fixture is named `<case>.<protocol>.<role>.<ext>`, where protocol is `anthropic` or `responses` and role is `input` or `golden`. Responses API stream
inputs use `<case>.responses.input.sse`; translated Anthropic stream goldens use
`<case>.anthropic.golden.sse`.
