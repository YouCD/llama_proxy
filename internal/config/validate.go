// Package config 定义 YAML 配置结构与运行时（legacy）配置。
// 配置通过 viper 读取文件并解码（mapstructure 标签），解码后应用默认值，
// 再用 go-playground/validator 做声明式校验，最后做跨字段/业务校验。
package config

import (
	"fmt"
	"net/url"
	"strings"

)

// ValidateModelIDs 校验所有已配置的模型 ID（backends.list 的 model 与 scheduling 的
// coding/background model）互不重复，且不与代理占位 ID llm_proxy 冲突：
// 具体模型 ID 路由要求"一个模型 ID 唯一对应一个后端/进程"，重复会导致路由歧义。
func (c *YAMLConfig) ValidateModelIDs() error {
	const proxyID = "llama_proxy"
	seen := make(map[string]string)
	locate := func(modelID, where string) error {
		if modelID == proxyID {
			return fmt.Errorf("%s: 模型 ID %q 与代理占位 ID %s 冲突（%s 保留用于轮询负载均衡）", where, modelID, proxyID, proxyID)
		}
		if prev, dup := seen[modelID]; dup {
			return fmt.Errorf("模型 ID %q 重复：同时配置在 %s 与 %s（具体模型路由要求模型 ID 全局唯一）", modelID, prev, where)
		}
		seen[modelID] = where
		return nil
	}
	for i := range c.Backends.List {
		b := &c.Backends.List[i]
		if m := strings.TrimSpace(b.Model); m != "" {
			if err := locate(m, "backends.list["+b.Name+"]"); err != nil {
				return err
			}
		}
	}
	if c.Scheduling != nil {
		for id := range c.Scheduling.Models {
			if err := locate(id, "scheduling."+id); err != nil {
				return err
			}
		}
	}
	return nil
}

// ValidatePortCollisions 校验本地监听端口不冲突（从 readiness_url 推导 origin）。
// 冲突判定只约束“同时运行”的进程：
//   - background 组成员并行常驻：成员之间、与 backends.list 之间不得同 origin；
//   - coding 模型互斥运行（同一时刻只有一个 coding 进程，且 background 组已停止）：
//     coding 条目之间可共用同一端口（如多个 coding 模型都监听 8000），但任一
//     coding 条目不得与 backends.list 同 origin（coding 运行时后端仍在服务）；
//   - coding 条目与 background 成员可共用端口（如 coding 复用某 background 成员
//     的端口做双卡张量并行）。
func (c *YAMLConfig) ValidatePortCollisions() error {
	if c.Scheduling == nil {
		return nil
	}
	originOf := func(raw string) string {
		u, err := url.Parse(raw)
		if err != nil || u.Scheme == "" || u.Host == "" {
			return ""
		}
		return u.Scheme + "://" + u.Host
	}
	type seenMap map[string]string
	check := func(seen seenMap, origin, where string) error {
		if origin == "" {
			return nil
		}
		if prev, dup := seen[origin]; dup {
			return fmt.Errorf("监听端口冲突：%s 与 %s 都使用 %s（同时运行的进程争抢同一监听端口）", prev, where, origin)
		}
		seen[origin] = where
		return nil
	}
	bgSeen := seenMap{}
	codeOrigins := make(map[string]string) // origin -> 首个使用该 origin 的 coding 条目
	for id, e := range c.Scheduling.Models {
		origin := originOf(e.ReadinessURL)
		where := "scheduling." + id
		if e.Mode == ModeCoding {
			if origin != "" {
				if _, dup := codeOrigins[origin]; !dup {
					codeOrigins[origin] = where
				}
			}
			continue
		}
		if err := check(bgSeen, origin, where); err != nil {
			return err
		}
	}
	for i := range c.Backends.List {
		b := &c.Backends.List[i]
		origin := originOf(b.URL)
		where := "backends.list[" + b.Name + "]"
		if err := check(bgSeen, origin, where); err != nil {
			return err
		}
		if prev, dup := codeOrigins[origin]; dup {
			return fmt.Errorf("监听端口冲突：%s 与 %s 都使用 %s（coding 模型与 backends.list 后端同时运行，争抢同一监听端口）", prev, where, origin)
		}
	}
	return nil
}

