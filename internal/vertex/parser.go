// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package vertex

import (
	"encoding/json"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

// ParseResult 是 batchGraphql 响应的解析结果（解析状态）。
type ParseResult struct { //nolint:govet
	Parts             []map[string]any
	FinishReason      string
	FinishMessage     any
	SafetyRatings     any
	CitationMetadata  any
	GroundingMetadata any
	TokenCount        any
	AvgLogprobs       any
	LogprobsResult    any
	CandidateIndex    int
	PromptFeedback    map[string]any
	UsageMetadata     map[string]any
	CreateTime        any
	ModelVersion      any
	ResponseID        any
	ModelStatus       any
	HasError          bool
	ErrorMessage      string
	ErrorObj          *VertexError
}

var trailingCommaBeforeBracket = regexp.MustCompile(`,\s*\]`)

// ParseUpstreamData 解析完整的上游原始数据。
func ParseUpstreamData(raw string) *ParseResult {
	s := &ParseResult{
		PromptFeedback: map[string]any{},
		UsageMetadata:  map[string]any{},
	}
	partsByPath := map[int]map[string]any{}
	var unindexed []map[string]any

	cleaned := cleanJSONString(raw)
	var dataList []any
	if err := json.Unmarshal([]byte(cleaned), &dataList); err != nil {
		// 顶层不是数组 → 尝试当单个对象包进数组；仍失败则记为 JSON 解析错误。
		var single any
		if err2 := json.Unmarshal([]byte(cleaned), &single); err2 == nil {
			dataList = []any{single}
		} else {
			s.HasError = true
			s.ErrorMessage = "JSON parse error: " + err.Error()
			s.Parts = []map[string]any{}
			return s
		}
	}

	for _, itemRaw := range dataList {
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}

		// 1. 统一错误解析。"Failed to verify action" 是匿名首帧预期错误，忽略不置 HasError，
		//    交由上层首次认证重试逻辑处理。
		if parsed := parseErrorResponse(item); parsed != nil {
			if strings.Contains(parsed.Message, "Failed to verify action") {
				// 忽略
			} else if !s.HasError {
				s.HasError = true
				s.ErrorMessage = parsed.Message
				s.ErrorObj = parsed
			}
		}

		// 2. 顶层错误兜底
		if msg := extractErrorMessage(item); msg != "" && !s.HasError {
			s.HasError = true
			s.ErrorMessage = msg
		}

		// 3. results
		results, ok := item["results"].([]any)
		if !ok {
			continue
		}
		typedResults := make([]map[string]any, 0, len(results))
		for _, r := range results {
			if rm, ok := r.(map[string]any); ok {
				typedResults = append(typedResults, rm)
			}
		}

		// results 中的错误（使用统一解析）
		if parsed := parseErrorResponse(toAnySlice(typedResults)); parsed != nil {
			s.HasError = true
			s.ErrorMessage = parsed.Message
			s.ErrorObj = parsed
		}

		// 4. 提取数据 parts
		for _, result := range typedResults {
			if result["data"] == nil {
				if _, hasErrs := result["errors"]; hasErrs {
					continue // data=null 且有 errors，已被上面捕获
				}
			}
			pathIndex := extractPathIndex(result)
			if data, ok := result["data"].(map[string]any); ok {
				updateStateFromData(s, partsByPath, &unindexed, data, pathIndex)
			}
		}
	}

	// 组装 parts：parts_by_path 按 int key 升序 + unindexed 追加，再合并相邻同类型块。
	keys := make([]int, 0, len(partsByPath))
	for k := range partsByPath {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	ordered := make([]map[string]any, 0, len(keys)+len(unindexed))
	for _, k := range keys {
		ordered = append(ordered, partsByPath[k])
	}
	ordered = append(ordered, unindexed...)

	s.Parts = transform.MergeContentBlocks(ordered)
	return s
}

// cleanJSONString 清理并规范化 JSON 字符串。
func cleanJSONString(raw string) string {
	cleaned := strings.TrimSpace(raw)
	if cleaned == "" {
		return "[]"
	}
	cleaned = strings.TrimSuffix(cleaned, ",")
	cleaned = trailingCommaBeforeBracket.ReplaceAllString(cleaned, "]")
	if !strings.HasPrefix(cleaned, "[") {
		cleaned = "[" + cleaned + "]"
	} else if !strings.HasSuffix(cleaned, "]") {
		if !strings.Contains(cleaned, "}]") {
			cleaned += "]"
		}
	}
	return cleaned
}

// extractPathIndex 从 result 的 path 中倒序取第一个整数索引。
func extractPathIndex(result map[string]any) int {
	path, ok := result["path"].([]any)
	if !ok || len(path) == 0 {
		return -1
	}
	for i := len(path) - 1; i >= 0; i-- {
		switch v := path[i].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case string:
			if n, err := strconv.Atoi(v); err == nil {
				return n
			}
		}
	}
	return -1
}

// processCandidateMetadata 提取 candidate 级元数据。
func processCandidateMetadata(c map[string]any) map[string]any {
	meta := map[string]any{}
	if fr := c["finishReason"]; isTruthyAny(fr) {
		meta["finish_reason"] = fr
	}
	if v, ok := c["finishMessage"]; ok {
		meta["finish_message"] = v
	}
	if v := c["safetyRatings"]; isTruthyAny(v) {
		meta["safety_ratings"] = v
	}
	if v := c["citationMetadata"]; isTruthyAny(v) {
		meta["citation_metadata"] = v
	}
	if v := c["groundingMetadata"]; isTruthyAny(v) {
		meta["grounding_metadata"] = v
	}
	if v, ok := c["tokenCount"]; ok {
		meta["token_count"] = v
	}
	if v, ok := c["avgLogprobs"]; ok {
		meta["avg_logprobs"] = v
	}
	if v, ok := c["logprobsResult"]; ok {
		meta["logprobs_result"] = v
	}
	if v := c["index"]; v != nil {
		meta["candidate_index"] = v
	}
	return meta
}

// updateStateFromData 从数据对象更新解析状态。
func updateStateFromData(s *ParseResult, partsByPath map[int]map[string]any, unindexed *[]map[string]any, data map[string]any, pathIndex int) {
	// 匿名 batchGraphql 把 Gemini 载荷包在 data["ui"]["streamGenerateContentAnonymous"] 下，
	// 需先 unwrap，否则 usageMetadata/candidates 取不到。
	if ui, ok := data["ui"].(map[string]any); ok {
		if inner, ok := ui["streamGenerateContentAnonymous"].(map[string]any); ok {
			data = inner
		}
	}

	if pf, ok := data["promptFeedback"].(map[string]any); ok && len(pf) > 0 {
		s.PromptFeedback = pf
	}
	if um, ok := data["usageMetadata"]; ok {
		// usageMetadata 取最后一个非空帧：batchGraphql 多帧流里只有末帧带累计 usage，
		// 但个别中间帧可能携带空对象，空帧不得覆盖已收集的真实统计（修掉旧实现的 usage=None 限制）。
		if m := toMap(um); len(m) > 0 {
			s.UsageMetadata = m
		}
	}
	if v, ok := data["createTime"]; ok {
		s.CreateTime = v
	}
	if v, ok := data["modelVersion"]; ok {
		s.ModelVersion = v
	}
	if v, ok := data["responseId"]; ok {
		s.ResponseID = v
	}
	if v, ok := data["modelStatus"]; ok {
		s.ModelStatus = v
	}

	candidates, _ := data["candidates"].([]any)
	for _, cRaw := range candidates {
		c, ok := cRaw.(map[string]any)
		if !ok {
			continue
		}
		applyMeta(s, processCandidateMetadata(c))
		content, _ := c["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for _, pRaw := range parts {
			if p, ok := pRaw.(map[string]any); ok {
				if pathIndex != -1 {
					partsByPath[pathIndex] = p
				} else {
					*unindexed = append(*unindexed, p)
				}
			}
		}
	}
}

// applyMeta 把 processCandidateMetadata 的结果写入 state（仅非 nil/非空容器才覆盖）。
func applyMeta(s *ParseResult, meta map[string]any) {
	for k, v := range meta {
		if v == nil || isEmptyContainer(v) {
			continue
		}
		switch k {
		case "finish_reason":
			s.FinishReason = toStr(v)
		case "finish_message":
			s.FinishMessage = v
		case "safety_ratings":
			s.SafetyRatings = v
		case "citation_metadata":
			s.CitationMetadata = v
		case "grounding_metadata":
			s.GroundingMetadata = v
		case "token_count":
			s.TokenCount = v
		case "avg_logprobs":
			s.AvgLogprobs = v
		case "logprobs_result":
			s.LogprobsResult = v
		case "candidate_index":
			s.CandidateIndex = toInt(v, 0)
		}
	}
}

// extractErrorMessage 从单个响应项提取错误信息。
func extractErrorMessage(item map[string]any) string {
	if errObj, ok := item["error"]; ok && errObj != nil {
		if m, ok := errObj.(map[string]any); ok {
			return toStrOr(m["message"], marshalStr(m))
		}
		return toStr(errObj)
	}
	if errs, ok := item["errors"].([]any); ok && len(errs) > 0 {
		if m, ok := errs[0].(map[string]any); ok {
			return toStrOr(m["message"], marshalStr(m))
		}
		return marshalStr(errs[0])
	}
	return ""
}

// ---- 小工具 ----

// isTruthyAny 委托 jsonx.Truthy（统一真值语义，见 jsonx.Truthy）。
func isTruthyAny(v any) bool { return jsonx.Truthy(v) }

func isEmptyContainer(v any) bool {
	switch x := v.(type) {
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

func toAnySlice(ms []map[string]any) []any {
	out := make([]any, len(ms))
	for i, m := range ms {
		out[i] = m
	}
	return out
}
