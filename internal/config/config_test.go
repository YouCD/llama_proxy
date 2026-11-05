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
	if cfg.Proxy.RetentionDays == nil || *cfg.Proxy.RetentionDays != 30 {
		t.Fatalf("retention_days=%v", cfg.Proxy.RetentionDays)
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
	if cfg.Proxy.RetentionDays == nil || *cfg.Proxy.RetentionDays != 14 {
		t.Fatalf("retention_days=%v", cfg.Proxy.RetentionDays)
	}
}

// TestLoadYAMLConfigRetentionZero 验证显式 retention_days: 0 表示永久保留（不被默认值 14 覆盖）。
func TestLoadYAMLConfigRetentionZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if werr := os.WriteFile(path, []byte("server: {}\ndatabase: {}\nbackends: {}\nproxy:\n  retention_days: 0\n"), 0o644); werr != nil {
		t.Fatalf("write config: %v", werr)
	}
	cfg, lerr := Load(path)
	if lerr != nil {
		t.Fatalf("load: %v", lerr)
	}
	if cfg.Proxy.RetentionDays == nil || *cfg.Proxy.RetentionDays != 0 {
		t.Fatalf("retention_days=%v, want 0 (permanent)", cfg.Proxy.RetentionDays)
	}
	if legacy := cfg.ToLegacy(); legacy.RetentionDays != 0 {
		t.Fatalf("legacy retention_days=%d, want 0", legacy.RetentionDays)
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
  qwen3.8:
    mode: coding
    command: "llama-server -m coding.gguf --port 8080"
    readiness_url: "http://127.0.0.1:8080"
  qwen3.6:
    mode: background
    command: "llama-server -m bg.gguf --port 8080"
    readiness_url: "http://127.0.0.1:8080"
    autostart: true
    weight: 3
    tags: ["tool_call"]
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
	coding := cfg.Scheduling.Entry("qwen3.8")
	if coding == nil || coding.Mode != ModeCoding || coding.Command != "llama-server -m coding.gguf --port 8080" {
		t.Fatalf("coding entry=%+v", coding)
	}
	bg := cfg.Scheduling.Entry("qwen3.6")
	if bg == nil || bg.Mode != ModeBackground || bg.ReadinessURL != "http://127.0.0.1:8080" || bg.Weight != 3 || !bg.EffectiveTags()["tool_call"] {
		t.Fatalf("background entry=%+v", bg)
	}
	group := cfg.Scheduling.DefaultBackgroundGroup()
	if len(group) != 1 || group[0] != "qwen3.6" {
		t.Fatalf("autostart group=%v, want [qwen3.6]", group)
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
  coding-model:
    mode: coding
    command: "llama-server -m coding.gguf"
    readiness_url: "http://127.0.0.1:8080"
  bg-model:
    mode: background
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
	if cfg.Scheduling.Entry("coding-model") == nil || cfg.Scheduling.Entry("bg-model") == nil {
		t.Fatalf("entries=%v", cfg.Scheduling.Models)
	}
	if e := cfg.Scheduling.Entry("bg-model"); e.Weight != 1 {
		t.Fatalf("default weight=%d, want 1", e.Weight)
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

// TestLoadYAMLConfigSchedulingValidation 验证调度启动校验：缺 mode / 非法 mode /
// 缺 readiness_url 时报错；模型 ID（key）与 backends.list 重复或与 llama_proxy
// 冲突时报错；autostart 配在 coding 条目上时报错；保留 key 用作模型 ID 时报错；
// gpu 仲裁（resident 独占、启动集内同卡）报错。
func TestLoadYAMLConfigSchedulingValidation(t *testing.T) {
	writeAndLoad := func(content string) error {
		return LoadYAMLForTest(t, content)
	}

	// 缺 mode
	err := writeAndLoad(`
scheduling:
  coding-model:
    command: "llama-server -m coding.gguf"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("expected missing-mode error, got %v", err)
	}

	// 非法 mode
	err = writeAndLoad(`
scheduling:
  coding-model:
    mode: "hybrid"
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "mode") {
		t.Fatalf("expected invalid-mode error, got %v", err)
	}

	// 缺 readiness_url
	err = writeAndLoad(`
scheduling:
  coding-model:
    mode: coding
    command: "llama-server -m coding.gguf"
`)
	if err == nil || !strings.Contains(err.Error(), "readiness_url") {
		t.Fatalf("expected missing readiness_url error, got %v", err)
	}

	// 模型 ID（key）与 backends.list 重复
	err = writeAndLoad(`
backends:
  list:
    - name: "gpu"
      url: "http://gpu:8080"
      model: "qwen3.8"
scheduling:
  qwen3.8:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "qwen3.8") || !strings.Contains(err.Error(), "重复") {
		t.Fatalf("expected duplicate model id error, got %v", err)
	}

	// 模型 ID（key）与代理占位 ID 冲突
	err = writeAndLoad(`
scheduling:
  llama_proxy:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "llama_proxy") {
		t.Fatalf("expected proxy-id conflict error, got %v", err)
	}

	// 两个 background 均未 autostart：合法（全部按需加载，启动时不拉起任何进程）
	if err := writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
`); err != nil {
		t.Fatalf("all-on-demand background should load, got %v", err)
	}

	// 多个 background 列入 autostart 启动集：合法，且多个 coding 也合法。
	// background 成员并行常驻必须各自不同端口；coding 互斥运行，可共用同一端口，
	// 也可与 background 成员共用端口（coding 运行期间 background 组已停止）。
	content := `
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    autostart: true
  code-a:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
  code-b:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`
	if err := writeAndLoad(content); err != nil {
		t.Fatalf("multi coding/background with autostart should load, got %v", err)
	}

	// coding 条目与 backends.list 同 origin：报错（coding 运行时后端仍在服务）
	err = writeAndLoad(`
backends:
  list:
    - name: "local"
      url: "http://127.0.0.1:8080/v1"
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    autostart: true
  code-a:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080/v1/models"
`)
	if err == nil || !strings.Contains(err.Error(), "端口冲突") {
		t.Fatalf("expected coding/backend port collision, got %v", err)
	}

	// 非 autostart background 不在启动集：合法（按需加载，启动时不拉起）
	if err := writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
`); err != nil {
		t.Fatalf("non-autostart background should load (on-demand), got %v", err)
	}

	// autostart 配在 coding 模型上：报错（coding 永远按需）
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
  code-a:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    autostart: true
`)
	if err == nil || !strings.Contains(err.Error(), "autostart") {
		t.Fatalf("expected autostart-on-coding error, got %v", err)
	}

	// 保留 key 用作模型 ID
	err = writeAndLoad(`
scheduling:
  lease:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "保留 key") {
		t.Fatalf("expected reserved-key error, got %v", err)
	}

	// sleep_timeout 配在 coding 模型上：报错（仅支持 background）
	err = writeAndLoad(`
scheduling:
  code-a:
    mode: coding
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    sleep_timeout: 1h
`)
	if err == nil || !strings.Contains(err.Error(), "sleep_timeout 仅支持 background") {
		t.Fatalf("expected sleep_timeout-on-coding error, got %v", err)
	}

	// sleep_timeout 配在 background 但缺 no_pool：报错（池内成员闲置判定失真）
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    sleep_timeout: 1h
    autostart: true
`)
	if err == nil || !strings.Contains(err.Error(), "no_pool") {
		t.Fatalf("expected sleep_timeout-without-no_pool error, got %v", err)
	}

	// sleep_timeout 配在 no_pool background 上：合法（专用模型闲置休眠）
	if err := writeAndLoad(`
scheduling:
  trans:
    mode: background
    no_pool: true
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    sleep_timeout: 30m
    autostart: true
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    autostart: true
`); err != nil {
		t.Fatalf("sleep_timeout with no_pool background should load, got %v", err)
	}

	// gpu 被 resident 独占：其他模型声明同一 gpu 报错
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    resident: true
    gpu: "CUDA0"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    gpu: "CUDA0"
`)
	if err == nil || !strings.Contains(err.Error(), "独占") {
		t.Fatalf("expected gpu-resident-pin error, got %v", err)
	}

	// 两个 resident 声明同一 gpu：报错（bg-b 在组外 = 按需常驻，同样受 resident 独占约束）
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    resident: true
    gpu: "CUDA1"
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    resident: true
    gpu: "cuda1"
    autostart: true
`)
	if err == nil || !strings.Contains(err.Error(), "独占") {
		t.Fatalf("expected two-residents-same-gpu error, got %v", err)
	}

	// 启动集（autostart）内两个成员声明同一 gpu：报错（启动时并行加载会抢同一张卡）
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    gpu: "CUDA1"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    gpu: "CUDA1"
    autostart: true
`)
	if err == nil || !strings.Contains(err.Error(), "同一张卡") {
		t.Fatalf("expected startup-same-gpu error, got %v", err)
	}

	// autostart 成员与按需成员声明同一 gpu：合法（运行时互斥换入）
	if err := writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    gpu: "CUDA1"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8081"
    gpu: "CUDA1"
`); err != nil {
		t.Fatalf("autostart/on-demand gpu sharing should load (runtime swap), got %v", err)
	}

	// 同 gpu 非 resident 模型共用端口：合法（运行时互斥，换入时先杀旧监听者）
	if err := writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    gpu: "CUDA1"
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    gpu: "CUDA1"
    autostart: true
`); err != nil {
		t.Fatalf("same-gpu non-resident port sharing should load, got %v", err)
	}

	// 同 gpu 但一方 resident：共存——该组合已被 validateScheduling 的 resident 独占
	// 规则提前拦截（gpu 独占校验先于端口校验），不会走到端口冲突分支。

	// GPU 模型与无 GPU 模型共用端口：共存，端口冲突
	err = writeAndLoad(`
scheduling:
  bg-a:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
    gpu: "CUDA1"
    autostart: true
  bg-b:
    mode: background
    command: "llama-server"
    readiness_url: "http://127.0.0.1:8080"
`)
	if err == nil || !strings.Contains(err.Error(), "端口冲突") {
		t.Fatalf("expected gpu/cpu port collision, got %v", err)
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
