package scheduler

import (
	"context"
	"net/http"
	"testing"
	"time"

	"llama_proxy/internal/config"
)

// newSleepTestScheduler 构造单个 no_pool background 模型，配置极短 sleep_timeout，
// 用于驱动休眠判定。就绪端口随机，避免与本机其他服务撞车。
func newSleepTestScheduler(t *testing.T, timeout time.Duration) *Scheduler {
	t.Helper()
	url := readinessURLFor(freePort(t))
	cfg := &config.SchedulingConfig{
		Models: map[string]config.ProcessConfig{
			"sleepy": {
				Mode:         config.ModeBackground,
				NoPool:       true,
				SleepTimeout: timeout,
				Command:      sleepCommand,
				ReadinessURL: url,
			},
		},
		Default: []string{"sleepy"},
	}
	sw := config.SwitchConfig{DrainTimeout: time.Millisecond, KillTimeout: time.Millisecond, StartupTimeout: 500 * time.Millisecond}
	s := New(cfg, time.Hour, sw, &http.Client{})
	s.SetProbe(fakeProbe(0)) // 立即就绪
	return s
}

// TestSleepIdleNoPoolModel 验证：no_pool 成员闲置超过 sleep_timeout 后，reconcile
// 主动停其进程（休眠）；休眠不记启动失败、不退避。
func TestSleepIdleNoPoolModel(t *testing.T) {
	s := newSleepTestScheduler(t, 10*time.Millisecond)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.mu.Lock()
	sl := s.procs["sleepy"]
	wasReady, runningBefore := sl.readyOK, sl.running()
	s.mu.Unlock()
	if !wasReady || !runningBefore {
		t.Fatalf("expected sleepy ready and running, got ready=%v running=%v", wasReady, runningBefore)
	}

	// 等待超过 sleep_timeout：markSlotReady 已锚定 lastActiveAt 为就绪时刻，此刻
	// 距其超过阈值 → 判定闲置。
	time.Sleep(30 * time.Millisecond)
	s.reconcile(ctx)

	s.mu.Lock()
	asleep, stillRunning, failN := sl.sleeping, sl.running(), s.failCount["sleepy"]
	s.mu.Unlock()
	if !asleep {
		t.Fatal("expected sleepy to be sleeping after idle reconcile")
	}
	if stillRunning {
		t.Fatal("expected sleepy process stopped after sleep")
	}
	if failN != 0 {
		t.Fatalf("expected no startup failure on intentional sleep, got %d", failN)
	}
}

// TestSleepWakeRoundTrip 验证：休眠成员被直接请求唤醒后重新就绪、脱离休眠态，且
// 复用原 readyCh（不 panic、不搁浅等待者）。
func TestSleepWakeRoundTrip(t *testing.T) {
	s := newSleepTestScheduler(t, 10*time.Millisecond)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	s.reconcile(ctx) // 休眠

	s.mu.Lock()
	asleep := s.procs["sleepy"].sleeping
	s.mu.Unlock()
	if !asleep {
		t.Fatal("precondition: sleepy should be sleeping")
	}

	ready, err := s.EnsureModel(ctx, "sleepy")
	if err != nil {
		t.Fatalf("wake sleepy: %v", err)
	}
	// 唤醒后 restartMember 已重新拉起进程并清休眠态；就绪探测交给探活循环。
	// fakeProbe 恒就绪，驱动一轮 reconcile 即 markSlotReady 关闭休眠时换的新 channel。
	s.reconcile(ctx)
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("sleepy did not become ready after wake")
	}
	s.mu.Lock()
	woke := !s.procs["sleepy"].sleeping
	s.mu.Unlock()
	if !woke {
		t.Fatal("expected sleepy to no longer be sleeping after wake")
	}
}

// TestSleepSkippedWhileInflight 验证：有在途请求（ModelEnter）时不休眠，避免停掉
// 仍有人使用的进程。
func TestSleepSkippedWhileInflight(t *testing.T) {
	s := newSleepTestScheduler(t, 10*time.Millisecond)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	s.ModelEnter("sleepy") // 模拟一个在途请求
	s.reconcile(ctx)

	s.mu.Lock()
	asleep := s.procs["sleepy"].sleeping
	s.mu.Unlock()
	if asleep {
		t.Fatal("expected sleepy NOT to sleep while a request is in flight")
	}
	s.ModelLeave("sleepy")
}

// TestSleepSkippedWhenRecentlyActive 验证：最近被请求过（TouchModel）的成员不满足
// 闲置条件，不休眠。
func TestSleepSkippedWhenRecentlyActive(t *testing.T) {
	s := newSleepTestScheduler(t, 10*time.Millisecond)
	ctx := context.Background()
	if err := s.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	s.TouchModel("sleepy") // 刚刚被请求过
	s.reconcile(ctx)

	s.mu.Lock()
	asleep := s.procs["sleepy"].sleeping
	s.mu.Unlock()
	if asleep {
		t.Fatal("expected sleepy NOT to sleep right after being touched")
	}
}
