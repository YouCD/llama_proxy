// snapshot.go 监控面板只读快照：ModelStatus/Snapshot 描述每个模型的运行状态
//（running/ready/pid）、常驻组归属与 coding 态标记。
package scheduler

import (
	"sort"
	"time"

)
// ModelStatus 是单个模型的运行时状态（监控面板展示用）。
type ModelStatus struct {
	ID        string
	Mode      string
	Running   bool // 进程在运行
	Ready     bool
	PID       int
	InGroup   bool // 是否属于默认 background 组（启动集）；false 的 background 为按需加载
	Resident  bool // 常驻模型：coding 态切换时不停止，与 coding 模型并行运行
	GPU       string // 声明占用的 GPU（如 "CUDA1"）；同卡非 resident 模型互斥换入
	NoPool    bool // 不入代理池（仅按模型 ID 直连）；不参与整体 background 就绪判定
	Sleeping  bool // 计划内休眠（闲置超时后主动停进程），属健康状态而非故障
	FailCount int  // 连续启动失败（未就绪即退出/崩溃）次数，就绪后清零
	NextRetry *time.Time // 启动失败退避：该时刻前不重复拉起；nil 表示无退避
	// LoadStartedAt 进行中的加载开始时刻（进程已启动、尚未就绪）；nil 表示无在途加载。
	LoadStartedAt *time.Time
	// LoadDuration 最近一次成功加载的完整耗时（进程启动→就绪探测通过）；零值表示无记录。
	LoadDuration time.Duration
}

// Snapshot 为调度器内部状态的只读快照，供监控面板展示。
type Snapshot struct {
	Models       []ModelStatus
	Group        []string // 常驻默认 background 组成员（按配置顺序）
	CodingModel  string   // coding 态下运行的模型 ID；background 组态为空
	SwitchTarget string   // 进行中的切换目标（"group" 或模型 ID）；无在途切换时为空
	LastCodingAt time.Time
	Lease        time.Duration
}

// Snapshot 返回当前调度器状态快照。
func (s *Scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	group := s.cfg.DefaultBackgroundGroup()
	inGroup := make(map[string]bool, len(group))
	for _, id := range group {
		inGroup[id] = true
	}
	ids := make([]string, 0, len(s.cfg.Models))
	for id := range s.cfg.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	snap := Snapshot{
		Group:        group,
		CodingModel:  s.codingModel,
		SwitchTarget: s.switchTarget,
		LastCodingAt: s.lastCodingAt,
		Lease:        s.lease,
	}
	for _, id := range ids {
		e := s.cfg.Entry(id)
		st := ModelStatus{ID: id, Mode: e.Mode, InGroup: inGroup[id], Resident: e.Resident, GPU: e.GPU, NoPool: e.NoPool}
		if sl := s.procs[id]; sl != nil {
			st.Running = sl.running()
			st.Ready = sl.readyOK
			st.Sleeping = sl.sleeping
			// 优先展示真实服务进程 PID（端口持有者）；未知时回退包装进程 PID。
			if sl.serverPID != 0 {
				st.PID = sl.serverPID
			} else if sl.cmd != nil && sl.cmd.Process != nil {
				st.PID = sl.cmd.Process.Pid
			}
			// 加载计时：仅在进行中的加载（进程存活且未就绪）时暴露开始时刻；
			// 最近一次成功加载的耗时则始终随快照返回（就绪后由面板展示）。
			if !sl.readyOK && !sl.loadStartedAt.IsZero() {
				t := sl.loadStartedAt
				st.LoadStartedAt = &t
			}
			st.LoadDuration = sl.lastLoadDuration
		}
		if n, ok := s.failCount[id]; ok {
			st.FailCount = n
		}
		if nt, ok := s.nextRestartAt[id]; ok && time.Now().Before(nt) {
			t := nt
			st.NextRetry = &t
		}
		snap.Models = append(snap.Models, st)
	}
	return snap
}
