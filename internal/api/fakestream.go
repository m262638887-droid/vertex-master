// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/transform"
)

// 本文件实现假流式：模型名带 "假流式-"/"fake-" 前缀时，先完整非流式生成、再切片按 SSE 推。
// OpenAI 端点与 Gemini 端点（use_fake 分支）共用此机制。

// splitIntoRuneChunks 把文本切成若干分片用于假流式推送，分片数约为 8。
//
// 必须按 rune（完整字符）切分，不能按字节：多字节 UTF-8 字符（如汉字、emoji）若在字节
// 边界被截断，半个字符经 JSON 序列化会被替换成 U+FFFD（），客户端收到的就是乱码。
// 空文本返回 nil。
func splitIntoRuneChunks(text string) []string {
	runes := []rune(text)
	if len(runes) == 0 {
		return nil
	}
	chunkSize := 1
	if cs := len(runes) / 8; cs > 1 {
		chunkSize = cs
	}
	chunks := make([]string, 0, (len(runes)+chunkSize-1)/chunkSize)
	for i := 0; i < len(runes); i += chunkSize {
		end := i + chunkSize
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}

// sseWriter 是一个带 flush 的 SSE 行写出器；write 返回 false 表示客户端断开。
type sseWriter struct {
	w     http.ResponseWriter
	flush func()
}

// newSSEWriter 写出标准 SSE 响应头并返回写出器。
// contentType 通常是 text/event-stream（OAI）或 application/json（Gemini 流，沿用既定 media_type）。
func (s *Server) newSSEWriter(w http.ResponseWriter, contentType string) *sseWriter {
	flusher, _ := w.(http.Flusher)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	sw := &sseWriter{w: w} //nolint:exhaustruct
	if flusher != nil {
		sw.flush = flusher.Flush
	}
	return sw
}

// write 写一条原始字符串并 flush。返回 false 表示客户端断开。
func (sw *sseWriter) write(line string) bool {
	if _, err := sw.w.Write([]byte(line)); err != nil {
		return false
	}
	if sw.flush != nil {
		sw.flush()
	}
	return true
}

// oaiFakeStream 处理 OpenAI 假流式：完整非流式生成 → 转 OAI → 把 content 文本切片按 chunk 推。
// 切片大小 = max(1, len/8)。
func (s *Server) oaiFakeStream(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any) {
	requestID := reqID24()
	sw := s.newSSEWriter(w, "text/event-stream")

	resp, vErr := s.vc.CompleteChat(ctx, model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		s.writeStreamError(sw.write, ve, requestID, model)
		return
	}

	oai := transform.GeminiJSONToOAIJSON(resp, model)
	contentText := firstChoiceContent(oai)

	createdTS := time.Now().Unix()
	chunks := splitIntoRuneChunks(contentText)
	for i, piece := range chunks {
		base := s.streamChunkBase(model, requestID)
		base["created"] = createdTS
		var delta map[string]any
		if i == 0 {
			delta = map[string]any{"role": "assistant", "content": piece}
		} else {
			delta = map[string]any{"content": piece}
		}
		choice := map[string]any{"index": 0, "delta": delta}
		if i == len(chunks)-1 {
			choice["finish_reason"] = "stop"
		}
		base["choices"] = []any{choice}
		if !sw.write(s.sseEvent(base)) {
			return
		}
	}
	_ = sw.write("data: [DONE]\n\n")
}

// firstChoiceContent 取 OAI 响应 choices[0].message.content（非字符串/缺失返 ""）。
func firstChoiceContent(oai map[string]any) string {
	choices, ok := oai["choices"].([]any)
	if !ok || len(choices) == 0 {
		return ""
	}
	choice, ok := choices[0].(map[string]any)
	if !ok {
		return ""
	}
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return ""
	}
	if c, ok2 := msg["content"].(string); ok2 { //nolint:govet
		return c
	}
	return ""
}
