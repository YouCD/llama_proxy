package scheduler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// modelsServer 启动一个假 OpenAI /v1/models 服务，返回固定的模型 ID 列表。
// 端口即就绪地址端口，供 launchSlot 按端口接管真实监听进程（resolveServerPID）。
func modelsServer(t *testing.T, ids ...string) (baseURL string, cleanup func()) {
	t.Helper()
	body := `{"object":"list","data":[`
	for i, id := range ids {
		if i > 0 {
			body += ","
		}
		body += fmt.Sprintf(`{"id":%q}`, id)
	}
	body += `]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv.URL, func() {}
}

// TestAdoptWithAliasModelField 回归：调度条目 key 与后端服务 ID（alias）不同时，
// 配置了 model 字段即按真实服务 ID 校验身份并接管——不重新加载模型。
// 对应生产场景：多副本 qwen3.8-27b-coder390-8000/-8001 共享 alias
// qwen3.8-27b-coder390，重启 llama_proxy 后应接管而非杀进程重载。
func TestAdoptWithAliasModelField(t *testing.T) {
	baseURL, _ := modelsServer(t, "the-alias")
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"the-model-8000": {
				Mode:         config.ModeBackground,
				Command:      sleepCommand,
				ReadinessURL: baseURL + "/v1/models",
				APIKey:       "k",
				Model:        "the-alias", // 后端实际服务 ID，与 key 不同
				Autostart:    true,
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	ctx := context.Background()

	slot := s.launchSlot(ctx, "the-model-8000")
	s.mu.Lock()
	adopted := slot.cmd == nil && slot.readyOK && slot.serverPID != 0
	s.mu.Unlock()
	if !adopted {
		t.Fatalf("expected adoption (no new process, ready, serverPID set); got cmd=%v readyOK=%v serverPID=%d",
			slot.cmd != nil, slot.readyOK, slot.serverPID)
	}
}

// TestAdoptFailsWithoutModelField 回归对照：未配置 model 字段且 alias 与 key 不同时，
// 身份校验按 key 比对失败，放弃接管、改为启动新进程（即修复前的行为）。
// 该用例证明 model 字段确实是接管成功的关键。
func TestAdoptFailsWithoutModelField(t *testing.T) {
	baseURL, _ := modelsServer(t, "the-alias")
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"the-model-8000": {
				Mode:         config.ModeBackground,
				Command:      sleepCommand,
				ReadinessURL: baseURL + "/v1/models",
				APIKey:       "k",
				// Model 留空：回退为 key "the-model-8000"，与后端 "the-alias" 不符
				Autostart: true,
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	ctx := context.Background()

	slot := s.launchSlot(ctx, "the-model-8000")
	s.mu.Lock()
	started := slot.cmd != nil
	s.mu.Unlock()
	if !started {
		t.Fatal("expected a new process to be started when alias != key and no model field; got adoption instead")
	}
	// 清理刚拉起的 sleep 进程。
	s.mu.Lock()
	sl := s.procs["the-model-8000"]
	s.mu.Unlock()
	if sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		_ = sl.cmd.Process.Kill()
		_, _ = sl.cmd.Process.Wait()
	}
}

// TestStartAdoptsAliasModels 端到端：Start() 加载默认 background 组时，alias 与 key
// 不同（配置 model 字段）的成员被接管而非重新加载——全部就绪且不启动新进程。
func TestStartAdoptsAliasModels(t *testing.T) {
	base8000, _ := modelsServer(t, "shared-alias")
	base8001, _ := modelsServer(t, "shared-alias")
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"shared-alias-8000": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: base8000 + "/v1/models", Model: "shared-alias", Autostart: true},
			"shared-alias-8001": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: base8001 + "/v1/models", Model: "shared-alias", Autostart: true},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	ctx := context.Background()

	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range []string{"shared-alias-8000", "shared-alias-8001"} {
		sl := s.procs[id]
		if sl == nil {
			t.Fatalf("%s slot missing after start", id)
		}
		if sl.cmd != nil {
			t.Fatalf("%s launched a new process, want adoption (no restart)", id)
		}
		if !sl.readyOK {
			t.Fatalf("%s not ready after start", id)
		}
	}
}

// TestAdoptRejectsWrongModel 安全性回归：端口上运行的是另一个模型（服务 ID 与期望
// 不符）时仍拒绝接管，避免误接管恰好占用端口的无关服务。
func TestAdoptRejectsWrongModel(t *testing.T) {
	baseURL, _ := modelsServer(t, "some-other-model")
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"my-model": {
				Mode:         config.ModeBackground,
				Command:      sleepCommand,
				ReadinessURL: baseURL + "/v1/models",
				APIKey:       "k",
				Model:        "my-model", // 期望服务 my-model，端口上却是 some-other-model
				Autostart:    true,
			},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	ctx := context.Background()

	slot := s.launchSlot(ctx, "my-model")
	s.mu.Lock()
	started := slot.cmd != nil
	s.mu.Unlock()
	if !started {
		t.Fatal("expected refusal to adopt a foreign model and a new process start")
	}
	s.mu.Lock()
	sl := s.procs["my-model"]
	s.mu.Unlock()
	if sl != nil && sl.cmd != nil && sl.cmd.Process != nil {
		_ = sl.cmd.Process.Kill()
		_, _ = sl.cmd.Process.Wait()
	}
}
