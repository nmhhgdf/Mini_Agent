package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type toolDef struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

func fn(name, desc string, params map[string]any) toolDef {
	var d toolDef
	d.Type = "function"
	d.Function.Name = name
	d.Function.Description = desc
	d.Function.Parameters = params
	return d
}

func toolDefs() []toolDef {
	return []toolDef{
		fn("read_file", "读取工作目录内的文本文件，支持按行分页。返回文件总行数、本次显示的行范围，以及下一次读取的 offset。内容超过字符上限会自动截断。", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path":   map[string]any{"type": "string", "description": "相对工作目录的文件路径"},
				"offset": map[string]any{"type": "integer", "description": "起始行号（从 1 开始），默认 1"},
				"limit":  map[string]any{"type": "integer", "description": "最多读取多少行，默认 80"},
			},
			"required": []string{"path"},
		}),
		fn("run_command", "在工作目录内执行一条 shell 命令（Windows 用 cmd /C），返回退出码和合并输出，超时 30 秒。", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{"type": "string", "description": "要执行的命令"},
			},
			"required": []string{"command"},
		}),
		fn("grep", "在工作目录内按正则搜索文本文件，返回「文件:行号: 内容」。自动跳过 .git、node_modules 和大于 2MB 的文件。", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"pattern":     map[string]any{"type": "string", "description": "Go 正则表达式，例如 revision=abc123 或 ERROR\\s+"},
				"path":        map[string]any{"type": "string", "description": "搜索的目录或文件，默认当前目录"},
				"max_results": map[string]any{"type": "integer", "description": "最多返回多少条，默认 50，上限 200"},
			},
			"required": []string{"pattern"},
		}),
	}
}

func runTool(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "read_file":
		var a struct {
			Path   string `json:"path"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
		return readFile(a.Path, a.Offset, a.Limit)
	case "run_command":
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
		return runCommand(ctx, a.Command)
	case "grep":
		var a struct {
			Pattern    string `json:"pattern"`
			Path       string `json:"path"`
			MaxResults int    `json:"max_results"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
		return grepFiles(a.Pattern, a.Path, a.MaxResults)
	default:
		return fmt.Sprintf("ERROR: 未知工具 %q", name)
	}
}

const (
	maxFileBytes     = 20 * 1024 * 1024
	defaultReadLimit = 80
	maxReadLimit     = 500
	maxToolChars     = 6000
	commandTimeout   = 30 * time.Second
)

func readFile(path string, offset, limit int) string {
	if strings.TrimSpace(path) == "" {
		return "ERROR: path 不能为空"
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "ERROR: 取工作目录失败: " + err.Error()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "ERROR: 路径非法: " + err.Error()
	}
	rel, err := filepath.Rel(cwd, abs)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "ERROR: 只能读取工作目录内的文件"
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "ERROR: " + err.Error()
	}
	if info.IsDir() {
		return "ERROR: 这是一个目录，不是文件"
	}
	if info.Size() > maxFileBytes {
		return fmt.Sprintf("ERROR: 文件太大（%d 字节，上限 %d），请改用 run_command 配合 findstr 等工具", info.Size(), maxFileBytes)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "ERROR: " + err.Error()
	}

	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // 文件以换行结尾时 Split 会多出一个空串
	}
	total := len(lines)
	if total == 0 {
		return fmt.Sprintf("file: %s (%d bytes), total_lines: 0\n(空文件)", rel, info.Size())
	}

	if offset < 1 {
		offset = 1
	}
	if limit <= 0 {
		limit = defaultReadLimit
	}
	if limit > maxReadLimit {
		limit = maxReadLimit
	}
	if offset > total {
		return fmt.Sprintf("file: %s (%d bytes), total_lines: %d\nERROR: offset=%d 超出范围，文件只有 %d 行", rel, info.Size(), total, offset, total)
	}

	// 字符预算：留出 header 和截断提示，尽量多装整行，保证 next_offset 准确。
	budget := maxToolChars - 200
	start := offset - 1
	used := 0
	actualEnd := start
	var body strings.Builder
	for i := start; i < total && i-start < limit; i++ {
		line := lines[i]
		n := utf8.RuneCountInString(line) + 1
		if used+n > budget {
			if i == start { // 单行就超预算：截断这一行
				runes := []rune(line)
				keep := budget
				if keep > len(runes) {
					keep = len(runes)
				}
				body.WriteString(string(runes[:keep]))
				body.WriteString("…[单行过长，已截断]\n")
				actualEnd = i + 1
			}
			break
		}
		body.WriteString(line)
		body.WriteByte('\n')
		used += n
		actualEnd = i + 1
	}

	last := actualEnd
	if last < offset {
		last = offset - 1
	}
	var head string
	if last < total {
		head = fmt.Sprintf("file: %s (%d bytes), total_lines: %d, showing %d-%d, next_offset: %d\n",
			rel, info.Size(), total, offset, last, last+1)
	} else {
		head = fmt.Sprintf("file: %s (%d bytes), total_lines: %d, showing %d-%d (end)\n",
			rel, info.Size(), total, offset, last)
	}
	return truncate(head+body.String(), maxToolChars)
}

// grepFiles 在工作目录内按正则搜索文本文件，结果有上限且会跳过二进制与大文件。
func grepFiles(pattern, root string, maxResults int) string {
	if strings.TrimSpace(pattern) == "" {
		return "ERROR: pattern 不能为空"
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		return "ERROR: 正则表达式无效: " + err.Error()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "ERROR: 取工作目录失败: " + err.Error()
	}
	target := cwd
	if strings.TrimSpace(root) != "" && root != "." {
		abs, err := filepath.Abs(root)
		if err != nil {
			return "ERROR: 路径非法: " + err.Error()
		}
		rel, err := filepath.Rel(cwd, abs)
		if err != nil || strings.HasPrefix(rel, "..") {
			return "ERROR: 只能搜索工作目录内的路径"
		}
		target = abs
	}
	if maxResults <= 0 {
		maxResults = 50
	}
	if maxResults > 200 {
		maxResults = 200
	}

	var sb strings.Builder
	count := 0
	total := 0
	const maxScan = 200000
	scanCapped := false
	err = filepath.WalkDir(target, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "node_modules" {
				return fs.SkipDir
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() > 2*1024*1024 {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		rel, err := filepath.Rel(cwd, path)
		if err != nil {
			return nil
		}
		lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
		for i, line := range lines {
			if !re.MatchString(line) {
				continue
			}
			total++
			if total >= maxScan {
				scanCapped = true
				return fs.SkipAll
			}
			if count >= maxResults {
				continue // 继续统计总数，但不再收集
			}
			if utf8.RuneCountInString(line) > 300 {
				line = string([]rune(line)[:300]) + "…"
			}
			fmt.Fprintf(&sb, "%s:%d: %s\n", rel, i+1, line)
			count++
		}
		return nil
	})
	if err != nil {
		return "ERROR: 搜索失败: " + err.Error()
	}
	if total == 0 {
		return fmt.Sprintf("grep: pattern=%q, matches=0（没有找到）", pattern)
	}
	head := fmt.Sprintf("grep: pattern=%q, matches=%d", pattern, total)
	if total > count {
		head += fmt.Sprintf("（仅显示前 %d 条）", count)
	}
	if scanCapped {
		head += fmt.Sprintf("（扫描达到 %d 条上限，总数可能不完整）", maxScan)
	}
	return truncate(head+"\n"+sb.String(), maxToolChars)
}

func runCommand(ctx context.Context, command string) string {
	if strings.TrimSpace(command) == "" {
		return "ERROR: command 不能为空"
	}
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	cmd := shellCommand(cctx, command)

	out, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		var ee *exec.ExitError
		if err == context.DeadlineExceeded || cctx.Err() == context.DeadlineExceeded {
			return fmt.Sprintf("ERROR: 命令超时（%s）", commandTimeout)
		}
		if ok := asExitError(err, &ee); ok {
			code = ee.ExitCode()
		} else {
			return "ERROR: " + err.Error()
		}
	}
	return truncate(fmt.Sprintf("exit=%d\n%s", code, string(out)), maxToolChars)
}

func asExitError(err error, target **exec.ExitError) bool {
	if ee, ok := err.(*exec.ExitError); ok {
		*target = ee
		return true
	}
	return false
}

func truncate(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max]) + fmt.Sprintf("\n...[输出截断：只保留前 %d 字符]", max)
}
