// gpuwait.go 显存释放等待：coding 模型被杀后，其 GPU 显存由驱动异步回收，
// 期间启动需要大显存的 background 模型会反复 OOM 崩溃（每次崩溃+重启约 7-10 秒，
// 循环可长达数分钟）。stopAll 之后、启动新模型之前，等显存回落到稳定值。
package scheduler

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/youcd/toolkit/log"
)

// smiMemUsedFn 返回各 GPU 已用显存（MiB）。抽成变量便于测试替换。
var smiMemUsedFn = querySmiMemUsed

// querySmiMemUsed 调 nvidia-smi 读取每卡已用显存。无 GPU 或命令失败时返回错误
// （调用方据此跳过等待）。
func querySmiMemUsed() ([]int, error) {
	out, err := exec.Command("nvidia-smi", "--query-gpu=memory.used",
		"--format=csv,noheader,nounits").Output()
	if err != nil {
		return nil, err
	}
	var used []int
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		v, err := strconv.Atoi(line)
		if err != nil {
			return nil, err
		}
		used = append(used, v)
	}
	if len(used) == 0 {
		return nil, syscall.ENOENT // 无 GPU
	}
	return used, nil
}

// waitForGPURelease 在杀掉占用显存的模型后调用：等总显存占用相对基线显著下降
// （>=512MiB）且连续稳定（3 次读数无进一步下降），表明驱动已完成回收。
// 全程无变化（被杀模型不占显存）时约 1.5 秒后放行；超时则告警放行（宁可让
// 后续启动自行重试，也不无限阻塞切换）。
func (s *Scheduler) waitForGPURelease(ctx context.Context, baselineTotal int, timeout time.Duration) {
	if baselineTotal <= 0 {
		return
	}
	const (
		dropThreshold = 512 // MiB：认为"开始回收"的最小下降量
		stableReads   = 3   // 连续稳定读数次数
		pollEvery     = 500 * time.Millisecond
	)
	deadline := time.Now().Add(timeout)
	last := baselineTotal
	stable := 0
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		time.Sleep(pollEvery)
		used, err := smiMemUsedFn()
		if err != nil {
			// 无 GPU 环境：无需等待。
			return
		}
		total := 0
		for _, v := range used {
			total += v
		}
		if total < last {
			last = total
			stable = 0
		} else {
			stable++
		}
		dropped := baselineTotal - last
		if (dropped >= dropThreshold && stable >= stableReads) || dropped >= dropThreshold*4 {
			log.WithCtx(ctx).Infof("[scheduler] GPU memory released: %d -> %d MiB (%d MiB freed)",
				baselineTotal, last, dropped)
			return
		}
		if stable >= stableReads && dropped < dropThreshold {
			// 显存从未明显下降：被杀模型不占显存（纯 CPU 模型或已释放完毕）。
			log.WithCtx(ctx).Infof("[scheduler] GPU memory unchanged (%d MiB), no release to wait for", last)
			return
		}
		if time.Now().After(deadline) {
			log.WithCtx(ctx).Warnf("[scheduler] GPU memory release timed out after %v (baseline %d, now %d MiB); proceeding",
				timeout, baselineTotal, last)
			return
		}
	}
}

// gpuBaselineTotal 读取当前全部 GPU 已用显存之和（MiB）；无 GPU 时返回 0。
func gpuBaselineTotal() int {
	used, err := smiMemUsedFn()
	if err != nil {
		return 0
	}
	total := 0
	for _, v := range used {
		total += v
	}
	return total
}
