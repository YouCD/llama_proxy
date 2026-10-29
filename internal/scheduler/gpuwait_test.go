package scheduler

import (
	"context"
	"syscall"
	"testing"
	"time"
)

// fakeSmi 用脚本化的显存读数序列替换 nvidia-smi 查询。
func fakeSmi(t *testing.T, seq []int) {
	t.Helper()
	calls := 0
	old := smiMemUsedFn
	smiMemUsedFn = func() ([]int, error) {
		if calls >= len(seq) {
			return []int{seq[len(seq)-1]}, nil
		}
		v := seq[calls]
		calls++
		return []int{v}, nil
	}
	t.Cleanup(func() { smiMemUsedFn = old })
}

func TestWaitForGPUReleaseGradual(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	// 基线 40000，逐步释放到 1000 后稳定：应等到稳定后放行。
	fakeSmi(t, []int{39000, 30000, 1000, 1000, 1000, 1000})
	done := make(chan struct{})
	go func() {
		s.waitForGPURelease(context.Background(), 40000, 5*time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("waitForGPURelease did not return after stable release")
	}
}

func TestWaitForGPUReleaseUnchanged(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	// 显存从未下降（被杀模型不占显存）：应快速放行而非等到超时。
	fakeSmi(t, []int{5000, 5000, 5000, 5000})
	start := time.Now()
	s.waitForGPURelease(context.Background(), 5000, 30*time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("unchanged GPU memory should release quickly, took %v", elapsed)
	}
}

func TestWaitForGPUReleaseNoGPU(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	old := smiMemUsedFn
	smiMemUsedFn = func() ([]int, error) { return nil, syscall.ENOENT }
	t.Cleanup(func() { smiMemUsedFn = old })
	s.waitForGPURelease(context.Background(), 40000, time.Second) // 应立即返回
}

func TestWaitForGPUReleaseTimeout(t *testing.T) {
	s := newTestScheduler(t, time.Hour)
	// 显存只下降一点就卡住：应超时放行（告警），不无限等待。
	fakeSmi(t, []int{39900, 39900, 39900, 39900, 39900})
	start := time.Now()
	s.waitForGPURelease(context.Background(), 40000, 800*time.Millisecond)
	if elapsed := time.Since(start); elapsed < 700*time.Millisecond {
		t.Fatalf("expected timeout wait ~800ms, got %v", elapsed)
	}
}

func TestQuerySmiMemUsedParsesOutput(t *testing.T) {
	// 无 GPU 环境该函数应报错（不 panic）；有 GPU 环境应返回非负读数。
	used, err := querySmiMemUsed()
	if err != nil {
		return // 无 GPU，跳过
	}
	if len(used) == 0 {
		t.Fatal("expected at least one GPU reading")
	}
	for i, v := range used {
		if v < 0 {
			t.Fatalf("gpu %d negative used memory %d", i, v)
		}
	}
}
