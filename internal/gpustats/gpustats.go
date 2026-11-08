// Package gpustats 通过 nvidia-smi CLI 周期读取 GPU 性能数据（利用率、显存、
// 风扇、温度、功耗），缓存最新快照供监控 API 与 SSE 广播。
//
// 选择 CLI 而非 NVML：零依赖、纯 Go（无 cgo）、与调度器内部已有的
// nvidia-smi 读取方式一致；5 秒间隔下一次 exec 的开销可忽略。
package gpustats

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/youcd/toolkit/log"
)

// GPU 是单张 GPU 的性能快照。
type GPU struct {
	Index    int     `json:"index"`
	Name     string  `json:"name"`
	Util     int     `json:"util"`      // GPU 利用率 %
	MemUsed  int     `json:"mem_used"`  // 已用显存 MiB
	MemTotal int     `json:"mem_total"` // 总显存 MiB
	Fan      int     `json:"fan"`       // 风扇转速 %（不支持为 0）
	Temp     int     `json:"temp"`      // 核心温度 °C
	Power    float64 `json:"power"`     // 功耗 W
}

// Snapshot 是一次完整的全部 GPU 数据。Available=false 表示无 GPU / nvidia-smi 不可用。
type Snapshot struct {
	Available bool    `json:"available"`
	Time      string  `json:"time"`
	Gpus      []GPU   `json:"gpus"`
	Error     string  `json:"error,omitempty"`
}

// Poller 周期轮询 nvidia-smi 并缓存最新快照。
type Poller struct {
	interval time.Duration
	latest   atomic.Pointer[Snapshot]
	onTick   func(*Snapshot)
	execFn   func() ([]byte, error) // 抽成变量便于测试替换
	lastSig  string
	errLogged bool
}

// NewPoller 创建轮询器。interval 建议 5s 左右。
func NewPoller(interval time.Duration) *Poller {
	return &Poller{interval: interval, execFn: runSMI}
}

// OnTick 注册每次轮询后的回调（如 SSE 广播）。快照与上次完全相同则不回调。
func (p *Poller) OnTick(f func(*Snapshot)) { p.onTick = f }

// Latest 返回最新快照（尚未轮询时为 nil）。
func (p *Poller) Latest() *Snapshot { return p.latest.Load() }

// Start 启动后台轮询循环。
func (p *Poller) Start(ctx context.Context) {
	go func() {
		p.tick()
		for {
			select {
			case <-ctx.Done():
				return
			case <-time.After(p.interval):
			}
			p.tick()
		}
	}()
}

func (p *Poller) tick() {
	snap := query(p.execFn)
	p.latest.Store(snap)
	if snap.Error != "" {
		// 无 GPU / nvidia-smi 缺失是常见部署形态，只记一次日志，不刷屏。
		if !p.errLogged {
			p.errLogged = true
			log.WithCtx(context.Background()).Infof("gpustats: nvidia-smi unavailable: %v", snap.Error)
		}
	}
	// 数据无变化（含持续无 GPU）时不广播，避免 SSE 无谓刷新。
	sig := snap.Signature()
	if p.onTick != nil && sig != p.lastSig {
		p.lastSig = sig
		p.onTick(snap)
	}
}

// runSMI 调 nvidia-smi 读取每卡性能数据。
func runSMI() ([]byte, error) {
	return exec.Command("nvidia-smi",
		"--query-gpu=index,name,utilization.gpu,memory.used,memory.total,fan.speed,temperature.gpu,power.draw",
		"--format=csv,noheader,nounits").Output()
}

// Signature 返回快照的轻量签名（用于变化检测）。
func (s *Snapshot) Signature() string {
	var b strings.Builder
	b.WriteString(strconv.FormatBool(s.Available))
	for _, g := range s.Gpus {
		b.WriteByte(',')
		b.WriteString(strconv.Itoa(g.Index))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(g.Util))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(g.MemUsed))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(g.Temp))
		b.WriteByte('/')
		b.WriteString(strconv.Itoa(g.Fan))
		b.WriteByte('/')
		b.WriteString(strconv.FormatFloat(g.Power, 'f', 0, 64))
	}
	return b.String()
}

// query 执行一次 nvidia-smi 并解析 CSV 为快照。命令失败返回 Available=false 的快照。
func query(run func() ([]byte, error)) *Snapshot {
	now := time.Now().UTC().Format(time.RFC3339)
	out, err := run()
	if err != nil {
		return &Snapshot{Available: false, Time: now, Gpus: []GPU{}, Error: err.Error()}
	}
	gpus := make([]GPU, 0)
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		f := strings.Split(line, ",")
		if len(f) != 8 {
			continue
		}
		g := GPU{Name: strings.TrimSpace(f[1])}
		g.Index, _ = strconv.Atoi(strings.TrimSpace(f[0]))
		g.Util, _ = strconv.Atoi(strings.TrimSpace(f[2]))
		g.MemUsed, _ = strconv.Atoi(strings.TrimSpace(f[3]))
		g.MemTotal, _ = strconv.Atoi(strings.TrimSpace(f[4]))
		g.Fan, _ = strconv.Atoi(strings.TrimSpace(f[5]))
		g.Temp, _ = strconv.Atoi(strings.TrimSpace(f[6]))
		g.Power, _ = strconv.ParseFloat(strings.TrimSpace(f[7]), 64)
		gpus = append(gpus, g)
	}
	if len(gpus) == 0 {
		return &Snapshot{Available: false, Time: now, Gpus: []GPU{}}
	}
	return &Snapshot{Available: true, Time: now, Gpus: gpus}
}
