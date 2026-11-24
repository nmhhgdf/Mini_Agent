package main

import (
	"strings"
	"testing"
)

const sampleSSE = `data: {"choices":[{"delta":{"content":"你"}}]}

data: {"choices":[{"delta":{"content":"好"}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"go"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".mod\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}

data: [DONE]
data: {"this line must be ignored after DONE"}
`

func TestParseStream(t *testing.T) {
	var printed strings.Builder
	res, err := parseStream(strings.NewReader(sampleSSE), func(s string) {
		printed.WriteString(s)
	})
	if err != nil {
		t.Fatalf("parseStream failed: %v", err)
	}

	if res.Content != "你好" {
		t.Fatalf("content = %q", res.Content)
	}
	if printed.String() != "你好" {
		t.Fatalf("streamed text = %q", printed.String())
	}
	if res.FinishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q", res.FinishReason)
	}
	if res.Usage.TotalTokens != 15 {
		t.Fatalf("usage = %+v", res.Usage)
	}
	if len(res.ToolCalls) != 1 {
		t.Fatalf("tool calls = %+v", res.ToolCalls)
	}
	call := res.ToolCalls[0]
	if call.ID != "call_1" || call.Function.Name != "read_file" {
		t.Fatalf("tool call identity = %+v", call)
	}
	if call.Function.Arguments != `{"path":"go.mod"}` {
		t.Fatalf("tool call arguments = %q", call.Function.Arguments)
	}
}
