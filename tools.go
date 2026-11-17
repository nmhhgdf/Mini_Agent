package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
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
		fn("read_file", "读取工作目录内的文本文件。返回文件内容；超过限制会被截断。", map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{"type": "string", "description": "相对工作目录的文件路径"},
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
	}
}

func runTool(ctx context.Context, name, argsJSON string) string {
	switch name {
	case "read_file":
		var a struct {
			Path string `json:"path"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
		return readFile(a.Path)
	case "run_command":
		var a struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "ERROR: 参数不是合法 JSON: " + err.Error()
		}
		return runCommand(ctx, a.Command)
	default:
		return fmt.Sprintf("ERROR: 未知工具 %q", name)
	}
}

const (
	maxReadBytes   = 64 * 1024
	maxToolChars   = 6000
	commandTimeout = 30 * time.Second
)

func readFile(path string) string {
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
	data, err := os.ReadFile(abs)
	if err != nil {
		return "ERROR: " + err.Error()
	}

	note := ""
	if len(data) > maxReadBytes {
		data = data[:maxReadBytes]
		note = fmt.Sprintf("\n...[读取截断：只取前 %d 字节]", maxReadBytes)
	}
	return truncate(fmt.Sprintf("file: %s (%d bytes)\n%s%s", rel, info.Size(), string(data), note), maxToolChars)
}

func runCommand(ctx context.Context, command string) string {
	if strings.TrimSpace(command) == "" {
		return "ERROR: command 不能为空"
	}
	cctx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()

	var cmd *exec.Cmd
	if runtime.GOOS == "windows" {
		cmd = exec.CommandContext(cctx, "cmd", "/C", command)
	} else {
		cmd = exec.CommandContext(cctx, "sh", "-c", command)
	}

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
