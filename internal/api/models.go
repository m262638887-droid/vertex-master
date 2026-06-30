// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// 本文件实现模型清单端点（list_models_oai / list_models_gemini / get_model_info）
// + 假流式前缀剥离工具。

// stripFakePrefix 检测并剥离假流式前缀，返回 (实际模型名, 是否假流式)。
func stripFakePrefix(model string) (string, bool) {
	for _, p := range config.FakePrefixes() {
		if strings.HasPrefix(model, p) {
			return model[len(p):], true
		}
	}
	return model, false
}

// supportedGenerationMethods 返回模型详情里声明的支持方法（本代理统一支持这三种）。
func supportedGenerationMethods() []any {
	return []any{"generateContent", "streamGenerateContent", "countTokens"}
}

// handleModelsOAI 返回 OpenAI 格式模型清单。
func (s *Server) handleModelsOAI(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	models := config.ModelsWithFakeVariants()
	log.Printf("[Server] [Models] 请求 OAI 模型列表，返回 %d 个模型", len(models))
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, map[string]any{
			"id": m, "object": "model", "created": now, "owned_by": "google", "permission": []any{},
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

// handleModelsGemini 返回 Gemini 格式模型清单（含 token 限额 + supportedGenerationMethods）。
// 与 OAI 清单一致包含假流式变体，使单模型 404 校验自洽。
func (s *Server) handleModelsGemini(w http.ResponseWriter, r *http.Request) {
	models := config.ModelsWithFakeVariants()
	data := make([]any, 0, len(models))
	for _, m := range models {
		data = append(data, geminiModelInfo(m))
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"models": data})
}

// handleModelInfo 返回单模型详情（Gemini 兼容格式）。
// 404 校验兼容假流式前缀变体名（成员校验即可命中）。
// modelName 已由路由解析出（去掉 :method 后缀、可能含 models/ 前缀）。
func (s *Server) handleModelInfo(w http.ResponseWriter, modelName string) {
	name := strings.TrimPrefix(modelName, "models/")
	known := false
	for _, m := range config.ModelsWithFakeVariants() {
		if m == name {
			known = true
			break
		}
	}
	if !known {
		s.writeJSON(w, http.StatusNotFound, map[string]any{"error": map[string]any{
			"code": 404, "message": "Model '" + modelName + "' not found.", "status": "NOT_FOUND",
		}})
		return
	}
	s.writeJSON(w, http.StatusOK, geminiModelInfo(name))
}

// geminiModelInfo 构造单个 Gemini 模型详情对象（供 get_model_info / list_models_gemini 用）。
func geminiModelInfo(name string) map[string]any {
	return map[string]any{
		"name":                       "models/" + name,
		"version":                    name,
		"displayName":                name,
		"description":                "Vertex AI Studio anonymous model",
		"inputTokenLimit":            1048576,
		"outputTokenLimit":           65536,
		"supportedGenerationMethods": supportedGenerationMethods(),
	}
}
