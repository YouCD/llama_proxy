package main

import (
	"os"
	"testing"

	"github.com/youcd/toolkit/log"
)

// TestMain 初始化 toolkit 日志：main.go 里 log.Init 只在生产入口调用，
// 测试中调度器 reap 等路径会调用 log.GetLogger()，未初始化会返回 nil 触发
// 空指针 panic。此处按 main.go 相同方式初始化到 stdout。
func TestMain(m *testing.M) {
	log.Init(&log.Config{Stdout: true})
	os.Exit(m.Run())
}
