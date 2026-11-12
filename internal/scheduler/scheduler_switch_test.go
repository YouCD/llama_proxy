package scheduler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// fakeProbe 返回一个就绪探测函数：前 attempts 次失败，之后成功。

func TestEnsureCodingPendingSharesSwitch(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	_ = s.Start(ctx)

	// 让 coding URL 的探针失败，使切换进入 pending 状态。
	s.SetProbe(urlAwareProbe(""))
	c1, _ := s.EnsureModel(ctx, "coding-model")
	c2, _ := s.EnsureModel(ctx, "coding-model")
	if c1 != c2 {
		t.Fatal("concurrent EnsureModel should share the same ready channel")
	}
}

func TestIdleSwitchBackToBackground(t *testing.T) {
	s := newTestScheduler(t, 50*time.Millisecond)
	ctx := context.Background()
	_ = s.Start(ctx)

	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready
	coding, _ := snapshotState(s)
	if coding != "coding-model" {
		t.Fatalf("expected coding model, got %s", coding)
	}

	s.TouchCoding()
	time.Sleep(120 * time.Millisecond)
	bgReady, err := s.EnsureModel(ctx, "bg-model")
	if err != nil {
		t.Fatalf("ensure background after idle: %v", err)
	}
	select {
	case <-bgReady:
	case <-time.After(2 * time.Second):
		t.Fatal("background group did not become ready after idle switch back")
	}
	coding, readyMap := snapshotState(s)
	if coding != "" || !readyMap["bg-model"] {
		t.Fatalf("expected background after idle, got coding=%q ready=%v", coding, readyMap)
	}
}

// TestLoopIdleSwitchBack 验证 Loop 中租约到期能真正触发切回默认 background 组：
// 租约必须大于探活周期（探活 tick 先于租约 timer 触发）。
func TestLoopIdleSwitchBack(t *testing.T) {
	s := newTestScheduler(t, 300*time.Millisecond) // 租约 > 探活周期 50ms
	s.probeInterval = 50 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready

	go s.Loop(ctx)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if coding, ready := snapshotState(s); coding == "" && ready["bg-model"] {
			return
		}
		<-ticker.C
	}
	t.Fatal("Loop did not switch back to background within 3s")
}

func TestCodingActiveStateAfterSwitch(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()
	_ = s.Start(ctx)
	ready, _ := s.EnsureModel(ctx, "coding-model")
	<-ready

	if !s.IsCodingActive() {
		t.Fatal("should be coding active")
	}
}

// TestEnsureBackground 验证 EnsureModel 在当前已是就绪组成员时返回已关闭的就绪
// channel，且不影响当前状态。

func TestCodingPreemptsBackgroundStartup(t *testing.T) {
	codingURL := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"coding-model": {Mode: config.ModeCoding, Command: sleepCommand, ReadinessURL: codingURL, APIKey: "coding-key"},
			"bg-model":     {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "bg-key", Autostart: true},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 3 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(urlAwareProbe(urlPortMarker(codingURL))) // background 永不就绪，coding 立即就绪
	ctx := context.Background()

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(ctx) }()

	// 等 background 组切换进入就绪等待（模拟大模型仍在加载）。
	waitSwitchInflight(t, s, "group")

	// coding 请求到达：必须插队抢占，而不是等 background 加载完（3s）。
	t0 := time.Now()
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed promptly after preemption")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel took %v; preemption should not wait out the 3s background load", d)
	}

	coding, readyMap := snapshotState(s)
	if coding != "coding-model" || !readyMap["coding-model"] {
		t.Fatalf("expected coding ready, got coding=%q ready=%v", coding, readyMap)
	}
	if got := s.APIKeyFor("coding-model"); got != "coding-key" {
		t.Fatalf("APIKeyFor=%q, want coding-key", got)
	}

	// Start 必须把"首次默认 background 组加载被抢占"视为非致命并返回 nil。
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start should not fail when initial background group load is preempted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after preemption")
	}
}

// TestOnDemandBackgroundPreemptsInitialLoad 验证：Start 初始加载期间，手动切换
// 到按需 background 模型应直接抢占初始加载的就绪等待（而不是排队等整个初始
// 加载完成）。被取消的只是等待，组内其余成员继续加载（探活循环接管），
// Start 对抢占豁免 FATAL。
func TestOnDemandBackgroundPreemptsInitialLoad(t *testing.T) {
	ondemandURL := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"bg-model":       {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: readinessURLFor(freePort(t)), APIKey: "bg-key", Autostart: true},
			"ondemand-model": {Mode: config.ModeBackground, Command: sleepCommand, ReadinessURL: ondemandURL, APIKey: "od-key"},
		},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 3 * time.Second}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(urlAwareProbe(urlPortMarker(ondemandURL))) // on-demand 立即就绪，background 永不就绪
	ctx := context.Background()

	startDone := make(chan error, 1)
	go func() { startDone <- s.Start(ctx) }()

	// 等 background 组切换进入就绪等待（模拟大模型仍在加载）。
	waitSwitchInflight(t, s, "group")

	// 按需 background 请求到达：必须插队抢占初始加载，而不是等 3s background 加载完。
	t0 := time.Now()
	ready, err := s.EnsureModel(ctx, "ondemand-model")
	if err != nil {
		t.Fatalf("ensure on-demand: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("on-demand ready channel not closed promptly after preemption")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel took %v; preemption should not wait out the 3s background load", d)
	}

	// Start 必须把"首次默认 background 组加载被抢占"视为非致命并返回 nil。
	select {
	case err := <-startDone:
		if err != nil {
			t.Fatalf("Start should not fail when initial background group load is preempted: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after preemption")
	}

	// 组内其余成员未被杀，仍在加载（由探活循环接管就绪）。
	s.mu.Lock()
	sl := s.procs["bg-model"]
	alive := sl != nil && sl.running()
	s.mu.Unlock()
	if !alive {
		t.Fatal("background member should keep loading after preemption (reconcile takes over)")
	}
}

// TestCodingPreemptsIdleSwitchBack 验证：运行期空闲切回 background 组的切换在加载
// 期间被新的 coding 请求抢占，background 切换应快速以"被抢占"错误返回，
// coding 切换随即完成。
func TestCodingPreemptsIdleSwitchBack(t *testing.T) {
	// 短租约：空闲切回 background 只在租约过期后才发生（与 Loop 的触发条件一致）。
	s := newTestScheduler(t, 50*time.Millisecond)
	ctx := context.Background()

	// 先让 coding 就绪。
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("initial coding did not become ready")
	}
	coding, readyMap := snapshotState(s)
	if coding != "coding-model" || !readyMap["coding-model"] {
		t.Fatalf("expected coding ready, got coding=%q ready=%v", coding, readyMap)
	}

	// 等 coding 租约过期，空闲切回 background 才合法（否则会被租约保护跳过）。
	time.Sleep(120 * time.Millisecond)

	// background 永不就绪（模拟大模型加载慢）。
	s.SetProbe(urlAwareProbe(urlPortMarker(s.cfg.Entry("coding-model").ReadinessURL)))

	bgErrCh := make(chan error, 1)
	go func() {
		_, e := s.EnsureModel(ctx, "bg-model")
		bgErrCh <- e
	}()
	waitSwitchInflight(t, s, "group")

	// coding 请求到达，抢占在途的 background 组切换。
	t0 := time.Now()
	ready2, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready2:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed promptly after preemption")
	}
	if d := time.Since(t0); d > 2*time.Second {
		t.Errorf("EnsureModel took %v; preemption should cut the background load short", d)
	}

	select {
	case bgErr := <-bgErrCh:
		if bgErr == nil || !strings.Contains(bgErr.Error(), "preempted") {
			t.Errorf("background switch should report preemption, got: %v", bgErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("background switch did not return after preemption")
	}

	coding, readyMap = snapshotState(s)
	if coding != "coding-model" || !readyMap["coding-model"] {
		t.Fatalf("expected coding ready, got coding=%q ready=%v", coding, readyMap)
	}
	if got := s.APIKeyFor("coding-model"); got != "coding-key" {
		t.Fatalf("APIKeyFor=%q, want coding-key", got)
	}
}

// TestStartThenCodingReadyClosed 回归：background 组先完成就绪并关闭各自
// readyCh 后，首个 coding 请求再触发切换，coding 的 channel 必须被关闭，
// 不得被旧的关闭标志吞掉。
func TestStartThenCodingReadyClosed(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	ctx := context.Background()

	var probeMu sync.Mutex
	probeOK := false
	s.SetProbe(func(context.Context, string) bool {
		probeMu.Lock()
		defer probeMu.Unlock()
		return probeOK
	})

	// 启动 background 组切换（模拟 Start），初始探针失败使其等待。
	bgDone := make(chan struct{})
	go func() {
		defer close(bgDone)
		_, _ = s.EnsureModel(ctx, "bg-model")
	}()
	time.Sleep(50 * time.Millisecond)

	// 让 background 就绪并等待其切换完成。
	probeMu.Lock()
	probeOK = true
	probeMu.Unlock()
	<-bgDone

	coding, readyMap := snapshotState(s)
	if coding != "" || !readyMap["bg-model"] {
		t.Fatalf("expected background ready, got coding=%q ready=%v", coding, readyMap)
	}

	// 现在触发 coding 切换：channel 必须被关闭，不得永久阻塞。
	ready, err := s.EnsureModel(ctx, "coding-model")
	if err != nil {
		t.Fatalf("ensure coding: %v", err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("coding ready channel not closed after background switch")
	}

	coding, readyMap = snapshotState(s)
	if coding != "coding-model" || !readyMap["coding-model"] {
		t.Fatalf("expected coding ready, got coding=%q ready=%v", coding, readyMap)
	}
}

// TestStartCancelledCtxNotFatal 模拟 Ctrl+C：Start 被已取消的 ctx 调用时，首次
// background 组加载被打断（Shutdown 已杀掉正在加载的进程，waitReady 返回
// context.Canceled），不得报致命错误——main.go 对 Start 的任何错误都会 Fatalf。
func TestStartCancelledCtxNotFatal(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	defer s.Shutdown()
	s.SetProbe(func(context.Context, string) bool { return false }) // 永不就绪：waitReady 只能经 ctx 取消结束
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("Start with cancelled ctx must not be fatal, got: %v", err)
	}
}

// TestSwitchToCancelledCtxNotPreempted 验证调用方 ctx 取消（优雅关停）不被误标为
// "被更高优先级请求抢占"：s.preempted 只由 preemptLowerPriority 置位，关停打断
// 属于"就绪探测失败（context canceled）"而非抢占。
func TestSwitchToCancelledCtxNotPreempted(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	defer s.Shutdown()
	s.SetProbe(func(context.Context, string) bool { return false }) // 永不就绪：waitReady 只能经 ctx 取消结束
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := s.switchTo(ctx, []string{"bg-model"}, config.ModeBackground)
	if err == nil {
		t.Fatal("expected error from cancelled switchTo")
	}
	if strings.Contains(err.Error(), "preempted") {
		t.Fatalf("cancelled switch mislabeled as preempted: %v", err)
	}
}
