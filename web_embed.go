//go:build embedweb

package main

import (
	"embed"
	"fmt"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"strings"

	"llama_proxy/internal/httpx"
)

//go:embed web
var webFS embed.FS

// handleUI serves the frontend embedded into the binary.
// Enabled by building with `go build -tags embedweb`.
func (s *Server) handleUI(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/_proxy")
	if p == "" {
		p = "/"
	}
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if !s.uiHostAllowed(r) {
		// 报错带上实际 Host 与配置提示：浏览器直接访问时能看到"该加哪个 Host 到白名单"。
		httpx.WriteJSON(w, http.StatusForbidden, map[string]any{
			"error":   "forbidden",
			"host":    httpx.HostOnly(r.Host),
			"message": fmt.Sprintf("ui access not allowed for host %q; add it to server.ui_allowed_hosts in config.yaml", httpx.HostOnly(r.Host)),
		})
		return
	}

	var name string
	if p == "/ui" || p == "/" {
		name = "web/index.html"
	} else if strings.HasPrefix(p, "/ui/") {
		clean := path.Clean(strings.TrimPrefix(p, "/ui"))
		if strings.Contains(clean, "..") {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
			return
		}
		name = path.Join("web", clean)
	} else if strings.HasPrefix(p, "/assets/") {
		clean := path.Clean(p)
		if strings.Contains(clean, "..") {
			httpx.WriteJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid path"})
			return
		}
		name = path.Join("web", clean)
	} else if p == "/favicon.svg" || p == "/favicon.ico" {
		name = "web/favicon.svg"
	} else {
		name = "web/index.html"
	}

	data, err := fs.ReadFile(webFS, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "handleUI: failed to read %s: %v\n", name, err)
		http.NotFound(w, r)
		return
	}
	if ct := mime.TypeByExtension(path.Ext(name)); ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	_, _ = w.Write(data)
}
