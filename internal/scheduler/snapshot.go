// snapshot.go 监控面板只读快照：ModelStatus/Snapshot 描述每个模型的运行状态
//（running/ready/pid）、常驻组归属与 coding 态标记。
package scheduler

import (
	"sort"
	"time"

)
// ModelStatus 是单个模型的运行时状态（监控面板展示用）。
type ModelStatus struct {
	ID      string
	Mode    string
	Running bool // 进程在运行
	Ready   bool
	PID     int
	InGroup bool // 是否属于常驻默认 background 组
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
		st := ModelStatus{ID: id, Mode: e.Mode, InGroup: inGroup[id]}
		if sl := s.procs[id]; sl != nil {
			st.Running = sl.cmd != nil
			st.Ready = sl.readyOK
			if sl.cmd != nil && sl.cmd.Process != nil {
				st.PID = sl.cmd.Process.Pid
			}
		}
		snap.Models = append(snap.Models, st)
	}
	return snap
}
