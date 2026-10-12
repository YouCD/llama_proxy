//go:build !embedweb

package main

import (
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"llama_proxy/internal/httpx"
)

// handleUI serves the frontend from the on-disk web/ directory.
// Use `go build -tags embedweb` to embed the frontend into the binary instead.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/_proxy")
	if p == "" {
		p = "/"
	}
	w.Header().Set("Cache-Control", "no-cache")

	if !s.uiHostAllowed(r) {
		// 报错带上实际 Host 与配置提示：浏览器直接访问时能看到"该加哪个 Host 到白名单"。
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error":   "forbidden",
			"host":    httpx.HostOnly(r.Host),
			"message": fmt.Sprintf("ui access not allowed for host %q; add it to server.ui_allowed_hosts in config.yaml", httpx.HostOnly(r.Host)),
		})
		return
	}

	var webPath string
	if p == "/ui" || p == "/" {
		webPath = "web/index.html"
	} else if strings.HasPrefix(p, "/ui/") {
		clean := filepath.Clean(strings.TrimPrefix(p, "/ui"))
		if strings.Contains(clean, "..") {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
			return
		}
		webPath = filepath.Join("web", clean)
	} else if strings.HasPrefix(p, "/assets/") {
		clean := filepath.Clean(p)
		if strings.Contains(clean, "..") {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
			return
		}
		webPath = filepath.Join("web", clean)
	} else if p == "/favicon.svg" || p == "/favicon.ico" {
		webPath = "web/favicon.svg"
	} else {
		webPath = "web/index.html"
	}

	http.ServeFile(w, r, webPath)
}
