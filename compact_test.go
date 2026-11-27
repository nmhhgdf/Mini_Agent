package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestFindTailStart(t *testing.T) {
	msgs := []Message{
		{Role: "system"},
		{Role: "user"},
		{Role: "assistant"},
		{Role: "tool"},
		{Role: "assistant"},
	}
	// 末尾保留 2 条会落在 tool 上，必须回退到发起调用的 assistant。
	if got := findTailStart(msgs, 2); got != 2 {
		t.Fatalf("tail start = %d, want 2", got)
	}

	msgs = []Message{
		{Role: "system"},
		{Role: "user"},
		{Role: "assistant"},
		{Role: "user"},
		{Role: "assistant"},
		{Role: "tool"},
	}
	if got := findTailStart(msgs, 3); got != 3 {
		t.Fatalf("tail start = %d, want 3", got)
	}

	if got := findTailStart(msgs, 100); got != 0 {
		t.Fatalf("tail start = %d, want 0", got)
	}

	// 任意 keep 下，尾部的第一条都不能是 tool。
	for keep := 1; keep <= len(msgs); keep++ {
		if start := findTailStart(msgs, keep); msgs[start].Role == "tool" {
			t.Fatalf("keep=%d leaves an orphan tool at %d", keep, start)
		}
	}
}

func TestCompact(t *testing.T) {
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"摘要：用户要依次读取三个文件。"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50,"completion_tokens":10,"total_tokens":60}}`)
	}))
	defer srv.Close()

	logFile, err := os.CreateTemp(t.TempDir(), "session-*.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	defer logFile.Close()

	a := &Agent{
		cfg:        Config{BaseURL: srv.URL, APIKey: "test", Model: "deepseek-flash"},
		compactAt:  1,
		compactMin: 1200,
		log:        logFile,
	}
	big := strings.Repeat("行内容", 500) // 2000 字符，确保超过 compactMin
	msgs := []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "原始任务：读三个文件"},
		{Role: "assistant", Content: "读 A"},
		{Role: "tool", Content: big, Name: "read_file"},
		{Role: "assistant", Content: "读 B"},
		{Role: "tool", Content: big, Name: "read_file"},
		{Role: "assistant", Content: "读 C"},
		{Role: "tool", Content: big, Name: "read_file"},
	}

	out, err := a.compact(context.Background(), msgs, 3)
	if err != nil {
		t.Fatalf("compact failed: %v", err)
	}
	if len(out) != 7 {
		t.Fatalf("messages = %d, want 7", len(out))
	}
	if out[0].Role != "system" || out[1].Content != "原始任务：读三个文件" {
		t.Fatalf("head not preserved: %+v", out[:2])
	}
	if out[2].Role != "system" || !strings.Contains(out[2].Content, "摘要：用户要依次读取三个文件。") {
		t.Fatalf("summary missing: %+v", out[2])
	}
	if out[3].Role == "tool" {
		t.Fatalf("tail starts with an orphan tool: %+v", out[3])
	}
	if a.usage.TotalTokens != 60 {
		t.Fatalf("summary usage not accumulated: %+v", a.usage)
	}

	data, _ := os.ReadFile(logFile.Name())
	if !strings.Contains(string(data), `"type":"compaction"`) {
		t.Fatalf("compaction log missing: %s", data)
	}
}

func TestCompactSkipsSmallHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not summarize when the old transcript is too small")
	}))
	defer srv.Close()

	a := &Agent{
		cfg:        Config{BaseURL: srv.URL, APIKey: "test", Model: "deepseek-flash"},
		compactMin: 5000,
	}
	msgs := []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "读 A"},
		{Role: "tool", Content: "A", Name: "read_file"},
		{Role: "assistant", Content: "读 B"},
		{Role: "tool", Content: "B", Name: "read_file"},
		{Role: "assistant", Content: "读 C"},
		{Role: "tool", Content: "C", Name: "read_file"},
	}
	out, err := a.compact(context.Background(), msgs, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("small history changed: %d -> %d", len(msgs), len(out))
	}
}

func TestCompactSkipsShortHistory(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("should not call the model for a short history")
	}))
	defer srv.Close()

	a := &Agent{cfg: Config{BaseURL: srv.URL, APIKey: "test", Model: "deepseek-flash"}}
	msgs := []Message{
		{Role: "system", Content: "system"},
		{Role: "user", Content: "task"},
		{Role: "assistant", Content: "ok"},
	}
	out, err := a.compact(context.Background(), msgs, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != len(msgs) {
		t.Fatalf("short history changed: %d -> %d", len(msgs), len(out))
	}
}
