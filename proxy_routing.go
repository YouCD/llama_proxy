package main

import (
	"fmt"
	"net/http"
	"net/textproto"
	"strings"


)

func (s *Server) effectiveRoutingRules() []routeRule {
	_, yamlCfg := s.snapshot()
	if yamlCfg == nil || yamlCfg.Routing == nil || len(yamlCfg.Routing.Rules) == 0 {
		return nil
	}
	rules := make([]routeRule, 0, len(yamlCfg.Routing.Rules))
	for i := range yamlCfg.Routing.Rules {
		rr := &yamlCfg.Routing.Rules[i]
		name := strings.TrimSpace(rr.Name)
		if name == "" {
			name = fmt.Sprintf("rule-%d", i+1)
		}
		rules = append(rules, routeRule{
			name:   name,
			header: rr.Header,
			pool:   strings.TrimSpace(rr.Pool),
		})
	}
	return rules
}

// matchRoutingRule 按配置顺序返回首条命中的路由规则；无规则或未命中时返回 nil。
func (s *Server) matchRoutingRule(r *http.Request) *routeRule {
	rules := s.effectiveRoutingRules()
	for i := range rules {
		if ruleMatches(r, rules[i].header) {
			return &rules[i]
		}
	}
	return nil
}

// ruleMatches 判定请求是否命中规则的 header 条件：map 内所有请求头必须同时满足
// （AND）。头名按规范化后的形式比较（Go 服务端把收到的头名规范化为标准形式，例如
// X-LLM-Purpose 会存为 X-Llm-Purpose，客户端原始大小写不可观测，因此配置头名写
// user-agent 或 X-Agent-Env 都等价于标准形式）；头值匹配规则：User-Agent 采用
// 包含匹配（不区分大小写的子串包含），其余请求头 trim 后精确相等、区分大小写；
// 头缺失或为空视为不满足。
func ruleMatches(r *http.Request, h map[string]string) bool {
	if len(h) == 0 {
		return false
	}
	for name, want := range h {
		key := textproto.CanonicalMIMEHeaderKey(name)
		vals, ok := r.Header[key]
		if !ok {
			return false
		}
		v := strings.TrimSpace(vals[0])
		if v == "" {
			return false
		}
		if key == "User-Agent" {
			// User-Agent 采用包含匹配（不区分大小写）：完整 UA 串很长且常带版本/平台后缀，
			// 精确相等难以维护；配置 "GoClaw" 即可命中 "GoClaw/2.1 (Windows)"。
			if !strings.Contains(strings.ToLower(v), strings.ToLower(want)) {
				return false
			}
			continue
		}
		if v != want {
			return false
		}
	}
	return true
}

// ProxyModelID 是代理对外暴露的占位模型 ID：客户端请求该 ID（或省略 model 字段）时
// 按策略轮询模型列表（backends.list + 本地 background 节点）；请求具体模型 ID 时
// 直连部署该模型的后端/本地进程。
const ProxyModelID = "llm_proxy"

// statusError 携带期望的 HTTP 状态码（selectBackend 的默认错误按 400 返回）。
