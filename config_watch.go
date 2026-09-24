// config_watch.go 配置文件的动态加载。
//
// 用 fsnotify 监视配置文件，变更去抖后重新 config.Load（全量解码 + 校验）：
//   - 校验失败（含模型 ID 冲突等跨段校验）：保留旧配置并打错误日志，绝不 crash；
//   - 可热更新项（proxy 限制/白名单/超时、log_level、ui_allowed_hosts、retention_days、
//     backends 池、routing 规则、scheduling 的 lease/switch 时序）立即生效；
//   - 无法热更新的项（listen_addr、data_dir、database、轮询参数、
//     scheduling 进程启动参数 command/model/readiness_url）保留旧值并提示重启。
package main

import (
	"context"
	"time"

	"llama_proxy/internal/balancer"
	"llama_proxy/internal/config"

	"github.com/fsnotify/fsnotify"
	"github.com/youcd/toolkit/log"
)

// configReloadDebounce 是文件变更事件的去抖时长：编辑器/保存工具常一次保存触发
// 多个写事件（含临时文件 rename），聚合后只重载一次。
const configReloadDebounce = 200 * time.Millisecond

// watchConfig 监视配置文件并热加载合法变更，ctx 结束（进程退出）时返回。
func watchConfig(ctx context.Context, s *Server, path string) {
	fsw, err := fsnotify.NewWatcher()
	if err != nil {
		log.WithCtx(ctx).Fatalf("watch config: %v", err)
	}
	defer fsw.Close()
	if err := fsw.Add(path); err != nil {
		log.WithCtx(ctx).Fatalf("watch config %s: %v", path, err)
	}
	log.WithCtx(ctx).Infof("config watcher started on %s (hot reload enabled)", path)

	// 常驻 timer（初始已 Stop）：select 直接引用 timer.C，避免 nil 指针。
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-fsw.Events:
			if !ok {
				return
			}
			switch {
			case ev.Has(fsnotify.Write) || ev.Has(fsnotify.Create):
				// 部分编辑器先删后建（Create 事件），确保监视器重新绑定新 inode。
				if ev.Has(fsnotify.Create) {
					_ = fsw.Add(path)
				}
			case ev.Has(fsnotify.Rename) || ev.Has(fsnotify.Remove):
				// sed -i 等工具经 rename(MOVE_SELF) 替换文件，旧 inode 监视失效：
				// 重新绑定新 inode 并触发重载。
				_ = fsw.Add(path)
			default:
				continue
			}
			// 去抖：Stop 失败说明旧定时器未触发，需先排空再重置。
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(configReloadDebounce)
		case err, ok := <-fsw.Errors:
			if !ok {
				return
			}
			log.WithCtx(ctx).Infof("config watcher error: %v", err)
		case <-timer.C:
			s.reloadConfigFrom(path)
		}
	}
}

// reloadConfigFrom 重新读取并校验配置文件，通过后应用新配置。
func (s *Server) reloadConfigFrom(path string) {
	newYaml, err := config.Load(path)
	if err != nil {
		log.WithCtx(nil).Errorf("reload config %s failed, keeping current config: %v", path, err)
		return
	}
	s.reloadConfig(newYaml.ToLegacy(), newYaml)
}

// schedulingLaunch 提取与进程启动相关的调度字段，用于热更新差异检测。
type schedulingLaunch struct {
	Command      string
	Model        string
	ReadinessURL string
	BgCommand    string
	BgModel      string
	BgReadiness  string
}

func processLaunch(sc *config.SchedulingConfig) schedulingLaunch {
	if sc == nil {
		return schedulingLaunch{}
	}
	return schedulingLaunch{
		Command:      sc.Coding.Command,
		Model:        sc.Coding.Model,
		ReadinessURL: sc.Coding.ReadinessURL,
		BgCommand:    sc.Background.Command,
		BgModel:      sc.Background.Model,
		BgReadiness:  sc.Background.ReadinessURL,
	}
}

// reloadConfig 应用新配置：热更新可动态项，保留并告警不可热更新项。
// 新配置已通过 config.Load 的全量校验（含模型 ID 全局唯一性），
// 因此不存在"新后端 model 撞上在服调度进程 ID"的部分生效场景——冲突即整体拒绝。
func (s *Server) reloadConfig(newLegacy config.Config, newYaml *config.YAMLConfig) {
	oldLegacy, oldYaml := s.snapshot()

	// 1) 检测无法热更新的项：保留旧值并记日志。
	var restart []string
	if newLegacy.ListenAddr != oldLegacy.ListenAddr {
		restart = append(restart, "server.listen_addr")
	}
	if newLegacy.DataDir != oldLegacy.DataDir {
		restart = append(restart, "server.data_dir")
	}
	if !sameDatabase(oldYaml, newYaml) {
		restart = append(restart, "database")
	}
	if newLegacy.PollBackendMetrics != oldLegacy.PollBackendMetrics {
		restart = append(restart, "proxy.poll_backend_metrics")
	}
	if newLegacy.PollInterval != oldLegacy.PollInterval {
		restart = append(restart, "proxy.poll_interval_seconds")
	}
	if oldL, newL := processLaunch(oldYaml.Scheduling), processLaunch(newYaml.Scheduling); oldL != newL {
		restart = append(restart, "scheduling.coding/background (command/model/readiness_url)")
	}

	// 2) 发布并应用可热更新部分。
	s.applyConfig(newLegacy, newYaml, newYaml.Backends.List)
	log.SetLogLevel(newLegacy.LogLevel)
	if s.client != nil {
		s.client.Timeout = newLegacy.RequestTimeout
	}
	if s.store != nil {
		s.store.SetRetentionDays(newLegacy.RetentionDays)
	}
	switch {
	case s.balancer != nil:
		s.balancer.Update(newYaml.Backends.List)
	case len(newYaml.Backends.List) > 0:
		// 启动时是纯调度模式（无静态后端池），新配置加入了 backends：现场建池。
		s.balancer = balancer.New(newYaml.Backends.List, newYaml.Backends.Strategy)
		s.balancer.SetLogger(func(format string, args ...any) {
			log.WithCtx(nil).Infof(format, args...)
		})
		s.balancer.LogPools()
	}
	if s.scheduler != nil && newYaml.Scheduling != nil {
		s.scheduler.SetTimings(newYaml.Scheduling.Lease.CodingIdleTimeout, newYaml.Scheduling.Switch)
	}

	// 3) 广播重载结果（前端面板可感知）。
	s.hub.BroadcastWithType("config", map[string]any{
		"reloaded_at":      time.Now().UTC().Format(time.RFC3339),
		"restart_required": restart,
	})
	if len(restart) == 0 {
		log.WithCtx(nil).Infof("config reloaded")
	} else {
		for _, k := range restart {
			log.WithCtx(nil).Warnf("config %s changed but cannot be hot-reloaded, restart required (old value kept)", k)
		}
		log.WithCtx(nil).Infof("config reloaded (%d restart-required change(s))", len(restart))
	}
}

// sameDatabase 判断新旧数据库配置是否一致（值结构体直接比较）。
func sameDatabase(old, new *config.YAMLConfig) bool {
	if old == nil || new == nil {
		return old == new
	}
	return old.Database == new.Database
}
