package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"llama_proxy/internal/httpx"
	"llama_proxy/internal/meta"
	"llama_proxy/internal/model"

	"github.com/youcd/toolkit/log"
)

func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	// 请求开始时取一份配置快照，处理期间配置热更新不影响本请求的判定。
	cfg, _ := s.snapshot()
	// 校验客户端 API Key（如果已配置）
	if cfg.APIKey != "" {
		clientAuth := strings.TrimSpace(r.Header.Get("Authorization"))
		if clientAuth != "Bearer "+cfg.APIKey {
			httpx.WriteJSON(w, http.StatusUnauthorized, map[string]any{"error": "unauthorized", "message": "missing or invalid api_key"})
			return
		}
	}
	if r.URL.Path == "/v1/models" && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		s.handleModels(w, r)
		return
	}
	if !s.shouldRecordProxy(r.URL.Path) {
		httpx.WriteJSON(w, http.StatusNotFound, map[string]any{"error": "not found"})
		return
	}
	started := time.Now()
	requestID := httpx.NewID()
	clientIP := httpx.ClientIP(r)
	// 创建带 request_id 的请求上下文，日志可按 request_id 串联整个请求生命周期。
	ctx := s.newInflightCtx(r.Context(), requestID)

	if cfg.MaxRequestBytes > 0 && r.ContentLength > cfg.MaxRequestBytes {
		httpx.WriteJSON(w, http.StatusRequestEntityTooLarge, map[string]any{"error": "request is too large"})
		return
	}

	// 先读请求体并识别模型 ID，再按模型 ID 选择转发后端：
	// llm_proxy（或空）→ 轮询模型列表；具体模型 ID → 直连对应后端/本地进程。
	originalBody, err := httpx.ReadWithLimit(r.Body, cfg.MaxRequestBytes)
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, httpx.ErrTooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		httpx.WriteJSON(w, code, map[string]any{"error": err.Error()})
		return
	}

	isStreaming, detectedModel := meta.DetectRequestMeta(originalBody)
	// 请求到达即打印（selectBackend 可能阻塞等待模型加载/切换，不能等转发前才打，
	// 否则"谁在什么时间触发了模型切换"无法从日志追溯）。
	log.WithCtx(ctx).Infof("request received: id=%s method=%s path=%s client=%s model=%s stream=%v ua=%q",
		requestID, r.Method, r.URL.Path, clientIP, detectedModel, isStreaming, r.UserAgent())
	firstCand, trimmedQuery, nextBackend, err := s.selectBackend(w, r, ctx, detectedModel)
	if err != nil {
		code := http.StatusBadRequest
		if se, ok := err.(*statusError); ok {
			code = se.code
		}
		httpx.WriteJSON(w, code, map[string]any{"error": err.Error(), "request_id": requestID})
		return
	}

	s.active.Add(1)
	s.hub.BroadcastWithType("active", map[string]any{"active_connections": s.active.Load(), "time": time.Now().UTC()})
	// 登记在途请求（供面板实时展示客户端 IP / User-Agent 等当前请求信息），
	// 所有结束路径（成功/失败/客户端断开）统一经 defer 注销。
	s.inflightAdd(&inflightInfo{
		ID:         requestID,
		ClientIP:   clientIP,
		UserAgent:  r.UserAgent(),
		Method:     r.Method,
		Path:       r.URL.Path,
		Model:      detectedModel,
		BackendURL: firstCand.url,
		Streaming:  isStreaming,
		StartedAt:  time.Now().UTC(),
	})
	// successNode 记录成功转发所在节点：其负载在响应体完全回传前一直计入在途，
	// 由本 defer 在处理器退出时注销；故障转移/候选耗尽路径的节点在各自分支即时注销。
	successNode := ""
	defer func() {
		s.leaveNode(successNode)
		s.inflightRemove(requestID)
		s.active.Add(-1)
		s.hub.BroadcastWithType("active", map[string]any{"active_connections": s.active.Load(), "time": time.Now().UTC()})
	}()

	// 转发请求：当前后端失败（连接错误，或响应体写出前返回非 200 状态码）时自动尝试
	// 下一个未试过的后端，直到成功或候选耗尽。候选耗尽时把每个后端的请求结果按后端
	// 分组返回给客户端：全部连接错误时返回 502；最后一次是非 200 响应时沿用该状态码，
	// 响应体中列出每个后端的状态/响应体或错误。
	var (
		cand     = firstCand
		tried    = map[string]bool{candidateKey(firstCand): true}
		resp     *http.Response
		body     []byte
		reqModel = detectedModel
		lastErr  error
		attempts []backendAttempt
	)
	for {
		body = originalBody
		reqModel = detectedModel
		if cand.cfg != nil && cand.cfg.Model != "" {
			if newBody, newModel, rerr := meta.RewriteModel(originalBody, cand.cfg.Model); rerr == nil {
				body = newBody
				reqModel = newModel
			}
		}
		log.WithCtx(ctx).Infof("forwarding request: id=%s backend=%s model=%s stream=%v",
			requestID, cand.url, reqModel, isStreaming)

		target := cand.url + buildProxyPath(cand.url, r.URL.Path)
		if trimmedQuery != "" {
			target += "?" + trimmedQuery
		}
		outReq, rerr := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
		if rerr != nil {
			httpx.WriteJSON(w, http.StatusInternalServerError, map[string]any{"error": rerr.Error()})
			return
		}
		httpx.CopyRequestHeaders(outReq.Header, r.Header)
		if cand.cfg != nil && cand.cfg.APIKey != "" {
			outReq.Header.Set("Authorization", "Bearer "+cand.cfg.APIKey)
		}
		outReq.Header.Set("X-Proxy-Request-ID", requestID)
		outReq.ContentLength = int64(len(body))

		// 负载感知：转发前把该节点在途计数 +1（直连与轮询候选都经此计入均衡器）。
		s.enterNode(nodeOf(cand))
		resp, err = s.client.Do(outReq)
		if err != nil {
			// 本次尝试已结束（无论随后故障转移还是候选耗尽），先注销该节点在途。
			s.leaveNode(nodeOf(cand))
			lastErr = err
			attempts = append(attempts, backendAttempt{
				Name:  candidateName(cand),
				URL:   cand.url,
				Error: err.Error(),
			})
			if nb := nextBackend(tried); nb != nil {
				log.WithCtx(ctx).Infof("backend request failed, failing over: id=%s failed=%s err=%v next=%s",
					requestID, cand.url, err, nb.url)
				tried[candidateKey(nb)] = true
				cand = nb
				continue
			}
			total := float64(time.Since(started).Milliseconds())
			// 候选耗尽时记录尚未插入（插入在循环之后），此处先补插再收尾；
			// 每个后端的失败原因按后端分组返回给客户端。
			s.recordRequest(ctx, requestID, started, r, clientIP, trimmedQuery, isStreaming, cand, reqModel, body)
			_ = s.store.FinishRequest(requestID, model.RequestRecord{Model: reqModel, StatusCode: 502, ErrorText: lastErr.Error(), TotalMs: total})
			httpx.WriteJSON(w, http.StatusBadGateway, map[string]any{
				"error":      "all backends failed",
				"request_id": requestID,
				"attempts":   attempts,
			})
			return
		}
		if resp.StatusCode != http.StatusOK {
			// 本次尝试已结束（随后故障转移或候选耗尽），先注销该节点在途。
			s.leaveNode(nodeOf(cand))
			respBody, truncated := readLimited(resp.Body, maxFailReportBytes)
			attempts = append(attempts, backendAttempt{
				Name:       candidateName(cand),
				URL:        cand.url,
				StatusCode: resp.StatusCode,
				Body:       string(respBody),
				Truncated:  truncated,
			})
			if nb := nextBackend(tried); nb != nil {
				log.WithCtx(ctx).Infof("backend returned %d, failing over: id=%s failed=%s next=%s",
					resp.StatusCode, requestID, cand.url, nb.url)
				_ = resp.Body.Close()
				tried[candidateKey(nb)] = true
				cand = nb
				continue
			}
			// 所有后端都返回非 200：每个后端的响应内容按后端分组返回，
			// 状态码沿用最后一个后端的。
			total := float64(time.Since(started).Milliseconds())
			s.recordRequest(ctx, requestID, started, r, clientIP, trimmedQuery, isStreaming, cand, reqModel, body)
			_ = s.store.FinishRequest(requestID, model.RequestRecord{Model: reqModel, StatusCode: resp.StatusCode, TotalMs: total})
			_ = resp.Body.Close()
			httpx.WriteJSON(w, resp.StatusCode, map[string]any{
				"error":      "all backends returned non-200",
				"request_id": requestID,
				"attempts":   attempts,
			})
			return
		}
		// 成功路径：该节点负载保持计入在途，直到响应体回传完毕（defer 注销）。
		successNode = nodeOf(cand)
		break
	}

	s.recordRequest(ctx, requestID, started, r, clientIP, trimmedQuery, isStreaming, cand, reqModel, body)

	defer resp.Body.Close()

	for k, vals := range resp.Header {
		if httpx.IsHopByHopHeader(k) {
			continue
		}
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Proxy-Request-ID", requestID)
	w.WriteHeader(resp.StatusCode)

	var firstByteMs float64
	var copied int64
	var chunks int64
	capturer := meta.NewLimitedBuffer(cfg.MaxCaptureBytes)

	if isStreamingResponse(resp, isStreaming) {
		copied, firstByteMs, chunks, err = streamCopySSE(w, resp.Body, capturer, started)
	} else {
		copied, firstByteMs, err = streamCopy(w, resp.Body, capturer, started)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.WithCtx(ctx).Infof("copy response failed req=%s: %v", requestID, err)
	}

	respBytes := capturer.Bytes()
	respRawPath, saveErr := s.store.SaveRawPayload(requestID, "response", respBytes)
	if saveErr != nil {
		log.WithCtx(ctx).Infof("save response raw failed: %v", saveErr)
	}

	parsed := meta.ParseResponseMeta(resp.Header, respBytes)
	finalModel := reqModel
	if parsed.Model != "" {
		finalModel = parsed.Model
	}
	cacheHitPct := 0.0
	if parsed.PromptTokens > 0 && parsed.CachedPromptTokens > 0 {
		cacheHitPct = float64(parsed.CachedPromptTokens) / float64(parsed.PromptTokens) * 100
	}
	totalMs := float64(time.Since(started).Milliseconds())

	if err := s.store.FinishRequest(requestID, model.RequestRecord{
		Model:              finalModel,
		StatusCode:         resp.StatusCode,
		ResponseBytes:      copied,
		PromptTokens:       parsed.PromptTokens,
		CachedPromptTokens: parsed.CachedPromptTokens,
		CacheHitPct:        cacheHitPct,
		CompletionTokens:   parsed.CompletionTok,
		TotalTokens:        parsed.TotalTokens,
		PromptMs:           parsed.PromptMs,
		CompletionMs:       parsed.CompletionMs,
		TotalMs:            totalMs,
		FirstByteMs:        firstByteMs,
		ChunksCount:        chunks,
		ResponseRawPath:    respRawPath,
	}); err != nil {
		log.WithCtx(ctx).Infof("finish request failed: %v", err)
	}

	s.hub.BroadcastWithType("request", map[string]any{
		"id":                   requestID,
		"time":                 time.Now().UTC(),
		"path":                 r.URL.Path,
		"method":               r.Method,
		"status_code":          resp.StatusCode,
		"total_ms":             totalMs,
		"first_byte_ms":        firstByteMs,
		"prompt_tokens":        parsed.PromptTokens,
		"cached_prompt_tokens": parsed.CachedPromptTokens,
		"cache_hit_pct":        cacheHitPct,
		"completion_tokens":    parsed.CompletionTok,
		"total_tokens":         parsed.TotalTokens,
		"model":                finalModel,
		"response_bytes":       copied,
		"chunks_count":         chunks,
		"backend_url":          cand.url,
		"active_connections":   s.active.Load(),
	})
}

func (s *Server) pathMatches(path string, list []string) bool {
	p := strings.Trim(path, "/")
	if p == "" {
		return false
	}
	for _, ig := range list {
		raw := strings.Trim(ig, "/")
		if raw == "" {
			continue
		}
		if p == raw || strings.HasPrefix(p, raw+"/") {
			return true
		}
	}
	return false
}

// shouldRecordProxy 决定某个路径的请求是否需要转发并记录。
// 仅命中白名单 record_paths 的路径会被转发记录；其余一律 404。
func (s *Server) shouldRecordProxy(path string) bool {
	if path == "/" {
		return false
	}
	cfg, _ := s.snapshot()
	return s.pathMatches(path, cfg.RecordPaths)
}

// buildProxyPath 拼接转发路径：当后端 url 已带 /v1 时去掉客户端路径的 /v1 前缀，
// 避免 /v1 重复；后端 url 不带 /v1 时保留客户端路径原样（OpenAI 标准 /v1/...）。
func buildProxyPath(backendURL, path string) string {
	if strings.Contains(backendURL, "/v1") {
		if path == "/v1" {
			return "/"
		}
		return strings.TrimPrefix(path, "/v1")
	}
	return path
}

// routeRule 描述一条生效的路由规则（来自 routing.rules 配置）。
type routeRule struct {
	name   string
	header map[string]string
	pool   string
}

// effectiveRoutingRules 返回生效的路由规则（来自 routing.rules 配置）；
// 未配置 routing 时返回空切片（无规则，全部流量走默认池）。

type statusError struct {
	code int
	msg  string
}

func (e *statusError) Error() string { return e.msg }

func statusErrf(code int, format string, args ...any) error {
	return &statusError{code: code, msg: fmt.Sprintf(format, args...)}
}

// selectBackend 选择本次请求的转发后端，返回首个转发候选与故障转移函数。
// 按请求体中的模型 ID 分派：
//
//   - 模型 ID 为 llm_proxy（或空）：默认轮询模型列表——命中 routing 规则（若配置）时
//     只从规则 pool 标签对应的后端子池（含本地节点）选择；其余流量统一
//     落入全量代理池（backends.list + 本地节点，本地节点随就绪状态动态
//     入池/出池，见 syncLocalBackendNode）。请求失败时自动转移到池内下一个未尝试的
//     后端；
//   - 模型 ID 为具体名称：直连部署该模型的后端（backends.list）或本地调度进程
//     （scheduling 模型条目，未就绪则启动并等待，coding 模式续期租约），
//     单后端、不可故障转移；未找到部署该模型的后端/进程时返回 400。

func (s *Server) recordRequest(ctx context.Context, requestID string, started time.Time, r *http.Request, clientIP, trimmedQuery string, isStreaming bool, cand *backendCandidate, reqModel string, body []byte) {
	reqRawPath, err := s.store.SaveRawPayload(requestID, "request", body)
	if err != nil {
		log.WithCtx(ctx).Infof("save request raw failed: %v", err)
	}
	if err := s.store.InsertRequest(model.RequestRecord{
		ID:             requestID,
		CreatedAt:      started.UTC(),
		Method:         r.Method,
		Path:           r.URL.Path,
		Query:          trimmedQuery,
		ClientIP:       clientIP,
		BackendURL:     cand.url,
		Model:          reqModel,
		IsStreaming:    isStreaming,
		RequestBytes:   int64(len(body)),
		RequestRawPath: reqRawPath,
		UserAgent:      r.UserAgent(),
	}); err != nil {
		log.WithCtx(ctx).Infof("insert request failed: %v", err)
	}
}

// candidateOf 把后端配置转换为转发候选。
