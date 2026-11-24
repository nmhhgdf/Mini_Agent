//go:build !windows

package main

import (
	"context"
	"os/exec"
)

// shellCommand 在非 Windows 平台用 sh -c 执行。
func shellCommand(ctx context.Context, command string) *exec.Cmd {
	return exec.CommandContext(ctx, "sh", "-c", command)
}
