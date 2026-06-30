// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

// Package api 暴露 OpenAI 兼容的 HTTP 端点。
//
// 里程碑1 只实现非流式 /v1/chat/completions（+ /、/health、/v1/models）。
// 真流式 SSE、图像/TTS/embeddings、Gemini 原生端点留待后续里程碑。
package api

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/metrics"
	"github.com/bsfdsagfadg/vertex/internal/transform"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
	"github.com/google/uuid"
)

// Server 持有依赖，挂载路由。
type Server struct {
	vc      *vertex.VertexAIClient
	keys    *APIKeyManager
	cfg     config.AppConfig
	metrics *metrics.Collector
}

// NewServer 构造 Server。
func NewServer(vc *vertex.VertexAIClient, keys *APIKeyManager, cfg config.AppConfig) *Server {
	return &Server{vc: vc, keys: keys, cfg: cfg, metrics: metrics.Default}
}

// Handler 构建带中间件链（recover → CORS → APIKey）的 HTTP handler。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/v1/models", s.handleModelsOAI)
	mux.HandleFunc("/v1beta/models", s.handleModelsGemini)
	mux.HandleFunc("/v1/chat/completions", s.handleChatCompletions)
	// 图像端点（文生图 / 编辑 / 变体）。
	mux.HandleFunc("/v1/images/generations", s.handleImageGenerations)
	mux.HandleFunc("/v1/images/edits", s.handleImageEdits)
	mux.HandleFunc("/v1/images/variations", s.handleImageVariations)
	// TTS。
	mux.HandleFunc("/v1/audio/speech", s.handleAudioSpeech)
	// 自身可观测：内部健康指标（需 API key；刻意不做 per-key/模型用量，那是上游网关的账本）。
	mux.HandleFunc("/metrics", s.handleMetrics)
	// 管理后台：静态面板（/admin、/admin/ 子树）+ JSON 接口（/api/admin/ 子树）。
	// 二者有独立的 session 鉴权（见 admin.go），故在 withAPIKey/withMetrics 里被排除。
	mux.HandleFunc("/favicon.ico", s.handleFavicon)
	mux.HandleFunc("/admin", s.handleAdminPage)
	mux.HandleFunc("/admin/", s.handleAdminPage)
	mux.HandleFunc("/api/admin/", s.handleAdminAPI)
	mux.HandleFunc("/assets/", s.handleAssets)
	// Gemini 原生端点 + 单模型详情 + countTokens：path 形如 /v1beta/models/{model}:method
	// 或 /v1beta/models/{model}，无法用静态 mux 表达（{model} 含点、:method 是末段后缀），
	// 故对 /v1beta/models/ 与 /v1/models/ 前缀走自定义分发器。
	mux.HandleFunc("/v1beta/models/", s.handleModelsSubtree)
	mux.HandleFunc("/v1/models/", s.handleModelsSubtree)

	// pprof 调试端点（通过配置 debug_pprof 启用）。
	if s.cfg.DebugPprof {
		mux.HandleFunc("/debug/pprof/", pprofIndex)
		mux.HandleFunc("/debug/pprof/cmdline", pprintCmdline)
		mux.HandleFunc("/debug/pprof/profile", pprofProfile)
		mux.HandleFunc("/debug/pprof/symbol", pprofSymbol)
		mux.HandleFunc("/debug/pprof/trace", pprofTrace)
		mux.HandleFunc("/debug/pprof/goroutine", pprofGoroutine)
		mux.HandleFunc("/debug/pprof/heap", pprofHeap)
		mux.HandleFunc("/debug/pprof/threadcreate", pprofThreadcreate)
		mux.HandleFunc("/debug/pprof/block", pprofBlock)
		mux.HandleFunc("/debug/pprof/mutex", pprofMutex)
	}

	return s.withRecover(s.withCORS(s.withMetrics(s.withAPIKey(s.withBodyLimit(mux)))))
}

// ---- 端点 ----

func (s *Server) handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleAssets(w http.ResponseWriter, r *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(config.ConfigDir()), "assets")
	fs := http.StripPrefix("/assets/", http.FileServer(http.Dir(assetsDir)))
	fs.ServeHTTP(w, r)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		s.oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"message": "Vertex AI Proxy", "version": "2.0-go"})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Server] [Health] 收到健康检查请求")
	s.writeJSON(w, http.StatusOK, map[string]any{
		"status":          "healthy",
		"timestamp":       time.Now().Unix(),
		"api_keys_loaded": s.keys.Count(),
	})
}

// handleMetrics 返回服务的实时状态，供健康检查与管理后台做存活探测。
// 刻意不含 per-key/模型用量（那是上游网关的账本职责）。
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	log.Printf("[Server] [Metrics] 收到指标获取请求")
	s.writeJSON(w, http.StatusOK, s.metricsBody())
}

func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if _, ok := err.(*json.SyntaxError); ok && strings.Contains(err.Error(), "invalid UTF-8") {
			s.oaiError(w, http.StatusBadRequest, "请求体编码错误，需为 UTF-8 (request body must be UTF-8 encoded)", "invalid_request_error")
			return
		}
		s.oaiError(w, http.StatusBadRequest, "请求格式错误，JSON 解析失败 (invalid JSON)", "invalid_request_error")
		return
	}
	if body == nil {
		body = make(map[string]any)
	}

	// model 必填校验
	rawModel, _ := body["model"].(string)
	if strings.TrimSpace(rawModel) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "请求参数有误: 缺少必需字段 model (missing required field 'model')",
			"type":    "invalid_request_error", "code": 400, "param": "model",
		}})
		return
	}

	// 假流式前缀剥离（把剥离后的真名写回 body，使后续 ConvertChatRequest 用真名构建 payload）。
	actualModel, useFake := stripFakePrefix(rawModel)
	body["model"] = actualModel
	// ⚡ 注入模型名称
	cli.UpdateReqModel(vertex.RequestIDFromContext(r.Context()), actualModel)

	// force_no_stream：强制非流式。
	stream, _ := body["stream"].(bool)
	if stream && s.cfg.ForceNoStream {
		stream = false
	}

	model, geminiPayload, convErr := transform.ConvertChatRequest(body, s.cfg)
	if convErr != nil {
		s.oaiError(w, http.StatusBadRequest, "请求参数有误: "+convErr.Error()+" (invalid argument)", "invalid_request_error")
		return
	}

	// n 多候选解析：OpenAI n = 返回 n 个 choice。Gemini 3.x 不支持 candidateCount>1（实测 400），
	// 故 n>1 用并发扇出实现（见下方非流式分支）。上限来自配置 max_n（默认 8），防滥用放大上游 429/成本。
	// 流式取舍：真流式热路径全程硬编码 index 0，故流式仅支持 n=1（stream=true 且 n>1 直接 400）。
	n, nErr := resolveN(body["n"], s.cfg.MaxN)
	if nErr != "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": nErr, "type": "invalid_request_error", "code": 400, "param": "n",
		}})
		return
	}
	if stream && n > 1 {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "流式不支持 n>1，请设 stream=false 或 n=1 (streaming supports only n=1; set stream=false or n=1 for multiple choices)",
			"type":    "invalid_request_error", "code": 400, "param": "n",
		}})
		return
	}

	log.Printf("[Server] [ChatCompletions] 收到请求: 模型=%s, 真模型=%s, 流式=%v, n=%d", rawModel, actualModel, stream, n)

	// 图像分辨率控制（additive）：chat 端点也支持 image_size/imageSize/size/imageConfig 写入 imageConfig.imageSize。
	transform.ApplyImageConfig(geminiPayload, body)
	// 图像模型（生图/改图）经 chat 端点时补 imageConfig.imageSize 默认 1K，否则上游对 *-image 模型只返文字、不出图。
	// 仅对 *-image 模型生效；文本模型与已显式设过 imageConfig 的请求逐字节不变。
	if strings.Contains(strings.ToLower(model), "image") {
		gc, ok := geminiPayload["generationConfig"].(map[string]any)
		if !ok {
			gc = map[string]any{}
			geminiPayload["generationConfig"] = gc
		}
		ic, ok := gc["imageConfig"].(map[string]any)
		if !ok {
			ic = map[string]any{}
			gc["imageConfig"] = ic
		}
		if _, has := ic["imageSize"]; !has {
			ic["imageSize"] = "1K"
		}
	}

	// anti429 注入（在 image 处理之后、分发各分支之前）：
	// 开启 anti429_enabled 时往 systemInstruction/user content 前插随机数字串以缓解上游 429。
	// 这是所有 OpenAI 兼容渠道走的主端点，此前漏调（仅 TTS/Gemini 原生有），开启 anti429 时在主路径不生效。
	s.injectAnti429(geminiPayload)

	// 假流式（stream=true 且模型带假流式前缀）：先完整非流式生成，再切片按 OAI SSE 推。
	if stream && useFake {
		s.oaiFakeStream(r.Context(), w, model, geminiPayload)
		return
	}

	// 真流式：stream=true 走 SSE generator。
	if stream {
		s.streamChatCompletions(r.Context(), w, model, geminiPayload)
		return
	}

	// n>1：并发扇出聚合成 n 个 choice。
	if n > 1 {
		responses, vErr := s.vc.CompleteChatN(r.Context(), model, geminiPayload, n)
		if vErr != nil {
			ve := toVertexError(vErr)
			if isSafetyBlock(ve) {
				log.Printf("[Vertex] 请求被 Google 安全审查拦截, 请求ID=%s, 原因: %s", vertex.RequestIDFromContext(r.Context()), ve.Status)
				s.writeJSON(w, http.StatusOK, oaiSafetyResponse(model))
				return
			}
			s.writeJSON(w, ve.Code, vertexErrorToOAI(ve))
			return
		}
		s.writeJSON(w, http.StatusOK, transform.GeminiResponsesToOAIJSON(responses, model))
		return
	}

	geminiResp, vErr := s.vc.CompleteChat(r.Context(), model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			log.Printf("[Vertex] 请求被 Google 安全审查拦截, 请求ID=%s, 原因: %s", vertex.RequestIDFromContext(r.Context()), ve.Status)
			// 安全拦截以错误形式抛出时，返回 200 + content_filter（而非错误码）。
			s.writeJSON(w, http.StatusOK, oaiSafetyResponse(model))
			return
		}
		s.writeJSON(w, ve.Code, vertexErrorToOAI(ve))
		return
	}

	oaiResp := transform.GeminiJSONToOAIJSON(geminiResp, model)
	s.writeJSON(w, http.StatusOK, oaiResp)
}

// resolveN 解析并校验 n 参数。返回 (n, errMsg)；errMsg 非空表示 400。
// 缺省 1、非整数/<1/超上限 各自 400。
func resolveN(raw any, maxN int) (int, string) {
	if maxN <= 0 {
		maxN = 8
	}
	if raw == nil {
		return 1, ""
	}
	var n int
	switch v := raw.(type) {
	case float64:
		// JSON 数字。要求是整数值（5.5 这种拒绝）。
		if v != float64(int(v)) {
			return 0, "请求参数有误: n 必须是整数 (n must be an integer)"
		}
		n = int(v)
	case int:
		n = v
	default:
		return 0, "请求参数有误: n 必须是整数 (n must be an integer)"
	}
	if n < 1 {
		return 0, "请求参数有误: n 必须 >= 1 (n must be >= 1)"
	}
	if n > maxN {
		return 0, "请求参数有误: n 超过上限 " + strconv.Itoa(maxN) + " (n exceeds maximum " + strconv.Itoa(maxN) + ")"
	}
	return n, ""
}

// streamChatCompletions 处理真流式 /v1/chat/completions。
//
// SSE 流：逐个 data: {OAI chunk}\n\n，末尾 data: [DONE]\n\n。
// got_content 追踪：全程 0 内容 → 发 EmptyResponseError 事件（而非伪装正常空结束）。
// 上游报错 / 安全拦截分别处理，逐字节保持既定行为。
// ctx 来自 r.Context()：客户端断开时上游流与重试随之中止。
func (s *Server) streamChatCompletions(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := reqID24()

	flusher, canFlush := w.(http.Flusher)
	// SSE 响应头。X-Accel-Buffering:no 对经 nginx 的 SSE 防缓冲关键。
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// write 写一条 SSE 行并立即 flush（避免代理/客户端攒包）。返回 false 表示客户端断开。
	write := func(line string) bool {
		if _, err := io.WriteString(w, line); err != nil {
			log.Printf("[Server] [Stream] 请求ID=%s 客户端已主动断开连接", requestID)
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	isFirst := true
	hasFinish := false
	gotContent := false
	startTime := time.Now()

	s.vc.StreamChat(ctx, model, geminiPayload, func(ch vertex.StreamChunk) bool {
		if isFirst && ch.Err == nil {
			log.Printf("[Server] [Stream] 请求ID=%s 首字响应耗时: %.2fs", requestID, time.Since(startTime).Seconds())
			// ⚡ 变更为流式输出状态
			cli.UpdateReqState(requestID, "💬 流式打字", "\033[36m", "正在输出...")
		}
		// 错误 chunk（重试耗尽）：发 OAI error 事件 + [DONE] 后终止。
		if ch.Err != nil {
			s.writeStreamError(write, ch.Err, requestID, model)
			return false
		}
		events := transform.ConvertRealtimeChunk(ch.Data, model, requestID, isFirst)
		isFirst = false
		for _, ev := range events {
			// 据事件文本判断是否含真实 finish / 内容（用字符串匹配，避免重解析 JSON）。
			if strings.Contains(ev, `"finish_reason"`) && !strings.Contains(ev, `"finish_reason":null`) {
				hasFinish = true
			}
			if strings.Contains(ev, `"content":`) || strings.Contains(ev, `"tool_calls":`) || strings.Contains(ev, `"reasoning_content":`) {
				gotContent = true
			}
			if !write(ev) {
				return false // 客户端断开
			}
		}
		return true
	})

	writeSilent := func(line string) bool {
		if _, err := io.WriteString(w, line); err != nil {
			return false
		}
		if canFlush {
			flusher.Flush()
		}
		return true
	}

	// 流式正常结束后：空响应检测 + 兜底 finish + [DONE]。
	if !gotContent {
		// 上游 0-token 空回 → 明确报错让客户端重试，而非正常空结束（EmptyResponseError 分支）。
		ee := vertex.NewEmptyResponseError("Upstream returned empty response (no content)")
		s.writeStreamError(write, ee, requestID, model)
		return
	}
	if !hasFinish {
		base := s.streamChunkBase(model, requestID)
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}
		writeSilent(s.sseEvent(base))
	}
	writeSilent("data: [DONE]\n\n")
}

// writeStreamError 发一条 OAI 错误事件 + [DONE]。安全拦截 → finish_reason=content_filter（不报错）；
// 其余走友好错误返回（流式错误分支）。
func (s *Server) writeStreamError(write func(string) bool, e *vertex.VertexError, requestID, model string) {
	if isSafetyBlock(e) {
		base := s.streamChunkBase(model, requestID)
		base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "content_filter"}}
		_ = write(s.sseEvent(base))
	} else {
		_ = write(s.sseEvent(vertexErrorToOAI(e)))
	}
	_ = write("data: [DONE]\n\n")
}

// streamChunkBase 构造一个 OAI 流式 chunk 的公共字段。
func (s *Server) streamChunkBase(model, requestID string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + requestID,
		"object":  "chat.completion.chunk",
		"created": time.Now().Unix(),
		"model":   model,
	}
}

// sseEvent 把一个对象序列化为 SSE 数据行（关 HTML 转义，红线⑥）。
func (s *Server) sseEvent(obj map[string]any) string {
	data, err := jsonx.Marshal(obj)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + string(data) + "\n\n"
}

// oaiSafetyResponse 构造非流式 OAI 安全拦截响应：200 + content=null + finish_reason=content_filter
// （使安全拦截不以错误码返回而是干净的 content_filter）。
func oaiSafetyResponse(model string) map[string]any {
	return map[string]any{
		"id":      "chatcmpl-" + reqID24(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       map[string]any{"role": "assistant", "content": nil},
			"finish_reason": "content_filter",
		}},
	}
}

// isSafetyBlock 判断 VertexError 是否由上游安全拦截引起。
// 安全拦截走 content_filter（不报错），其余走友好错误。仅关键词匹配，不改状态。
func isSafetyBlock(e *vertex.VertexError) bool {
	msg := strings.ToLower(e.Message)
	status := strings.ToLower(e.Status)
	for _, k := range []string{"safety", "block_reason", "content_filter", "finish_reason_safety"} {
		if strings.Contains(msg, k) || strings.Contains(status, k) {
			return true
		}
	}
	return false
}

// reqID24 生成 uuid。
func reqID24() string {
	return uuid.New().String()
}

// ---- 错误映射 ----

// vertexErrorToOAI 把 VertexError 转为 OpenAI 错误响应体。
func vertexErrorToOAI(e *vertex.VertexError) map[string]any {
	var errType string
	switch e.Kind {
	case "invalid":
		errType = "invalid_request_error"
	case "ratelimit":
		errType = "rate_limit_error"
	case "auth":
		// recaptcha/token 临时问题：对外当服务端错误（配合 code 502），避免网关误判禁用渠道。
		errType = "server_error"
	case "notfound", "permission":
		errType = "invalid_request_error"
	default:
		errType = "server_error"
	}
	return map[string]any{"error": map[string]any{
		"message": withUpstreamDetail(vertex.FriendlyErrorMessage(e), e),
		"type":    errType,
		"code":    e.Code,
	}}
}

// withUpstreamDetail 在友好提示后附上上游真实原因。
func withUpstreamDetail(friendly string, e *vertex.VertexError) string {
	detail := strings.TrimSpace(e.Message)
	if detail == "" {
		detail = strings.TrimSpace(e.UpstreamResponse)
	}
	if detail == "" || strings.Contains(friendly, detail) {
		return friendly
	}
	if r := []rune(detail); len(r) > 400 {
		detail = string(r[:400]) + "…"
	}
	return friendly + "（上游原因：" + detail + "）"
}

func toVertexError(err error) *vertex.VertexError {
	if ve, ok := err.(*vertex.VertexError); ok {
		return ve
	}
	return vertex.NewInternalError(err.Error())
}

// ---- 中间件 ----

func (s *Server) withRecover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[Server] panic recovered: %v\n%s", rec, debug.Stack())
				s.oaiError(w, http.StatusInternalServerError, "服务内部错误，请联系开发者 (internal error)", "server_error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "*")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- request-id 上下文 ----

// statusWriter 包装 ResponseWriter 以捕获状态码；透传 Flush 以不破坏 SSE 流式。
type statusWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status = code
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true // 隐式 200
	}
	return w.ResponseWriter.Write(b) //nolint:wrapcheck
}

// Flush 透传底层 Flusher（流式 SSE 关键：handler 用 w.(http.Flusher) 断言需能命中）。
func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// withBodyLimit 在 config.max_request_mb>0 时给入站 body 套 MaxBytesReader（防绝对失控的安全阀）。
// 默认 0 = 不限，直接透传（不对合法大媒体取舍）。
func (s *Server) withBodyLimit(next http.Handler) http.Handler {
	limit := int64(s.cfg.MaxRequestMB) << 20
	if limit <= 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// withMetrics 为每个 API 请求生成 request-id、记录指标、打 access 日志。
// 跳过 /、/health、/metrics 及管理后台（非上游业务请求，不计入指标、不刷日志）。
func (s *Server) withMetrics(next http.Handler) http.Handler {
	skip := map[string]bool{"/": true, "/health": true, "/metrics": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if skip[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		reqID := reqID24()
		w.Header().Set("X-Request-Id", reqID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK} //nolint:exhaustruct
		ctx := context.WithValue(r.Context(), vertex.RequestIDKey{}, reqID)

		// ⚡ 开始追踪
		cli.StartReq(reqID)

		start := time.Now()
		s.metrics.StartRequest()
		next.ServeHTTP(sw, r.WithContext(ctx))
		elapsed := time.Since(start)

		// ⚡ 结束追踪
		cli.FinishReq(reqID)

		success := sw.status < 400
		s.metrics.EndRequest(success, elapsed.Seconds())
		s.metrics.RecordRequest(r.URL.Path, success, elapsed.Seconds(), start.Format("15:04:05"))
		log.Printf("[Server] %s %s - %d (%.3fs) 请求ID=%s", r.Method, r.URL.Path, sw.status, elapsed.Seconds(), reqID)
	})
}

// isAdminPath 报告 path 是否属于管理后台（静态面板或 JSON 接口），用于在业务中间件里整体跳过。
func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/") || strings.HasPrefix(path, "/api/admin/") || strings.HasPrefix(path, "/assets/")
}

func (s *Server) withAPIKey(next http.Handler) http.Handler {
	excluded := map[string]bool{"/": true, "/health": true, "/favicon.ico": true}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if excluded[r.URL.Path] || isAdminPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		key := extractAPIKey(r)
		if key == "" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "缺少 API 密钥 (missing API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if key == "sk-your-key-here" {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "示例密钥禁止调用，请新建密钥。", "status": "UNAUTHENTICATED",
			}})
			return
		}
		if !s.keys.ValidateKey(key) {
			s.writeJSON(w, http.StatusUnauthorized, map[string]any{"error": map[string]any{
				"code": 401, "message": "API 密钥无效 (invalid API key)", "status": "UNAUTHENTICATED",
			}})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 响应写出 ----

// writeJSON 用关 HTML 转义的编码器写 JSON 响应（红线⑥）。
func (s *Server) writeJSON(w http.ResponseWriter, status int, body any) {
	// 状态码钳制：上游 gRPC 错误的数字码（如 INTERNAL=13、UNKNOWN=2）可能被原样当作 HTTP 状态传进来，
	// 而 net/http 的 WriteHeader 对 <100 或 >599 的码会 panic「invalid WriteHeader code」。
	// 统一钳到合法区间，非法码当 500，避免单个上游错误码崩响应（root cause 见审计）。
	if status < 100 || status > 599 {
		status = http.StatusInternalServerError
	}
	data, err := jsonx.Marshal(body)
	if err != nil {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"序列化失败 (internal error)","type":"server_error","code":500}}`))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}

func (s *Server) oaiError(w http.ResponseWriter, status int, msg, errType string) {
	s.writeJSON(w, status, map[string]any{"error": map[string]any{
		"message": msg, "type": errType, "code": status,
	}})
}
