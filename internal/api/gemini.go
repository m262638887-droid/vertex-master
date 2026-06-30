// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/vertex"
)

// 本文件实现 Gemini 原生端点（透传 Gemini 请求）+ countTokens + 单模型详情的分发。
// 提供 stream_generate_content / generate_content / count_tokens / get_model_info。
// 路由形如 /v1beta/models/{model}:method 或 /v1beta/models/{model}。

// handleModelsSubtree 分发 /v1beta/models/ 与 /v1/models/ 下的请求：
//
//	GET  /v1[beta]/models/{model}                       → 单模型详情
//	POST /v1[beta]/models/{model}:generateContent       → Gemini 非流式
//	POST /v1[beta]/models/{model}:streamGenerateContent → Gemini 流式
//	POST /v1[beta]/models/{model}:countTokens           → token 计数
//
// {model} 可含点（如 gemini-2.5-flash），:method 是末段后缀，故手工解析而非静态 mux。
func (s *Server) handleModelsSubtree(w http.ResponseWriter, r *http.Request) {
	// 取出 /v1beta/models/ 或 /v1/models/ 之后的部分。
	var rest string
	switch {
	case strings.HasPrefix(r.URL.Path, "/v1beta/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1beta/models/")
	case strings.HasPrefix(r.URL.Path, "/v1/models/"):
		rest = strings.TrimPrefix(r.URL.Path, "/v1/models/")
	default:
		s.oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	if rest == "" {
		s.oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}

	// 拆出 model 与 :method（method 可空 = 单模型详情）。冒号取最后一个，模型名本身不含冒号。
	model := rest
	method := ""
	if idx := strings.LastIndex(rest, ":"); idx != -1 {
		model = rest[:idx]
		method = rest[idx+1:]
	}

	switch method {
	case "":
		if r.Method != http.MethodGet {
			s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
			return
		}
		s.handleModelInfo(w, model)
	case "generateContent":
		s.requirePost(w, r, func() { s.handleGeminiGenerate(w, r, model) })
	case "streamGenerateContent":
		s.requirePost(w, r, func() { s.handleGeminiStreamGenerate(w, r, model) })
	case "countTokens":
		s.requirePost(w, r, func() { s.handleCountTokens(w, r, model) })
	default:
		s.oaiError(w, http.StatusNotFound, "未知方法 "+method+" (unknown method)", "invalid_request_error")
	}
}

// requirePost 限定 POST，否则 405。
func (s *Server) requirePost(w http.ResponseWriter, r *http.Request, fn func()) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	fn()
}

// readGeminiBody 读取并解析 Gemini 端点请求体（JSON 对象）。返回 (body, ok)；ok=false 时已写出 400。
func (s *Server) readGeminiBody(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		if _, ok := err.(*json.SyntaxError); ok && strings.Contains(err.Error(), "invalid UTF-8") {
			s.geminiError(w, http.StatusBadRequest, "请求体编码错误，需为 UTF-8 (request body must be UTF-8 encoded)", "INVALID_ARGUMENT")
			return nil, false
		}
		s.geminiError(w, http.StatusBadRequest, "请求格式错误，JSON 解析失败 (invalid JSON)", "INVALID_ARGUMENT")
		return nil, false
	}
	if body == nil {
		body = make(map[string]any)
	}
	return body, true
}

// handleGeminiGenerate 处理 Gemini 非流式 generateContent（透传请求体作为 gemini_payload）。
// 安全拦截返回 200 + 空 candidates + promptFeedback（不报错）。
func (s *Server) handleGeminiGenerate(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, _ := stripFakePrefix(model)
	body, ok := s.readGeminiBody(w, r)
	if !ok {
		return
	}
	// 兼容 generateContentRequest 包裹（某些 SDK 如 google-genai-sdk 会发这种格式）。
	if reqObj, ok2 := body["generateContentRequest"].(map[string]any); ok2 { //nolint:govet
		body = reqObj
	}
	log.Printf("[Server] [GeminiGenerate] 收到请求: 模型=%s, 真模型=%s", model, actualModel)
	s.injectAnti429(body)

	resp, vErr := s.vc.CompleteChat(r.Context(), actualModel, body)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			s.writeJSON(w, http.StatusOK, geminiSafetyResponse(ve))
			return
		}
		s.writeJSON(w, ve.Code, vertexErrorToGemini(ve))
		return
	}
	s.writeJSON(w, http.StatusOK, resp)
}

// handleGeminiStreamGenerate 处理 Gemini 流式 streamGenerateContent。
// 含 use_fake 假流式分支。
func (s *Server) handleGeminiStreamGenerate(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, useFake := stripFakePrefix(model)
	body, ok := s.readGeminiBody(w, r)
	if !ok {
		return
	}
	// 兼容 generateContentRequest 包裹（某些 SDK 如 google-genai-sdk 会发这种格式）。
	if reqObj, ok2 := body["generateContentRequest"].(map[string]any); ok2 { //nolint:govet
		body = reqObj
	}
	log.Printf("[Server] [GeminiStreamGenerate] 收到请求: 模型=%s, 真模型=%s, 假流式=%v", model, actualModel, useFake)
	s.injectAnti429(body)

	sw := s.newSSEWriter(w, "text/event-stream")

	if useFake {
		s.geminiFakeStream(r.Context(), sw, actualModel, body)
		return
	}

	gotChunk := false
	hasFinish := false
	s.vc.StreamChat(r.Context(), actualModel, body, func(ch vertex.StreamChunk) bool {
		if ch.Err != nil {
			// 安全拦截走 Gemini 标准格式（空 candidates + promptFeedback.blockReason）；其余走 error 事件。
			if isSafetyBlock(ch.Err) {
				_ = sw.write(s.geminiSSE(geminiSafetyChunk(ch.Err)))
			} else {
				_ = sw.write(s.geminiSSE(map[string]any{"error": map[string]any{
					"code": ch.Err.Code, "message": vertex.FriendlyErrorMessage(ch.Err), "status": geminiStatusOf(ch.Err),
				}}))
			}
			return false
		}
		gotChunk = true
		if fr := chunkFinishReasonFromData(ch.Data); fr != "" {
			hasFinish = true
		}
		return sw.write(s.geminiSSE(ch.Data))
	})

	if !gotChunk {
		_ = sw.write(s.geminiSSE(map[string]any{
			"error": map[string]any{
				"code": 500, "message": "Upstream returned empty response (no content)", "status": "INTERNAL",
			},
		}))
		return
	}
	if !hasFinish {
		_ = sw.write(s.geminiSSE(map[string]any{
			"candidates": []any{map[string]any{
				"content":      map[string]any{"parts": []any{}, "role": "model"},
				"finishReason": "STOP",
				"index":        0,
			}},
		}))
	}
}

// geminiFakeStream 处理 Gemini 假流式：完整非流式生成 → 抽文本 → 切片按 Gemini chunk 推。
// 切片大小 = max(1, len/8)。
func (s *Server) geminiFakeStream(ctx context.Context, sw *sseWriter, model string, body map[string]any) {
	resp, vErr := s.vc.CompleteChat(ctx, model, body)
	if vErr != nil {
		ve := toVertexError(vErr)
		if isSafetyBlock(ve) {
			_ = sw.write(s.geminiSSE(geminiSafetyChunk(ve)))
			return
		}
		_ = sw.write(s.geminiSSE(map[string]any{"error": map[string]any{
			"code": ve.Code, "message": vertex.FriendlyErrorMessage(ve), "status": geminiStatusOf(ve),
		}}))
		return
	}

	text := geminiResponseText(resp)
	chunks := splitIntoRuneChunks(text)
	for i, piece := range chunks {
		cand := map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": piece}}}}
		if i == len(chunks)-1 {
			cand["finishReason"] = "STOP"
		}
		chunk := map[string]any{"candidates": []any{cand}}
		if !sw.write(s.geminiSSE(chunk)) {
			return
		}
	}
}

// handleCountTokens 处理 Gemini countTokens（读 generateContentRequest.contents 或顶层 contents）。
func (s *Server) handleCountTokens(w http.ResponseWriter, r *http.Request, model string) {
	actualModel, _ := stripFakePrefix(model)
	body, ok := s.readGeminiBody(w, r)
	if !ok {
		return
	}
	log.Printf("[Server] [CountTokens] 收到请求: 模型=%s, 真模型=%s", model, actualModel)

	var contents []any
	if reqObj, ok2 := body["generateContentRequest"].(map[string]any); ok2 { //nolint:govet
		contents, _ = reqObj["contents"].([]any)
	} else {
		contents, _ = body["contents"].([]any)
	}
	if contents == nil {
		contents = []any{}
	}

	total := s.vc.CountTokens(r.Context(), actualModel, contents)
	s.writeJSON(w, http.StatusOK, map[string]any{"totalTokens": total})
}

// ---- Gemini 响应/错误辅助 ----

// chunkFinishReasonFromData 从 Gemini chunk data 中提取 candidates[0].finishReason。
func chunkFinishReasonFromData(data map[string]any) string {
	cands, ok := data["candidates"].([]any)
	if !ok || len(cands) == 0 {
		return ""
	}
	c, ok := cands[0].(map[string]any)
	if !ok {
		return ""
	}
	fr, _ := c["finishReason"].(string)
	return fr
}

// geminiSSE 把对象序列化为一条 Gemini SSE 行（data: {json}\n\n，关 HTML 转义）。
func (s *Server) geminiSSE(obj map[string]any) string {
	data, err := jsonx.Marshal(obj)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + string(data) + "\n\n"
}

// geminiError 写出 Gemini 风格错误响应（InvalidArgumentError 等）。
func (s *Server) geminiError(w http.ResponseWriter, status int, msg, geminiStatus string) {
	s.writeJSON(w, status, map[string]any{"error": map[string]any{
		"code": status, "message": msg, "status": geminiStatus,
	}})
}

// vertexErrorToGemini 把 VertexError 转为 Gemini 错误响应体（friendly 提示 + status）。
func vertexErrorToGemini(e *vertex.VertexError) map[string]any {
	msg := vertex.FriendlyErrorMessage(e)
	if e.Message != "" {
		msg += " | Raw: " + e.Message
	}
	if e.UpstreamResponse != "" {
		msg += " | Upstream: " + e.UpstreamResponse
	}
	return map[string]any{"error": map[string]any{
		"code": e.Code, "message": msg, "status": geminiStatusOf(e),
	}}
}

// geminiStatusOf 返回 Gemini 错误响应的 status 字段：优先用 VertexError 的真实 gRPC 状态
// （INVALID_ARGUMENT/RESOURCE_EXHAUSTED/UNAUTHENTICATED 等），为空时回退 INTERNAL。
func geminiStatusOf(e *vertex.VertexError) string {
	if e != nil && e.Status != "" {
		return e.Status
	}
	return "INTERNAL"
}

// geminiSafetyResponse 安全拦截的非流式 Gemini 标准响应（200 + 空 candidates + promptFeedback）。
// 用于 generateContent 的安全拦截分支。
func geminiSafetyResponse(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if blockReason == "" {
		blockReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

// geminiSafetyChunk 安全拦截的流式 Gemini 标准 chunk（空 candidates parts + promptFeedback）。
// 用于流式生成的安全拦截分支。
func geminiSafetyChunk(e *vertex.VertexError) map[string]any {
	blockReason := e.Status
	if blockReason == "" {
		blockReason = "SAFETY"
	}
	return map[string]any{
		"candidates": []any{map[string]any{
			"content":       map[string]any{"parts": []any{}, "role": "model"},
			"finishReason":  "SAFETY",
			"safetyRatings": []any{},
			"index":         0,
		}},
		"promptFeedback": map[string]any{
			"blockReason":        blockReason,
			"safetyRatings":      []any{},
			"blockReasonMessage": e.Message,
		},
	}
}

// geminiResponseText 从 Gemini 响应抽取纯文本（跳过 thought part），供假流式切片用。
// 用于假流式分支的文本拼接。
func geminiResponseText(resp map[string]any) string {
	var sb strings.Builder
	cands, _ := resp["candidates"].([]any)
	for _, cRaw := range cands {
		c, ok := cRaw.(map[string]any)
		if !ok {
			continue
		}
		content, ok := c["content"].(map[string]any)
		if !ok {
			continue
		}
		parts, _ := content["parts"].([]any)
		for _, pRaw := range parts {
			p, ok2 := pRaw.(map[string]any) //nolint:govet
			if !ok2 {
				continue
			}
			if isTruthyAny(p["thought"]) {
				continue
			}
			if t, ok3 := p["text"].(string); ok3 { //nolint:govet
				sb.WriteString(t)
			}
		}
	}
	return sb.String()
}

// isTruthyAny 委托 jsonx.Truthy（统一真值语义，见 jsonx.Truthy）。
func isTruthyAny(v any) bool { return jsonx.Truthy(v) }
