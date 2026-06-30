// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/jsonx"
	"github.com/google/uuid"
)

// FinishReasonMap 把 Gemini finishReason 映射到 OpenAI finish_reason。
// 未命中（含 FINISH_REASON_UNSPECIFIED）→ "stop"。
var FinishReasonMap = map[string]string{ //nolint:gochecknoglobals
	"STOP":                    "stop",
	"MAX_TOKENS":              "length",
	"SAFETY":                  "content_filter",
	"RECITATION":              "content_filter",
	"PROHIBITED_CONTENT":      "content_filter",
	"TOOL_CALLS":              "tool_calls",
	"MALFORMED_FUNCTION_CALL": "tool_calls",
	"BLOCKLIST":               "content_filter",
	"SPII":                    "content_filter",
	"OTHER":                   "stop",
}

// reqID 生成 uuid。
func reqID() string {
	return uuid.New().String()
}

// MapFinishReason 把 Gemini finishReason 转 OpenAI finish_reason。
// 有工具调用统一 tool_calls；空/未知默认 stop。注意：调用方需自行过滤
// FINISH_REASON_UNSPECIFIED（流式血泪教训），本函数不负责。
func MapFinishReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_calls"
	}
	if finish == "" {
		return "stop"
	}
	if v, ok := FinishReasonMap[strings.ToUpper(finish)]; ok {
		return v
	}
	return "stop"
}

// GeminiJSONToOAIJSON 把 Gemini 非流式响应转为 OpenAI ChatCompletion JSON。
func GeminiJSONToOAIJSON(geminiResp map[string]any, model string) map[string]any {
	candidate := firstCandidate(geminiResp)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	text, toolCalls, reasoning := ExtractParts(parts, false)

	var oaiFinish string
	if finish != "" {
		oaiFinish = MapFinishReason(finish, len(toolCalls) > 0)
	} else if len(toolCalls) > 0 {
		oaiFinish = "tool_calls"
	} else {
		oaiFinish = "stop"
	}

	message := map[string]any{"role": "assistant"}
	if text != "" {
		message["content"] = text
	} else {
		message["content"] = nil // content 为空时显式 null（对齐 OpenAI）
	}
	if len(toolCalls) > 0 {
		message["tool_calls"] = toolCalls
	}
	if reasoning != "" {
		message["reasoning_content"] = reasoning
	}

	result := map[string]any{
		"id":      "chatcmpl-" + reqID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       message,
			"finish_reason": oaiFinish,
		}},
	}
	if usageMeta, ok := geminiResp["usageMetadata"].(map[string]any); ok {
		result["usage"] = ConvertUsage(usageMeta)
	}
	return result
}

// ExtractParts 从 Gemini parts 提取 (text_content, tool_calls, reasoning_content)。
// inlineData(image/*) 以 markdown data-URI 追加到
// text_content；executableCode / codeExecutionResult 渲染成 markdown 代码块。
func ExtractParts(parts []any, forStream bool) (string, []any, string) {
	var texts []string
	var thoughts []string
	var toolCalls []any
	var images []string

	for _, pRaw := range parts {
		part, ok := pRaw.(map[string]any)
		if !ok {
			continue
		}
		// 非空文本（而非键存在）：上游流式每个 part 会带上所有字段的空默认值
		// （text:"" + 空 inlineData/functionCall），靠真实非空字段区分类型。必须按"值非空"
		// 判断，否则带 text:"" 的工具/图片帧会被误判成空文本而丢弃（流式下 functionCall/
		// inlineData 丢失的根因）。故先判非空 functionCall/inlineData，再判非空 text。
		hasText := toString(part["text"]) != ""
		isThought := isTruthy(part["thought"])

		switch {
		case isFunctionCallWithName(part):
			fc, _ := part["functionCall"].(map[string]any)
			args := fc["args"]
			if args == nil {
				args = map[string]any{}
			}
			argBytes, _ := jsonx.Marshal(args)
			tc := map[string]any{
				"index": len(toolCalls),
				"id":    "call_" + reqID(),
				"type":  "function",
				"function": map[string]any{
					"name":      toString(fc["name"]),
					"arguments": string(argBytes),
				},
			}
			if !forStream {
				delete(tc, "index") // 非流式不带 index
			}
			toolCalls = append(toolCalls, tc)
		case hasInlineImage(part):
			id, _ := part["inlineData"].(map[string]any)
			mime := toString(firstNonEmpty(id["mimeType"], id["mime_type"]))
			data := toString(id["data"])
			images = append(images, "\n![image](data:"+mime+";base64,"+data+")")
		case isThought && hasText:
			thoughts = append(thoughts, toString(part["text"]))
		case hasText:
			texts = append(texts, toString(part["text"]))
		case hasKey(part, "executableCode"):
			if ec, ok := part["executableCode"].(map[string]any); ok {
				lang := strings.ToLower(toString(ec["codeLanguage"]))
				texts = append(texts, "```"+lang+"\n"+toString(ec["code"])+"\n```")
			}
		case hasKey(part, "codeExecutionResult"):
			if cer, ok := part["codeExecutionResult"].(map[string]any); ok {
				texts = append(texts, "```output\n"+toString(cer["output"])+"\n```")
			}
		}
	}

	textContent := strings.Join(texts, "") + strings.Join(images, "")
	reasoning := strings.Join(thoughts, "")
	if len(toolCalls) == 0 {
		return textContent, nil, reasoning
	}
	return textContent, toolCalls, reasoning
}

// ConvertUsage 把 Gemini usageMetadata 转 OpenAI usage。
// 含 prompt/completion 细分 token 统计。
func ConvertUsage(meta map[string]any) map[string]any {
	prompt := numOf(meta["promptTokenCount"]) + numOf(meta["toolUsePromptTokenCount"])
	completion := numOf(meta["candidatesTokenCount"]) + numOf(meta["thoughtsTokenCount"])
	total := prompt + completion
	if _, ok := meta["totalTokenCount"]; ok {
		total = numOf(meta["totalTokenCount"])
	}
	result := map[string]any{
		"prompt_tokens":     prompt,
		"completion_tokens": completion,
		"total_tokens":      total,
	}

	promptDetails := map[string]any{}
	if c := numOf(meta["cachedContentTokenCount"]); c > 0 {
		promptDetails["cached_tokens"] = c
	}
	for _, d := range asMapSlice(meta["promptTokensDetails"]) {
		count := numOf(d["tokenCount"])
		switch toString(d["modality"]) {
		case "AUDIO":
			promptDetails["audio_tokens"] = numOf(promptDetails["audio_tokens"]) + count
		case "TEXT":
			promptDetails["text_tokens"] = numOf(promptDetails["text_tokens"]) + count
		}
	}
	if len(promptDetails) > 0 {
		result["prompt_tokens_details"] = promptDetails
	}

	completionDetails := map[string]any{}
	if t := numOf(meta["thoughtsTokenCount"]); t > 0 {
		completionDetails["reasoning_tokens"] = t
	}
	for _, d := range asMapSlice(meta["candidatesTokensDetails"]) {
		count := numOf(d["tokenCount"])
		switch toString(d["modality"]) {
		case "IMAGE":
			completionDetails["image_tokens"] = numOf(completionDetails["image_tokens"]) + count
		case "AUDIO":
			completionDetails["audio_tokens"] = numOf(completionDetails["audio_tokens"]) + count
		case "TEXT":
			completionDetails["text_tokens"] = numOf(completionDetails["text_tokens"]) + count
		}
	}
	if len(completionDetails) > 0 {
		result["completion_tokens_details"] = completionDetails
	}

	return result
}

// ConvertRealtimeChunk 把单个 Gemini 增量 dict 转为 OAI SSE 事件字符串列表（真流式用）。
//
// 红线：finishReason 过滤（红线⑤）：仅当 finishReason 是真实终止值（STOP/MAX_TOKENS/SAFETY...）
// 才发 finish 事件；匿名 batchGraphql 每帧都带 FINISH_REASON_UNSPECIFIED（protobuf 默认值、
// 无意义），绝不能据它发 finish_reason，否则客户端首帧就提前停止 → 截断（血泪教训）。
// 真实 finishReason 常与最后一段文本同帧到达，故不限 not parts（内容事件已在上方先发）。
func ConvertRealtimeChunk(chunk map[string]any, model, requestID string, isFirst bool) []string {
	candidate := firstCandidate(chunk)
	parts := candidateParts(candidate)
	finish, _ := candidate["finishReason"].(string)

	created := time.Now().Unix()
	base := func() map[string]any {
		return map[string]any{
			"id":      "chatcmpl-" + requestID,
			"object":  "chat.completion.chunk",
			"created": created,
			"model":   model,
		}
	}
	var events []string

	if isFirst {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}

	text, toolCalls, reasoning := ExtractParts(parts, true)

	if reasoning != "" {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": reasoning}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}
	if text != "" {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"content": text}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}
	if len(toolCalls) > 0 {
		b := base()
		b["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": toolCalls}, "finish_reason": nil}}
		events = append(events, sseLine(b))
	}

	// 收尾 finish 事件：仅对真实 finishReason（已过滤空 / FINISH_REASON_UNSPECIFIED）。
	if finish != "" && finish != FinishReasonUnspecified {
		oaiFinish := MapFinishReason(finish, len(toolCalls) > 0)
		finishEvt := base()
		choice := map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": oaiFinish}
		finishEvt["choices"] = []any{choice}
		if usageMeta, ok := chunk["usageMetadata"].(map[string]any); ok && len(usageMeta) > 0 {
			finishEvt["usage"] = ConvertUsage(usageMeta)
		}
		events = append(events, sseLine(finishEvt))
	}

	return events
}

// GeminiResponsesToOAIJSON 把 N 个 Gemini 非流式响应聚合成一个含 N 个 choice 的 OAI 响应
// （n>1 多候选用）。
//
// 背景：Gemini 3.x 已禁用 candidateCount>1，故 n>1 由并发 N 次单候选请求实现（见 api 层），
// 这里只负责把 N 个独立响应拼成 N 个 choice（index 从 0 递增），并把各次 usage 累加
// （prompt 会重复计——每次并发都重发了同一 prompt，是 n 扇出的固有代价）。
func GeminiResponsesToOAIJSON(geminiResponses []map[string]any, model string) map[string]any {
	choices := make([]any, 0, len(geminiResponses))
	totalPrompt, totalCompletion, totalTokens := 0, 0, 0
	anyUsage := false

	for idx, resp := range geminiResponses {
		candidate := firstCandidate(resp)
		parts := candidateParts(candidate)
		finish, _ := candidate["finishReason"].(string)
		text, toolCalls, reasoning := ExtractParts(parts, false)

		var oaiFinish string
		if finish != "" {
			oaiFinish = MapFinishReason(finish, len(toolCalls) > 0)
		} else if len(toolCalls) > 0 {
			oaiFinish = "tool_calls"
		} else {
			oaiFinish = "stop"
		}

		message := map[string]any{"role": "assistant"}
		if text != "" {
			message["content"] = text
		} else {
			message["content"] = nil
		}
		if len(toolCalls) > 0 {
			message["tool_calls"] = toolCalls
		}
		if reasoning != "" {
			message["reasoning_content"] = reasoning
		}
		choices = append(choices, map[string]any{
			"index": idx, "message": message, "finish_reason": oaiFinish,
		})

		if usageMeta, ok := resp["usageMetadata"].(map[string]any); ok {
			anyUsage = true
			u := ConvertUsage(usageMeta)
			totalPrompt += numOf(u["prompt_tokens"])
			totalCompletion += numOf(u["completion_tokens"])
			totalTokens += numOf(u["total_tokens"])
		}
	}

	result := map[string]any{
		"id":      "chatcmpl-" + reqID(),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   model,
		"choices": choices,
	}
	if anyUsage {
		if totalTokens == 0 {
			totalTokens = totalPrompt + totalCompletion
		}
		result["usage"] = map[string]any{
			"prompt_tokens":     totalPrompt,
			"completion_tokens": totalCompletion,
			"total_tokens":      totalTokens,
		}
	}
	return result
}

// FinishReasonUnspecified 是匿名端点每帧携带的 protobuf 默认值（无意义），流式必须过滤（红线⑤）。
const FinishReasonUnspecified = "FINISH_REASON_UNSPECIFIED"

// sseLine 把对象序列化成一条 SSE 数据行（data: {json}\n\n，不做 HTML 转义）。
func sseLine(obj map[string]any) string {
	data, err := jsonx.Marshal(obj)
	if err != nil {
		return "data: {}\n\n"
	}
	return "data: " + string(data) + "\n\n"
}

// ---- 响应解析用的小工具 ----

func firstCandidate(resp map[string]any) map[string]any {
	if cands, ok := resp["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			return c
		}
	}
	return map[string]any{}
}

func candidateParts(candidate map[string]any) []any {
	if content, ok := candidate["content"].(map[string]any); ok {
		if parts, ok := content["parts"].([]any); ok {
			return parts
		}
	}
	return nil
}

func isFunctionCallWithName(part map[string]any) bool {
	if fc, ok := part["functionCall"].(map[string]any); ok {
		return truthyStr(fc["name"])
	}
	return false
}

func hasInlineImage(part map[string]any) bool {
	if id, ok := part["inlineData"].(map[string]any); ok {
		mime := toString(firstNonEmpty(id["mimeType"], id["mime_type"]))
		data := toString(id["data"])
		return mime != "" && data != "" && strings.HasPrefix(mime, "image/")
	}
	return false
}

func hasKey(m map[string]any, k string) bool {
	_, ok := m[k]
	return ok
}

func firstNonEmpty(vals ...any) any {
	for _, v := range vals {
		if v != nil && toString(v) != "" {
			return v
		}
	}
	return ""
}

// numOf 把任意 JSON 数字（float64/int）转 int，非数字返回 0。
func numOf(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}
