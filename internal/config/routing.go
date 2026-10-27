// Package config 定义 YAML 配置结构与运行时（legacy）配置。
// 配置通过 viper 读取文件并解码（mapstructure 标签），解码后应用默认值，
// 再用 go-playground/validator 做声明式校验，最后做跨字段/业务校验。
package config

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// RoutingConfig 描述按客户端特征路由到指定标签池的规则表。
// 规则按配置顺序评估，首条命中的规则生效；未配置时不存在路由规则，全部流量走默认池。
type RoutingConfig struct {
	Rules []RoutingRule `mapstructure:"rules" validate:"dive"`
}

// RoutingRule 描述一条路由规则：Header 条件命中后，请求只从 Pool 标签对应的后端子池
// 中选择；请求失败时自动故障转移到子池内下一个未尝试的后端。
type RoutingRule struct {
	// Name 规则名，仅用于日志与报错；留空时按序号生成。
	Name string `mapstructure:"name"`
	// Header 规则命中条件：每个请求头的名称与值都必须与客户端请求头匹配，map 内所有
	// key 同时满足（AND）才算命中。头名按规范化形式比较（服务端不区分大小写），头值
	// trim 后精确相等、区分大小写。
	Header map[string]string `mapstructure:"header" validate:"min=1"`
	// Pool 标签名：命中后只从携带该标签（tags）的后端中选择。
	Pool string `mapstructure:"pool" validate:"required"`
}

// checkLegacyRouting 检测旧版 routing 配置字段（match / user_agent_contains）并给出
// 迁移提示；这些字段已不再支持。
func checkLegacyRouting(data []byte) error {
	var legacy struct {
		Routing *struct {
			Rules []struct {
				Match             map[string]any `yaml:"match"`
				UserAgentContains string         `yaml:"user_agent_contains"`
			} `yaml:"rules"`
		} `yaml:"routing"`
	}
	if err := yaml.Unmarshal(data, &legacy); err != nil {
		return err
	}
	if legacy.Routing == nil {
		return nil
	}
	for i, lr := range legacy.Routing.Rules {
		if lr.Match != nil || lr.UserAgentContains != "" {
			return fmt.Errorf("routing.rules[%d]: 旧字段 match/user_agent_contains 已移除，请改写为 header（如 User-Agent: \"完整User-Agent值\"）", i)
		}
	}
	return nil
}

// validateRouting 保留作为路由规则的结构兜底校验（声明式校验已覆盖 pool/header
// 非空），当前仅做 trim 后的空串判定以给出带序号的友好报错。
func validateRouting(rc *RoutingConfig) error {
	if rc == nil {
		return nil
	}
	for i, rr := range rc.Rules {
		if strings.TrimSpace(rr.Pool) == "" {
			return fmt.Errorf("routing.rules[%d]: pool 不能为空", i)
		}
		if len(rr.Header) == 0 {
			return fmt.Errorf("routing.rules[%d]: header 不能为空（需配置至少一个请求头条件）", i)
		}
	}
	return nil
}

