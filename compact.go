package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

const compactionKeepTail = 4

// findTailStart 从末尾保留 keep 条，并把切点从 tool 消息回退到发起调用的 assistant。
// 尾部绝不以 tool 开头，否则 assistant(tool_calls) 和它的结果会被拆开。
func findTailStart(msgs []Message, keep int) int {
	if keep < 1 {
		keep = 1
	}
	start := len(msgs) - keep
	if start < 0 {
		start = 0
	}
	for start > 0 && msgs[start].Role == "tool" {
		start--
	}
	return start
}

// messagesChars 统计一段真实消息的字符数，用于判断是否值得压缩。
func messagesChars(msgs []Message) int {
	n := 0
	for _, m := range msgs {
		n += len([]rune(m.Content))
		for _, tc := range m.ToolCalls {
			n += len([]rune(tc.Function.Name)) + len([]rune(tc.Function.Arguments))
		}
	}
	return n
}

// compact 把较早的对话总结成一条 system 摘要，保留 system、原始任务和最近几条消息。
func (a *Agent) compact(ctx context.Context, msgs []Message, step int) ([]Message, error) {
	headEnd := 0
	for headEnd < len(msgs) && msgs[headEnd].Role == "system" {
		headEnd++
	}
	if headEnd < len(msgs) && msgs[headEnd].Role == "user" {
		headEnd++ // 保留原始任务，避免摘要把目标改掉
	}
	if headEnd+2 >= len(msgs) {
		return msgs, nil
	}

	keep := a.compactKeep
	if keep < 1 {
		keep = compactionKeepTail
	}
	start := headEnd + findTailStart(msgs[headEnd:], keep)
	if start <= headEnd || start >= len(msgs) {
		return msgs, nil
	}

	old := msgs[headEnd:start]
	if len(old) == 0 {
		return msgs, nil
	}

	var sb strings.Builder
	for _, m := range old {
		switch m.Role {
		case "assistant":
			if m.Content != "" {
				fmt.Fprintf(&sb, "assistant: %s\n", truncate(m.Content, 4000))
			}
			for _, tc := range m.ToolCalls {
				fmt.Fprintf(&sb, "assistant 调用工具 %s(%s)\n", tc.Function.Name, truncate(tc.Function.Arguments, 500))
			}
		case "tool":
			fmt.Fprintf(&sb, "tool %s 返回: %s\n", m.Name, truncate(m.Content, 4000))
		default:
			fmt.Fprintf(&sb, "%s: %s\n", m.Role, truncate(m.Content, 4000))
		}
	}

	transcript := sb.String()
	// 用真实消息大小判断是否值得压缩；transcript 里的单条内容会被截断，
	// 拿它的长度当依据会低估旧上下文（工具结果动辄几千字符）。
	oldChars := messagesChars(old)
	if a.compactMin > 0 && oldChars < a.compactMin {
		fmt.Printf("[compaction] 跳过：旧消息只有 %d 字符（阈值 %d）\n", oldChars, a.compactMin)
		return msgs, nil
	}

	req := ChatRequest{
		Model:     a.cfg.Model,
		MaxTokens: 600,
		Messages: []Message{
			{Role: "system", Content: "你是对话摘要器。把下面的对话压缩成 200 字以内的中文摘要，保留：用户目标、已确认的事实、做过的决定、工具执行结果里的关键数据。不要编造没有出现过的信息。"},
			{Role: "user", Content: truncate(transcript, 16000)},
		},
	}
	resp, err := a.complete(ctx, req)
	if err != nil {
		return nil, err
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("摘要请求没有返回 choices")
	}
	if resp.Choices[0].FinishReason == "length" {
		fmt.Fprintln(os.Stderr, "[compaction] 警告：摘要达到 max_tokens 上限，可能被截断")
	}
	summary := strings.TrimSpace(resp.Choices[0].Message.Content)
	if summary == "" {
		return msgs, nil
	}
	if len([]rune(summary))*5 >= oldChars*4 {
		fmt.Printf("[compaction] 跳过替换：摘要 %d 字，旧消息 %d 字，没有明显变短\n", len([]rune(summary)), oldChars)
		return msgs, nil
	}

	a.usage.PromptTokens += resp.Usage.PromptTokens
	a.usage.CompletionTokens += resp.Usage.CompletionTokens
	a.usage.TotalTokens += resp.Usage.TotalTokens

	newMsgs := make([]Message, 0, len(msgs)-len(old)+1)
	newMsgs = append(newMsgs, msgs[:headEnd]...)
	newMsgs = append(newMsgs, Message{Role: "system", Content: "较早对话的摘要：\n" + summary})
	newMsgs = append(newMsgs, msgs[start:]...)

	a.writeCompactionLog(step, len(msgs), len(newMsgs), len(old), summary, resp.Usage)
	fmt.Printf("[compaction] 消息 %d → %d，摘要 %d 字，摘要用量 prompt=%d completion=%d\n",
		len(msgs), len(newMsgs), len([]rune(summary)), resp.Usage.PromptTokens, resp.Usage.CompletionTokens)
	return newMsgs, nil
}

func (a *Agent) writeCompactionLog(step, before, after, summarized int, summary string, usage Usage) {
	entry := map[string]any{
		"type":                "compaction",
		"step":                step,
		"before_messages":     before,
		"after_messages":      after,
		"summarized_messages": summarized,
		"summary":             truncate(summary, 500),
		"usage":               usage,
	}
	line, err := json.Marshal(entry)
	if err != nil {
		return
	}
	a.log.Write(append(line, '\n'))
	a.log.Sync()
}
