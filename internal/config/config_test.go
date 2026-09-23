package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadYAMLConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  listen_addr: ":9999"
  data_dir: "/tmp/monitor"

database:
  type: "postgresql"
  postgresql:
    dsn: "postgres://user:pass@localhost:5432/db"

backends:
  strategy: "wrr"
  list:
    - name: "backend-1"
      url: "http://backend-1:8080"
      weight: 50
      enabled: true
    - name: "backend-2"
      url: "http://backend-2:8080"
      weight: 30
      enabled: true
    - name: "backend-3"
      url: "http://backend-3:8080"
      weight: 20
      enabled: true

proxy:
  retention_days: 30
  max_request_bytes: 1048576
  max_capture_bytes: 1048576
  request_timeout_seconds: 300
  poll_backend_metrics: false
  poll_interval_seconds: 30
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if cfg.Server.ListenAddr != ":9999" {
		t.Fatalf("listen_addr=%q", cfg.Server.ListenAddr)
	}
	if cfg.Database.Type != "postgresql" {
		t.Fatalf("db type=%q", cfg.Database.Type)
	}
	if cfg.Database.PostgreSQL.DSN != "postgres://user:pass@localhost:5432/db" {
		t.Fatalf("dsn=%q", cfg.Database.PostgreSQL.DSN)
	}
	if cfg.Backends.Strategy != "wrr" {
		t.Fatalf("strategy=%q", cfg.Backends.Strategy)
	}
	if len(cfg.Backends.List) != 3 {
		t.Fatalf("backends=%d", len(cfg.Backends.List))
	}
	if cfg.Proxy.RetentionDays != 30 {
		t.Fatalf("retention_days=%d", cfg.Proxy.RetentionDays)
	}
	if cfg.Proxy.PollBackendMetrics != nil && *cfg.Proxy.PollBackendMetrics {
		t.Fatal("poll_backend_metrics should be false")
	}

	legacy := cfg.ToLegacy()
	if legacy.ListenAddr != ":9999" {
		t.Fatalf("listen=%q", legacy.ListenAddr)
	}
	if legacy.RequestTimeout.Seconds() != 300 {
		t.Fatalf("timeout=%v", legacy.RequestTimeout)
	}
}

func TestLoadYAMLConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `server: {}
database: {}
backends: {}
proxy: {}
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if cfg.Server.ListenAddr != ":9091" {
		t.Fatalf("listen_addr=%q", cfg.Server.ListenAddr)
	}
	if cfg.Database.Type != "sqlite" {
		t.Fatalf("db type=%q", cfg.Database.Type)
	}
	if cfg.Backends.Strategy != "wrr" {
		t.Fatalf("strategy=%q", cfg.Backends.Strategy)
	}
	if cfg.Proxy.RetentionDays != 14 {
		t.Fatalf("retention_days=%d", cfg.Proxy.RetentionDays)
	}
	if !(cfg.Proxy.PollBackendMetrics != nil && *cfg.Proxy.PollBackendMetrics) {
		t.Fatalf("expected poll_backend_metrics=true")
	}
	if cfg.Proxy.PollInterval != 10 {
		t.Fatalf("poll_interval=%d", cfg.Proxy.PollInterval)
	}
}

func TestLoadYAMLConfigScheduling(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server:
  listen_addr: ":9091"
database:
  type: "sqlite"
  sqlite:
    path: "proxy.db"
backends:
  list:
    - name: "ext"
      url: "http://ext:8080"
      enabled: true
proxy: {}
scheduling:
  coding:
    model: "qwen3.8"
    command: "llama-server -m coding.gguf --port 8080"
    readiness_url: "http://127.0.0.1:8080"
  background:
    model: "qwen3.6"
    command: "llama-server -m bg.gguf --port 8080"
    readiness_url: "http://127.0.0.1:8080"
  lease:
    coding_idle_timeout: 45m
  switch:
    drain_timeout: 15s
    kill_timeout: 20s
    startup_timeout: 200s
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}

	if !cfg.HasScheduling() {
		t.Fatal("hasScheduling should be true")
	}
	if cfg.Scheduling.Coding.Command != "llama-server -m coding.gguf --port 8080" {
		t.Fatalf("coding command=%q", cfg.Scheduling.Coding.Command)
	}
	if cfg.Scheduling.Coding.Model != "qwen3.8" {
		t.Fatalf("coding model=%q", cfg.Scheduling.Coding.Model)
	}
	if cfg.Scheduling.Background.Model != "qwen3.6" {
		t.Fatalf("background model=%q", cfg.Scheduling.Background.Model)
	}
	if cfg.Scheduling.Background.ReadinessURL != "http://127.0.0.1:8080" {
		t.Fatalf("bg readiness=%q", cfg.Scheduling.Background.ReadinessURL)
	}
	if cfg.Scheduling.Lease.CodingIdleTimeout != 45*time.Minute {
		t.Fatalf("idle timeout=%v", cfg.Scheduling.Lease.CodingIdleTimeout)
	}
	if cfg.Scheduling.Switch.DrainTimeout != 15*time.Second || cfg.Scheduling.Switch.KillTimeout != 20*time.Second || cfg.Scheduling.Switch.StartupTimeout != 200*time.Second {
		t.Fatalf("switch=%+v", cfg.Scheduling.Switch)
	}
}

func TestLoadYAMLConfigNoSchedulingDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "server: {}\ndatabase: {}\nbackends: {}\nproxy: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}
	if cfg.HasScheduling() {
		t.Fatal("hasScheduling should be false without scheduling config")
	}
	if cfg.Scheduling != nil {
		t.Fatal("scheduling should be nil without scheduling config")
	}
}

func TestLoadYAMLConfigSchedulingDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := `
server: {}
database: {}
backends: {}
proxy: {}
scheduling:
  coding:
    model: "coding-model"
    command: "llama-server -m coding.gguf"
    readiness_url: "http://127.0.0.1:8080"
  background:
    model: "bg-model"
    command: "llama-server -m bg.gguf"
    readiness_url: "http://127.0.0.1:8080"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}
	if !cfg.HasScheduling() {
		t.Fatal("hasScheduling should be true")
	}
	if cfg.Scheduling.Coding.Model != "coding-model" || cfg.Scheduling.Background.Model != "bg-model" {
		t.Fatalf("models=%q/%q", cfg.Scheduling.Coding.Model, cfg.Scheduling.Background.Model)
	}
	if cfg.Scheduling.Lease.CodingIdleTimeout != 30*time.Minute {
		t.Fatalf("default idle timeout=%v", cfg.Scheduling.Lease.CodingIdleTimeout)
	}
	if cfg.Scheduling.Switch.DrainTimeout != 10*time.Second || cfg.Scheduling.Switch.KillTimeout != 10*time.Second || cfg.Scheduling.Switch.StartupTimeout != 120*time.Second {
		t.Fatalf("default switch=%+v", cfg.Scheduling.Switch)
	}
}

// LoadYAMLForTest 把 content 写入临时文件并调用 Load，返回加载错误（nil 表示成功）。
func LoadYAMLForTest(t *testing.T, content string) error {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	_, err := Load(path)
	return err
}

// TestLoadYAMLConfigSchedulingValidation 验证调度启动校验：配置了 command 但缺
// model / readiness_url 时报错；模型 ID 与 backends.list 重复或与 llm_prox 冲突时报错。
func TestLoadYAMLConfigSchedulingValidation(t *testing.T) {
	writeAndLoad := func(content string) error {
		return LoadYAMLForTest(t, content)
	}

	// 缺 model
	err := writeAndLoad(`
scheduling:
  coding:
    command: "llama-server -m coding.gguf"
    readiness_url: "http://127.0.0.1:8080"
  background:
    model: "bg-model"
    command: "llama-server -m bg.gguf"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "model") {
		t.Fatalf("expected missing-model error, got %v", err)
	}

	// 缺 readiness_url
	err = writeAndLoad(`
scheduling:
  coding:
    model: "coding-model"
    command: "llama-server -m coding.gguf"
  background:
    model: "bg-model"
    command: "llama-server -m bg.gguf"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "readiness_url") {
		t.Fatalf("expected missing readiness_url error, got %v", err)
	}

	// 模型 ID 与 backends.list 重复
	err = writeAndLoad(`
backends:
  list:
    - name: "gpu"
      url: "http://gpu:8080"
      model: "qwen3.8"
scheduling:
  coding:
    model: "qwen3.8"
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
  background:
    model: "bg-model"
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "qwen3.8") || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("expected duplicate model id error, got %v", err)
	}

	// 模型 ID 与代理占位 ID 冲突
	err = writeAndLoad(`
scheduling:
  coding:
    model: "llm_prox"
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
  background:
    model: "bg-model"
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "llm_prox") {
		t.Fatalf("expected proxy-id conflict error, got %v", err)
	}
}

// TestLoadYAMLConfigRouting 验证 routing 规则解析与校验：合法配置正常加载，
// pool 为空、header 无条件或残留旧字段（match/user_agent_contains）时报错。
func TestLoadYAMLConfigRouting(t *testing.T) {
	dir := t.TempDir()

	writeAndLoad := func(t *testing.T, content string) (*YAMLConfig, error) {
		t.Helper()
		path := filepath.Join(dir, "config.yaml")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		return Load(path)
	}

	content := `
server:
  listen_addr: ":9999"
  data_dir: "/tmp/monitor"
backends:
  list:
    - name: "b1"
      url: "http://b1:8080"
      weight: 1
      tags: ["fast", "tool_call"]
routing:
  rules:
    - name: "goclaw"
      header:
        User-Agent: "GoClaw/2.1"
      pool: "tool_call"
    - name: "agent-x"
      header:
        User-Agent: "AgentX/1.0"
        X-Agent-Env: "prod"
      pool: "fast"
`
	cfg, err := writeAndLoad(t, content)
	if err != nil {
		t.Fatalf("load yaml config: %v", err)
	}
	if cfg.Routing == nil || len(cfg.Routing.Rules) != 2 {
		t.Fatalf("routing rules=%+v, want 2", cfg.Routing)
	}
	r0 := cfg.Routing.Rules[0]
	if r0.Name != "goclaw" || r0.Pool != "tool_call" || r0.Header["User-Agent"] != "GoClaw/2.1" {
		t.Fatalf("rule[0]=%+v", r0)
	}
	r1 := cfg.Routing.Rules[1]
	if r1.Pool != "fast" || r1.Header["User-Agent"] != "AgentX/1.0" || r1.Header["X-Agent-Env"] != "prod" {
		t.Fatalf("rule[1]=%+v", r1)
	}
	// 后端有效标签集
	tags := cfg.Backends.List[0].EffectiveTags()
	if !tags["tool_call"] || !tags["fast"] {
		t.Fatalf("effective tags=%v, want tool_call and fast", tags)
	}

	// pool 为空
	if _, err := writeAndLoad(t, "routing:\n  rules:\n    - header:\n        User-Agent: \"X\"\n"); err == nil {
		t.Fatal("expected error for empty pool")
	}
	// header 无条件
	if _, err := writeAndLoad(t, "routing:\n  rules:\n    - pool: \"fast\"\n"); err == nil {
		t.Fatal("expected error for empty header")
	}
	// 旧字段 match/user_agent_contains 不再支持，应给出迁移提示
	if _, err := writeAndLoad(t, "routing:\n  rules:\n    - match:\n        user_agent_contains: \"X\"\n      pool: \"fast\"\n"); err == nil {
		t.Fatal("expected error for legacy match field")
	}
}
