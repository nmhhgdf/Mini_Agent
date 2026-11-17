package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

// ---------- OpenAI 兼容的线格式（DeepSeek 用的就是这套） ----------

type ToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ChatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	Tools     []toolDef `json:"tools,omitempty"`
	MaxTokens int       `json:"max_tokens,omitempty"`
}

type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage Usage `json:"usage"`
}

type Config struct {
	APIKey  string
	BaseURL string
	Model   string
}

type Agent struct {
	cfg   Config
	mock  bool
	log   *os.File
	usage Usage
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	mock := flag.Bool("mock", false, "用假模型跑通循环，不调用 API")
	task := flag.String("task", "读取 go.mod，告诉我 module 名和 go 版本，然后用一句话说明你用了哪些工具。", "交给 agent 的任务")
	maxSteps := flag.Int("max-steps", 8, "最多模型调用次数")
	flag.Parse()

	cfg := Config{
		APIKey:  os.Getenv("DEEPSEEK_API_KEY"),
		BaseURL: envOr("DEEPSEEK_BASE_URL", "https://api.deepseek.com"),
		Model:   envOr("DEEPSEEK_MODEL", "deepseek-flash"),
	}
	if !*mock && cfg.APIKey == "" {
		fmt.Fprintln(os.Stderr, "缺少 DEEPSEEK_API_KEY。先设置环境变量，或加 -mock 用假模型跑通循环。")
		os.Exit(1)
	}

	logFile, err := os.Create("session.jsonl")
	if err != nil {
		fmt.Fprintln(os.Stderr, "创建会话日志失败:", err)
		os.Exit(1)
	}
	defer logFile.Close()

	a := &Agent{cfg: cfg, mock: *mock, log: logFile}
	if err := a.Run(context.Background(), *task, *maxSteps); err != nil {
		fmt.Fprintln(os.Stderr, "\n[agent 失败]", err)
		os.Exit(1)
	}
}

func (a *Agent) Run(ctx context.Context, task string, maxSteps int) error {
	msgs := []Message{
		{Role: "system", Content: "你是一个最小可用的编码 Agent。需要事实时先调用工具；工具失败时自己修正；回答用中文，尽量简洁。"},
		{Role: "user", Content: task},
	}

	for step := 1; step <= maxSteps; step++ {
		req := ChatRequest{Model: a.cfg.Model, Messages: msgs, Tools: toolDefs(), MaxTokens: 1024}
		resp, err := a.complete(ctx, req)
		if err != nil {
			return fmt.Errorf("step %d 请求失败: %w", step, err)
		}
		if len(resp.Choices) == 0 {
			return fmt.Errorf("step %d 没有返回 choices", step)
		}
		a.usage.PromptTokens += resp.Usage.PromptTokens
		a.usage.CompletionTokens += resp.Usage.CompletionTokens
		a.usage.TotalTokens += resp.Usage.TotalTokens

		msg := resp.Choices[0].Message
		msgs = append(msgs, msg)
		fmt.Printf("\n── step %d ──  prompt=%d completion=%d\n", step, resp.Usage.PromptTokens, resp.Usage.CompletionTokens)

		if len(msg.ToolCalls) == 0 {
			fmt.Println("\nassistant:", msg.Content)
			a.writeLog(step, msg, nil, resp.Usage)
			return nil
		}

		var results []Message
		for _, tc := range msg.ToolCalls {
			fmt.Printf("→ tool: %s %s\n", tc.Function.Name, tc.Function.Arguments)
			out := runTool(ctx, tc.Function.Name, tc.Function.Arguments)
			fmt.Printf("← %s\n", preview(out))
			results = append(results, Message{
				Role:       "tool",
				ToolCallID: tc.ID,
				Name:       tc.Function.Name,
				Content:    out,
			})
		}
		msgs = append(msgs, results...)
		a.writeLog(step, msg, results, resp.Usage)
	}

	return errors.New("达到 max-steps 仍未结束：检查工具是否总是失败，或调大 -max-steps")
}

func (a *Agent) complete(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if a.mock {
		return mockComplete(req), nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		hint := ""
		if resp.StatusCode == http.StatusUnauthorized {
			hint = "（检查 DEEPSEEK_API_KEY 是否正确）"
		}
		return nil, fmt.Errorf("HTTP %d %s%s", resp.StatusCode, string(raw), hint)
	}

	var out ChatResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// writeLog 模拟 DSH 的会话日志：每一步把 assistant 消息和工具结果追加成一行 JSON。
func (a *Agent) writeLog(step int, assistant Message, results []Message, usage Usage) {
	entry := map[string]any{
		"step":      step,
		"assistant": assistant,
		"usage":     usage,
	}
	if len(results) > 0 {
		entry["tool_results"] = results
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	a.log.Write(append(line, '\n'))
	a.log.Sync()
}

func mockComplete(req ChatRequest) *ChatResponse {
	hasToolResult := false
	for _, m := range req.Messages {
		if m.Role == "tool" {
			hasToolResult = true
		}
	}

	var msg Message
	usage := Usage{PromptTokens: 320, CompletionTokens: 24, TotalTokens: 344}

	if !hasToolResult {
		var tc ToolCall
		tc.ID = "call_mock_1"
		tc.Type = "function"
		tc.Function.Name = "read_file"
		tc.Function.Arguments = `{"path":"go.mod"}`
		msg = Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{tc}}
	} else {
		msg = Message{Role: "assistant", Content: "（mock）我只做了一件事：调用 read_file 读取 go.mod。真实模型会根据文件内容继续回答。"}
		usage = Usage{PromptTokens: 512, CompletionTokens: 42, TotalTokens: 554}
	}

	return &ChatResponse{
		Choices: []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{{Message: msg, FinishReason: "stop"}},
		Usage: usage,
	}
}

func preview(s string) string {
	const n = 120
	s = string([]rune(s))
	if len([]rune(s)) <= n {
		return s
	}
	return string([]rune(s)[:n]) + "…"
}
