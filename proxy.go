package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/textproto"
	"strings"
	"time"

	"llama_proxy/internal/config"
	"llama_proxy/internal/httpx"
	"llama_proxy/internal/meta"
	"llama_proxy/internal/model"

	"github.com/youcd/toolkit/log"
)

// handleProxy 是代理转发入口：选择后端、转发请求、流式回传并记录指标。
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
	// llm_prox（或空）→ 轮询模型列表；具体模型 ID → 直连对应后端/本地进程。
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
	defer func() {
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

		resp, err = s.client.Do(outReq)
		if err != nil {
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
const ProxyModelID = "llm_prox"

// statusError 携带期望的 HTTP 状态码（selectBackend 的默认错误按 400 返回）。
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
//   - 模型 ID 为 llm_prox（或空）：默认轮询模型列表——命中 routing 规则（若配置）时
//     只从规则 pool 标签对应的后端子池（含本地 background 节点）选择；其余流量统一
//     落入全量代理池（backends.list + 本地 background 节点，本地节点随就绪状态动态
//     入池/出池，见 syncLocalBackendNode）。请求失败时自动转移到池内下一个未尝试的
//     后端；
//   - 模型 ID 为具体名称：直连部署该模型的后端（backends.list）或本地调度进程
//     （coding/background，未就绪则启动并等待，并续期租约），单后端、不可故障转移；
//     未找到部署该模型的后端/进程时返回 400。
func (s *Server) selectBackend(w http.ResponseWriter, r *http.Request, ctx context.Context, reqModel string) (*backendCandidate, string, func(map[string]bool) *backendCandidate, error) {
	// 具体模型 ID：直连对应后端/本地进程（不使用 routing 规则）。
	if reqModel != "" && reqModel != ProxyModelID {
		cand, err := s.selectSpecificBackend(r, ctx, reqModel)
		if err != nil {
			return nil, r.URL.RawQuery, nil, err
		}
		return cand, r.URL.RawQuery, noFailover, nil
	}

	var first *backendCandidate
	var nextFn func(map[string]bool) *backendCandidate

	if rule := s.matchRoutingRule(r); rule != nil {
		// 命中路由规则：只从 pool 标签子池选点（含本地 background 节点）。
		// 请求失败时自动转移到子池内下一个未尝试的后端。
		if s.balancer != nil {
			if b := s.balancer.SelectFromTag(rule.pool); b != nil {
				first = candidateOf(b)
				pool := rule.pool
				nextFn = func(exclude map[string]bool) *backendCandidate {
					if b := s.balancer.SelectFromTagExcluding(pool, exclude); b != nil {
						return candidateOf(b)
					}
					return nil
				}
			}
		}
		if first == nil {
			return nil, "", nil, fmt.Errorf("routing rule %q matched but pool %q has no available backend", rule.name, rule.pool)
		}
	} else if s.balancer != nil {
		if b := s.balancer.Select(); b != nil {
			first = candidateOf(b)
			nextFn = s.nextBalancerCandidate
		}
	}

	if first == nil {
		return nil, "", nil, fmt.Errorf("no backend selected: configure backends.list")
	}

	if err := config.ValidateBackendURL(first.url); err != nil {
		return nil, "", nil, fmt.Errorf("invalid backend URL: %w", err)
	}
	if nextFn == nil {
		nextFn = noFailover
	}
	return first, r.URL.RawQuery, nextFn, nil
}

// selectSpecificBackend 按具体模型 ID 直连部署该模型的目标：
//  1. 本地 coding 进程（请求其固化模型 ID 时启动/切换并等待就绪，同时续期开发租约）；
//  2. 代理池内声明该模型的后端（backends.list 与就绪的本地 background 节点，
//     经 GetBackendByModel 反查，模型 ID 唯一性由启动时校验保证）；
//  3. 未就绪的本地 background 进程（启动并等待就绪；coding 租约仍活跃时进程被占用，
//     返回 503）。
func (s *Server) selectSpecificBackend(r *http.Request, ctx context.Context, reqModel string) (*backendCandidate, error) {
	sc := s.schedulingConfig()
	if sc != nil && reqModel == sc.Coding.Model {
		return s.selectCodingBackend(ctx, reqModel)
	}
	if s.balancer != nil {
		if b := s.balancer.GetBackendByModel(reqModel); b != nil {
			return candidateOf(b), nil
		}
	}
	if sc != nil && reqModel == sc.Background.Model && s.scheduler != nil {
		if s.scheduler.IsCodingActive() {
			return nil, statusErrf(http.StatusServiceUnavailable,
				"model %q not ready: coding model is active (lease), retry later", reqModel)
		}
		ready, rerr := s.scheduler.EnsureBackground(ctx)
		if rerr != nil {
			return nil, rerr
		}
		select {
		case <-ready:
		case <-ctx.Done():
			return nil, fmt.Errorf("timed out waiting for background model ready")
		}
		base := s.scheduler.ActiveBaseURL()
		if base == "" {
			return nil, fmt.Errorf("background model not ready")
		}
		return &backendCandidate{url: strings.TrimRight(base, "/"), cfg: s.schedulerForwardBC()}, nil
	}
	return nil, fmt.Errorf("unknown model %q: use %q for load-balanced routing or one of the configured model ids", reqModel, ProxyModelID)
}

// selectCodingBackend 把请求路由到本地 coding 进程：未就绪则启动并等待，同时续期租约。
func (s *Server) selectCodingBackend(ctx context.Context, reqModel string) (*backendCandidate, error) {
	s.scheduler.TouchCoding()
	s.pushSchedEvent("coding_request", map[string]any{"model": reqModel})
	ready, rerr := s.scheduler.EnsureCoding(ctx)
	if rerr != nil {
		return nil, rerr
	}
	select {
	case <-ready:
	case <-ctx.Done():
		return nil, fmt.Errorf("timed out waiting for coding model ready")
	}
	base := s.scheduler.ActiveBaseURL()
	if base == "" {
		return nil, fmt.Errorf("coding model not ready")
	}
	s.scheduler.TouchCoding()
	return &backendCandidate{url: strings.TrimRight(base, "/"), cfg: s.schedulerForwardBC()}, nil
}

// schedulingConfig 返回调度配置（未启用调度时为 nil）。
func (s *Server) schedulingConfig() *config.SchedulingConfig {
	_, yamlCfg := s.snapshot()
	if yamlCfg == nil || yamlCfg.Scheduling == nil {
		return nil
	}
	return yamlCfg.Scheduling
}

// backendCandidate 是单个转发目标（后端地址及其配置；动态指定后端时 cfg 为 nil）。
type backendCandidate struct {
	url string
	cfg *config.BackendConfig
}

// backendAttempt 记录单个后端的请求结果，用于所有后端失败时把完整的故障转移过程
// 按后端分组返回给客户端。
type backendAttempt struct {
	Name       string `json:"name,omitempty"`
	URL        string `json:"url"`
	StatusCode int    `json:"status_code,omitempty"`
	Body       string `json:"body,omitempty"`
	Truncated  bool   `json:"truncated,omitempty"`
	Error      string `json:"error,omitempty"`
}

// noFailover 表示没有故障转移目标（单后端或动态指定的后端）。
var noFailover = func(map[string]bool) *backendCandidate { return nil }

// candidateKey 返回故障转移时标记“已尝试”的键：池内后端用后端名（与 balancer 的
// SelectExcluding 按名排除一致），动态指定的后端用地址（其不参与故障转移）。
func candidateKey(c *backendCandidate) string {
	if c.cfg != nil {
		return c.cfg.Name
	}
	return c.url
}

// candidateName 返回后端名；动态指定的后端没有名字，返回空串。
func candidateName(c *backendCandidate) string {
	if c.cfg != nil {
		return c.cfg.Name
	}
	return ""
}

// maxFailReportBytes 是“所有后端失败”报告中每个后端响应体捕获的字节上限。
const maxFailReportBytes = 8 * 1024

// readLimited 从 body 读取至多 cap 字节并返回截断标志（不关闭 body）。
func readLimited(body io.Reader, cap int) ([]byte, bool) {
	raw, _ := io.ReadAll(io.LimitReader(body, int64(cap)+1))
	truncated := len(raw) > cap
	if truncated {
		raw = raw[:cap]
	}
	return raw, truncated
}

// recordRequest 保存请求原文并插入一条请求记录，指向实际转发（或最后尝试）的后端。
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
func candidateOf(b *config.BackendConfig) *backendCandidate {
	return &backendCandidate{url: strings.TrimRight(b.URL, "/"), cfg: b}
}

// nextBalancerCandidate 从全量池返回下一个未尝试过的后端（请求失败后的自动故障转移）。
func (s *Server) nextBalancerCandidate(exclude map[string]bool) *backendCandidate {
	if s.balancer == nil {
		return nil
	}
	if b := s.balancer.SelectExcluding(exclude); b != nil {
		return candidateOf(b)
	}
	return nil
}

// isStreamingResponse 判定响应是否按流式回传（请求声明 stream 或响应为 SSE）。
func isStreamingResponse(resp *http.Response, reqStreaming bool) bool {
	if reqStreaming {
		return true
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	return strings.Contains(ct, "text/event-stream")
}

// streamCopy 普通响应的流式转发：边读边写、边刷盘，同时限量采样到 capture。
func streamCopy(w http.ResponseWriter, src io.Reader, capture *meta.LimitedBuffer, started time.Time) (copied int64, firstByteMs float64, err error) {
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	seenFirst := false
	for {
		n, rerr := src.Read(buf)
		if n > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			chunk := buf[:n]
			wn, werr := w.Write(chunk)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(chunk[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, nil
			}
			return copied, firstByteMs, rerr
		}
	}
}

// streamCopySSE 流式响应的逐行转发：额外统计 SSE data 块数量（排除 [DONE]）。
func streamCopySSE(w http.ResponseWriter, src io.Reader, capture *meta.LimitedBuffer, started time.Time) (copied int64, firstByteMs float64, chunks int64, err error) {
	flusher, _ := w.(http.Flusher)
	reader := bufio.NewReader(src)
	seenFirst := false
	for {
		line, rerr := reader.ReadBytes('\n')
		if len(line) > 0 {
			if !seenFirst {
				seenFirst = true
				firstByteMs = float64(time.Since(started).Milliseconds())
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				trimmed := strings.TrimSpace(strings.TrimPrefix(string(line), "data:"))
				if trimmed != "" && trimmed != "[DONE]" {
					chunks++
				}
			}
			wn, werr := w.Write(line)
			if wn > 0 {
				copied += int64(wn)
				_, _ = capture.Write(line[:wn])
				if flusher != nil {
					flusher.Flush()
				}
			}
			if werr != nil {
				return copied, firstByteMs, chunks, werr
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				return copied, firstByteMs, chunks, nil
			}
			return copied, firstByteMs, chunks, rerr
		}
	}
}
