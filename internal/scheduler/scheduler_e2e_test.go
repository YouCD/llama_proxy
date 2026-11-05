//go:build e2e

package scheduler

// 端到端冒烟测试：用真实 llama-server + 一个小模型验证 no_pool 休眠模型的完整
// 生命周期——加载就绪、闲置休眠（进程真停、显存释放）、按需唤醒重载。
//
// 该测试会真正加载模型（数十秒级），故隔离在 e2e 构建标签后，常规 `go test ./...`
// 不编译、不运行。手动运行：
//
//	go test -tags e2e ./internal/scheduler/ -run TestE2ESleepWakeRealServer -v -timeout 600s
//
// 可用环境变量覆盖默认路径：
//
//	SCHEDULER_E2E_SERVER  llama-server 可执行文件（默认 /usr/local/llama.cpp/llama-server）
//	SCHEDULER_E2E_MODEL   模型 gguf 路径（默认 Hy-MT2-1.8B 小模型）
//	SCHEDULER_E2E_SLEEP   休眠阈值（默认 10s，须短于模型加载完成后的等待窗口）
import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// e2eGetServer 返回 llama-server 路径（找不到则跳过）。
func e2eGetServer(t *testing.T) string {
	t.Helper()
	bin := os.Getenv("SCHEDULER_E2E_SERVER")
	if bin == "" {
		bin = "/usr/local/llama.cpp/llama-server"
	}
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("llama-server 不可用（%s）：%v", bin, err)
	}
	return bin
}

// e2eGetModel 返回模型 gguf 路径（找不到则跳过）。
func e2eGetModel(t *testing.T) string {
	t.Helper()
	model := os.Getenv("SCHEDULER_E2E_MODEL")
	if model == "" {
		model = "/home/ycd/Data/models/ollama/HunYuan/Hy-MT2-1.8B-Q8_0.gguf"
	}
	if _, err := os.Stat(model); err != nil {
		t.Skipf("模型不可用（%s）：%v", model, err)
	}
	return model
}

// verifyModelsServed 对 readiness URL（/v1/models）发 GET，验证进程真正在服务
// 且返回了模型列表（而非仅端口被占）。
func verifyModelsServed(t *testing.T, url string) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s 失败（进程未真正服务）：%v", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s 状态码 %d，期望 200", url, resp.StatusCode)
	}
	var body struct {
		Data   []any `json:"data"`   // OpenAI 兼容格式
		Models []any `json:"models"` // llama.cpp 原生格式
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("解析 %s 响应失败：%v", url, err)
	}
	if len(body.Data) == 0 && len(body.Models) == 0 {
		t.Fatalf("%s 返回空模型列表，模型未加载", url)
	}
}

// verifyPortClosed 验证端口已无监听者（进程已停、显存已释放）。
func verifyPortClosed(t *testing.T, port int) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 500*time.Millisecond)
	if err == nil {
		conn.Close()
		t.Fatalf("端口 %d 仍在监听，进程未真正停止", port)
	}
}

// TestE2ESleepWakeRealServer 验证 no_pool 休眠模型的真实加载→休眠→唤醒生命周期。
func TestE2ESleepWakeRealServer(t *testing.T) {
	serverBin := e2eGetServer(t)
	model := e2eGetModel(t)
	sleepTimeout := 10 * time.Second
	if v := os.Getenv("SCHEDULER_E2E_SLEEP"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			sleepTimeout = d
		}
	}

	port := freePort(t)
	url := readinessURLFor(port)
	logFile := os.Getenv("SCHEDULER_E2E_LOG")
	if logFile == "" {
		logFile = "/tmp/e2e_translator.log"
	}
	// llama.cpp 的共享库（libllama-server-impl.so 等）与二进制同目录，启动时带
	// LD_LIBRARY_PATH，否则子进程报 "cannot open shared object file"。
	bindir := filepath.Dir(serverBin)
	// -ngl 0 强制 CPU 推理：读 /v1/models 不需要推理，避开 GPU 显存被其它模型
	// 占满导致的 OOM，让测试独立于显存可用性。
	cmd := fmt.Sprintf("LD_LIBRARY_PATH=%s %s -m %s --host 127.0.0.1 --port %d -c 4096 --no-webui -ngl 0",
		bindir, serverBin, model, port)
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"translator": {
				Mode:         config.ModeBackground,
				NoPool:       true,
				SleepTimeout: sleepTimeout,
				Command:      cmd,
				ReadinessURL: url,
				LogFile:      logFile,
				Autostart:    true,
			},
		},
	}
	// 真实模型加载较慢，给足启动超时。
	sw := config.SwitchConfig{DrainTimeout: 5 * time.Second, KillTimeout: 15 * time.Second, StartupTimeout: 240 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	defer s.Shutdown()
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start（加载模型）：%v", err)
	}
	// Start 已阻塞到就绪；再确认进程真正在服务（/v1/models 返回模型列表）。
	verifyModelsServed(t, url)
	t.Logf("模型已加载并就绪，端口 %d", port)

	// 跑探活循环（生产行为）：每 probeInterval 探活，闲置超阈值自动休眠，唤醒后
	// 探测到就绪即 close 休眠时换的新 readyCh。
	loopCtx, loopCancel := context.WithCancel(ctx)
	defer loopCancel()
	go s.Loop(loopCtx)

	// 等成员进入休眠（闲置超过 sleepTimeout；markSlotReady 已把闲置时钟锚定在就绪时刻）。
	// 休眠是“置 sleeping → 杀进程 → 清 cmd”的多步操作，需等到进程真正停止（running=false）。
	sleepDeadline := time.Now().Add(sleepTimeout + 2*s.probeInterval + 15*time.Second)
	for {
		s.mu.Lock()
		asleep := s.procs["translator"].sleeping
		running := s.procs["translator"].running()
		s.mu.Unlock()
		if asleep && !running {
			break
		}
		if time.Now().After(sleepDeadline) {
			t.Fatalf("闲置超过阈值后未在合理时间内休眠（sleeping=%v running=%v）", asleep, running)
		}
		time.Sleep(200 * time.Millisecond)
	}
	s.mu.Lock()
	failN := s.failCount["translator"]
	s.mu.Unlock()
	if failN != 0 {
		t.Fatalf("计划内休眠不应记启动失败，got %d", failN)
	}
	verifyPortClosed(t, port)
	t.Logf("已休眠：进程停止、端口 %d 释放", port)

	// 唤醒：再次请求该模型 ID → ensureBackground→restartMember 重新加载；探活循环
	// 探测到就绪后 close 新 readyCh（模型重载约数秒，远小于 240s 上限）。
	ready, err := s.EnsureModel(ctx, "translator")
	if err != nil {
		t.Fatalf("唤醒失败：%v", err)
	}
	select {
	case <-ready:
	case <-time.After(240 * time.Second):
		t.Fatalf("唤醒后 240s 内未就绪；进程日志：%s", logFile)
	}
	verifyModelsServed(t, url)
	s.mu.Lock()
	woke := !s.procs["translator"].sleeping
	s.mu.Unlock()
	if !woke {
		t.Fatal("唤醒后仍标记为休眠")
	}
	t.Logf("已唤醒：重新加载并就绪，端口 %d 重新服务", port)
}
