package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// streamChunk 是 SSE 里一行 data 对应的增量结构。
type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage"`
}

type streamResult struct {
	Content      string
	ToolCalls    []ToolCall
	FinishReason string
	Usage        Usage
}

// parseStream 读取 data-only SSE，把 delta 拼成完整消息。
// onContent 每收到一段文本就调用一次，用于边收边打印。
func parseStream(r io.Reader, onContent func(string)) (*streamResult, error) {
	res := &streamResult{}
	calls := map[int]*ToolCall{}

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		line := strings.TrimRight(scanner.Text(), "\r")
		if !strings.HasPrefix(line, "data:") {
			continue // 空行、注释行（: keep-alive）都跳过
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}

		var ch streamChunk
		if err := json.Unmarshal([]byte(payload), &ch); err != nil {
			return nil, fmt.Errorf("解析 SSE 失败: %w（payload=%s）", err, truncate(payload, 200))
		}
		if ch.Usage != nil {
			res.Usage = *ch.Usage
		}
		for _, choice := range ch.Choices {
			if choice.Delta.Content != "" {
				res.Content += choice.Delta.Content
				if onContent != nil {
					onContent(choice.Delta.Content)
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := calls[delta.Index]
				if call == nil {
					call = &ToolCall{}
					calls[delta.Index] = call
				}
				if delta.ID != "" {
					call.ID = delta.ID
				}
				if delta.Type != "" {
					call.Type = delta.Type
				}
				// name 通常只在第一块里出现；arguments 才是逐块拼接的。
				if delta.Function.Name != "" {
					call.Function.Name = delta.Function.Name
				}
				call.Function.Arguments += delta.Function.Arguments
			}
			if choice.FinishReason != nil {
				res.FinishReason = *choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for i := 0; i < len(calls); i++ {
		if call, ok := calls[i]; ok {
			res.ToolCalls = append(res.ToolCalls, *call)
		}
	}
	return res, nil
}

// streamComplete 发一次流式请求，边打印文本边累积 tool_calls。
func (a *Agent) streamComplete(ctx context.Context, req ChatRequest) (*ChatResponse, error) {
	if a.mock {
		return mockComplete(req), nil
	}

	req.Stream = true
	req.StreamOptions = &StreamOptions{IncludeUsage: true}

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

	client := &http.Client{Timeout: 300 * time.Second}
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

	printed := false
	res, err := parseStream(resp.Body, func(s string) {
		if !printed {
			fmt.Print("\nassistant: ")
			printed = true
		}
		fmt.Print(s)
	})
	if printed {
		fmt.Println()
	}
	if err != nil {
		return nil, err
	}

	msg := Message{Role: "assistant", Content: res.Content, ToolCalls: res.ToolCalls}
	return &ChatResponse{
		Choices: []struct {
			Message      Message `json:"message"`
			FinishReason string  `json:"finish_reason"`
		}{{Message: msg, FinishReason: res.FinishReason}},
		Usage: res.Usage,
	}, nil
}
