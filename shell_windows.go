//go:build windows

package main

import (
	"context"
	"os/exec"
	"syscall"
)

// shellCommand 用 cmd /C 执行命令。SysProcAttr.CmdLine 让命令行原样交给
// cmd.exe，避免 Go 的参数转义把命令里的引号变成 \"（findstr 会因此匹配失败）；
// 同时把代码页切到 UTF-8，减少中文 Windows 下 dir/findstr 输出的乱码。
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "cmd")
	cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: `/C chcp 65001 >nul & ` + command}
	return cmd
}
