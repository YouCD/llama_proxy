package main

import (
	"net/http"
	"strings"

	"llama_proxy/internal/httpx"
)

// uiHostAllowed 判断当前请求的 Host 是否允许访问 UI 面板。
// 未配置 ui_allowed_hosts 时放行所有 Host；配置后仅放行列表中的域名（忽略端口与大小写）。
func (s *Server) uiHostAllowed(r *http.Request) bool {
	cfg, _ := s.snapshot()
	allowed := cfg.UIAllowedHosts
	if len(allowed) == 0 {
		return true
	}
	host := httpx.HostOnly(r.Host)
	if host == "" {
		return false
	}
	for _, h := range allowed {
		if strings.EqualFold(host, httpx.HostOnly(h)) {
			return true
		}
	}
	return false
}
