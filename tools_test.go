package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func skipWithoutBigFile(t *testing.T) {
	t.Helper()
	if _, err := os.Stat("big-file.txt"); err != nil {
		t.Skip("big-file.txt 未生成，跳过测试")
	}
}

func TestReadFilePaging(t *testing.T) {
	skipWithoutBigFile(t)

	tail := readFile("big-file.txt", 3995, 20)
	if !strings.Contains(tail, "FINAL: deployment completed revision=abc123") {
		t.Fatalf("tail missing FINAL:\n%s", tail)
	}
	if !strings.Contains(tail, "total_lines: 4001") {
		t.Fatalf("tail missing total_lines:\n%s", tail)
	}

	head := readFile("big-file.txt", 1, 5)
	if !strings.Contains(head, "showing 1-5") || !strings.Contains(head, "next_offset: 6") {
		t.Fatalf("head header wrong:\n%s", head)
	}

	bad := readFile("big-file.txt", 99999, 10)
	if !strings.Contains(bad, "ERROR") || !strings.Contains(bad, "4001") {
		t.Fatalf("out-of-range message wrong:\n%s", bad)
	}
}

func TestRunCommandQuotes(t *testing.T) {
	skipWithoutBigFile(t)

	out := runCommand(context.Background(), `findstr /n /c:"revision=abc123" big-file.txt`)
	if !strings.Contains(out, "4001") {
		t.Fatalf("findstr through runCommand failed:\n%s", out)
	}
}

func TestGrep(t *testing.T) {
	skipWithoutBigFile(t)

	out := grepFiles(`revision=abc123`, ".", 5)
	if !strings.Contains(out, "big-file.txt:4001") {
		t.Fatalf("grep did not find the line:\n%s", out)
	}

	stats := grepFiles(`ERROR`, "big-file.txt", 5)
	if !strings.Contains(stats, "matches=666") || !strings.Contains(stats, "仅显示前 5 条") {
		t.Fatalf("grep total count wrong:\n%s", stats)
	}
}
