// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

// part 清洗的单一来源。
//
// 里程碑1 走纯文本路径，clean/merge 之外的多模态/工具分支（functionCall/
// functionResponse/inlineData/fileData/thoughtSignature 等）虽全部实现，但实际
// 触发要等里程碑3 的工具与多模态支持。

const skipThoughtSentinel = "skip_thought_signature_validator"

// NormalizeBase64 规范化 base64：剥离 data URI 前缀、URL-safe 字符还原、补 padding。
func NormalizeBase64(data string) string {
	value := strings.TrimSpace(data)
	if strings.Contains(value, ",") && strings.HasPrefix(value, "data:") {
		if idx := strings.Index(value, ","); idx >= 0 {
			value = value[idx+1:]
		}
	}
	value = strings.NewReplacer("-", "+", "_", "/").Replace(value)
	if pad := len(value) % 4; pad != 0 {
		value += strings.Repeat("=", 4-pad)
	}
	return value
}

// FcNameTracker 按出现顺序追踪 functionCall 名称，用于回填空的 functionResponse.name。
type FcNameTracker struct {
	names []string
	idx   int
}

// NewFcNameTracker 过滤掉空名后构造追踪器。
func NewFcNameTracker(names []string) *FcNameTracker {
	filtered := make([]string, 0, len(names))
	for _, n := range names {
		if strings.TrimSpace(n) != "" {
			filtered = append(filtered, n)
		}
	}
	return &FcNameTracker{names: filtered} //nolint:exhaustruct
}

// NextName 返回下一个未用的名称，用尽返回 ("", false)。
func (t *FcNameTracker) NextName() (string, bool) {
	if t.idx < len(t.names) {
		name := strings.TrimSpace(t.names[t.idx])
		t.idx++
		if name != "" {
			return name, true
		}
	}
	return "", false
}

// truthyStr 判断一个 any 是否为“非空字符串”（非空字符串视为真）。
func truthyStr(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) != ""
}

// CleanPart 清洗单个 part，去除空字段、修复边界情况。无有效内容时返回 (nil, false)。
// （顺序推断版，供内容块合并等无 id 锚点的场景用；
// 请求 contents 走 cleanPartWithID 的 id 锚点版）。
func CleanPart(part map[string]any, functionCallNames []string, fc *FcNameTracker) (map[string]any, bool) {
	hasValid := false
	cleaned := map[string]any{}

	if v, ok := part["text"]; ok {
		if v != nil && toString(v) != "" {
			cleaned["text"] = v
			hasValid = true
		}
	}

	if v, ok := part["thought"]; ok {
		cleaned["thought"] = v
	}

	if fcRaw, ok := part["functionCall"]; ok {
		if fcMap, ok := fcRaw.(map[string]any); ok {
			if truthyStr(fcMap["name"]) {
				cleaned["functionCall"] = fixFunctionCallArgs(fcMap)
				hasValid = true
			}
		}
	}

	if frRaw, ok := part["functionResponse"]; ok {
		if frMap, ok := frRaw.(map[string]any); ok {
			currentName, _ := frMap["name"].(string)
			if strings.TrimSpace(currentName) == "" {
				inferred := ""
				if fc != nil {
					if n, ok := fc.NextName(); ok {
						inferred = n
					}
				} else if len(functionCallNames) > 0 {
					inferred = functionCallNames[len(functionCallNames)-1]
				}
				if inferred != "" {
					fixed := copyMap(frMap)
					fixed["name"] = inferred
					normalizeFunctionResponseBody(fixed)
					cleaned["functionResponse"] = fixed
					hasValid = true
				}
			} else {
				fixed := copyMap(frMap)
				normalizeFunctionResponseBody(fixed)
				cleaned["functionResponse"] = fixed
				hasValid = true
			}
		}
	}

	if idRaw, ok := part["inlineData"]; ok {
		if id, ok := idRaw.(map[string]any); ok {
			if truthyStr(id["data"]) && truthyStr(id["mimeType"]) {
				cleaned["inlineData"] = idRaw
				hasValid = true
			}
		}
	}

	if fdRaw, ok := part["fileData"]; ok {
		if fd, ok := fdRaw.(map[string]any); ok {
			if truthyStr(fd["fileUri"]) && truthyStr(fd["mimeType"]) {
				cleaned["fileData"] = fdRaw
				hasValid = true
			}
		}
	}

	for _, key := range []string{"executableCode", "codeExecutionResult"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
			hasValid = true
		}
	}

	if v, ok := part["thoughtSignature"]; ok {
		cleaned["thoughtSignature"] = v
	}

	for _, key := range []string{"videoMetadata", "mediaResolution"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
		}
	}

	finalizeCleanedPart(cleaned)

	if hasValid {
		return cleaned, true
	}
	return nil, false
}

// normalizeFunctionResponseBody 把 functionResponse.response 的非对象值包成 {"result": ...}。
func normalizeFunctionResponseBody(fr map[string]any) {
	if resp, ok := fr["response"]; ok {
		if _, isMap := resp.(map[string]any); !isMap {
			fr["response"] = map[string]any{"result": resp}
		}
	}
}

// cleanPartWithID 是 CleanPart 的 id 锚点版本：
// functionResponse 缺 name 时，先按其 id 在 callIDMap 反查，再按 responseIndex 在
// functionCallNames 里按位置兜底。其余字段清洗与 CleanPart 完全一致。
//
//   - functionCallNames：当前 model 消息里 functionCall 的 name 列表（按出现顺序）。
//   - responseIndex：当前 functionResponse 在其 function 消息内的位置（非 functionResponse 传 -1）。
//   - callIDMap：累积的 id→name 映射（穿线自 functionCall.id）。
func cleanPartWithID(part map[string]any, functionCallNames []string, responseIndex int, callIDMap map[string]string) (map[string]any, bool) {
	hasValid := false
	cleaned := map[string]any{}

	if v, ok := part["text"]; ok {
		if v != nil && toString(v) != "" {
			cleaned["text"] = v
			hasValid = true
		}
	}

	if v, ok := part["thought"]; ok {
		cleaned["thought"] = v
	}

	if fcRaw, ok := part["functionCall"]; ok {
		if fcMap, ok := fcRaw.(map[string]any); ok {
			if truthyStr(fcMap["name"]) {
				fixed := fixFunctionCallArgs(fcMap)
				// id 是我们的内部锚点（穿线给 callIDMap 反查 name），不属 Gemini schema，剥离。
				delete(fixed, "id")
				cleaned["functionCall"] = fixed
				hasValid = true
			}
		}
	}

	if frRaw, ok := part["functionResponse"]; ok {
		if frMap, ok := frRaw.(map[string]any); ok {
			name := strings.TrimSpace(toString(frMap["name"]))
			if name == "" {
				// ① 按 id 反查（并行工具调用精确配对）。
				if fid, _ := frMap["id"].(string); fid != "" && callIDMap != nil {
					name = callIDMap[fid]
				}
				// ② 按位置兜底。
				if name == "" && responseIndex >= 0 && responseIndex < len(functionCallNames) {
					name = functionCallNames[responseIndex]
				}
				// ③ 最后兜底 "unknown"：保住 functionResponse 不被丢弃（缺 name 会触发上游
				//    400 / 单边 functionResponse 报错）。优于静默丢响应。
				if name == "" {
					name = "unknown"
				}
			}
			fixed := copyMap(frMap)
			fixed["name"] = name
			delete(fixed, "id") // 内部锚点，不下发上游。
			normalizeFunctionResponseBody(fixed)
			cleaned["functionResponse"] = fixed
			hasValid = true
		}
	}

	if idRaw, ok := part["inlineData"]; ok {
		if id, ok := idRaw.(map[string]any); ok {
			if truthyStr(id["data"]) && truthyStr(id["mimeType"]) {
				cleaned["inlineData"] = idRaw
				hasValid = true
			}
		}
	}

	if fdRaw, ok := part["fileData"]; ok {
		if fd, ok := fdRaw.(map[string]any); ok {
			if truthyStr(fd["fileUri"]) && truthyStr(fd["mimeType"]) {
				cleaned["fileData"] = fdRaw
				hasValid = true
			}
		}
	}

	for _, key := range []string{"executableCode", "codeExecutionResult"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
			hasValid = true
		}
	}

	if v, ok := part["thoughtSignature"]; ok {
		cleaned["thoughtSignature"] = v
	}

	for _, key := range []string{"videoMetadata", "mediaResolution"} {
		if v, ok := part[key]; ok && isTruthy(v) {
			cleaned[key] = v
		}
	}

	finalizeCleanedPart(cleaned)

	if hasValid {
		return cleaned, true
	}
	return nil, false
}

// fixFunctionCallArgs 拷贝 functionCall 并把字符串 args 解析为对象（无效则包成 {"raw":...}）。
func fixFunctionCallArgs(fc map[string]any) map[string]any {
	fixed := copyMap(fc)
	if argStr, ok := fixed["args"].(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(argStr), &parsed); err == nil {
			fixed["args"] = parsed
		} else {
			fixed["args"] = map[string]any{"raw": argStr}
		}
	}
	return fixed
}

// finalizeCleanedPart 对清洗后的 part 做收尾归一（对应 clean_part 末尾的几步）：
// thought 非 str/bool 归空、functionResponse 不带 thought*、含 functionCall/thought*/sig 注入
// sentinel、纯文本不残留 thought*。
func finalizeCleanedPart(cleaned map[string]any) {
	// thought 必须是 string 或 bool（Vertex 要求），其它类型归一为空串。
	if tv, ok := cleaned["thought"]; ok {
		if _, isStr := tv.(string); !isStr {
			if _, isBool := tv.(bool); !isBool {
				cleaned["thought"] = ""
			}
		}
	}

	// functionResponse part 不得携带 thought/thoughtSignature；
	// 含 functionCall/thought/thoughtSignature 的 part 注入 sentinel，待 EncodeThoughtSignature 编码。
	if _, ok := cleaned["functionResponse"]; ok {
		delete(cleaned, "thought")
		delete(cleaned, "thoughtSignature")
	} else {
		_, hasFC := cleaned["functionCall"]
		_, hasThought := cleaned["thought"]
		_, hasSig := cleaned["thoughtSignature"]
		if hasFC || hasThought || hasSig {
			cleaned["thoughtSignature"] = skipThoughtSentinel
		}
	}

	// 纯文本（有 text 且非 thought）不应残留 thought/thoughtSignature。
	if truthyStr(cleaned["text"]) && !isTruthy(cleaned["thought"]) {
		delete(cleaned, "thought")
		delete(cleaned, "thoughtSignature")
	}
}

// EncodeThoughtSignature 递归把 thoughtSignature 的 sentinel 值 base64 编码。
// 含 MAX_DEPTH=64 深度上限。
func EncodeThoughtSignature(contents any, depth int) any {
	const maxDepth = 64
	if depth > maxDepth {
		return contents
	}
	switch v := contents.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = EncodeThoughtSignature(item, depth+1)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			if k == "parts" {
				if parts, ok := val.([]any); ok {
					newParts := make([]any, len(parts))
					for i, p := range parts {
						if pm, ok := p.(map[string]any); ok {
							np := copyMap(pm)
							if sig, ok := np["thoughtSignature"].(string); ok && sig == skipThoughtSentinel {
								np["thoughtSignature"] = base64.StdEncoding.EncodeToString([]byte(sig))
							}
							newParts[i] = np
						} else {
							newParts[i] = p
						}
					}
					out[k] = newParts
					continue
				}
			}
			switch val.(type) {
			case map[string]any, []any:
				out[k] = EncodeThoughtSignature(val, depth+1)
			default:
				out[k] = val
			}
		}
		return out
	default:
		return contents
	}
}

// HandleBase64InContents 递归规范化 contents 中 inlineData 的 base64 数据。
func HandleBase64InContents(contents any) any {
	switch v := contents.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = HandleBase64InContents(item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, val := range v {
			if k == "inlineData" {
				if id, ok := val.(map[string]any); ok {
					if data, ok := id["data"].(string); ok {
						ni := copyMap(id)
						ni["data"] = NormalizeBase64(data)
						out[k] = ni
						continue
					}
				}
			}
			out[k] = HandleBase64InContents(val)
		}
		return out
	default:
		return contents
	}
}

// MergeContentBlocks 合并相邻同类型文本块（thought+thought、text+text），保留交错顺序。
// 响应解析（parser）用到。
func MergeContentBlocks(parts []map[string]any) []map[string]any {
	cleaned := make([]map[string]any, 0, len(parts))
	for _, p := range parts {
		if c := cleanSimple(p); c != nil {
			cleaned = append(cleaned, c)
		}
	}
	if len(cleaned) == 0 {
		return []map[string]any{}
	}

	merged := make([]map[string]any, 0, len(cleaned))
	var current map[string]any

	for _, part := range cleaned {
		isText := truthyStr(part["text"])
		if !isText {
			merged = append(merged, part)
			current = nil
			continue
		}
		isThought := isTruthy(part["thought"])
		if current != nil && isTruthy(current["thought"]) == isThought {
			current["text"] = toString(current["text"]) + toString(part["text"])
			if sig, ok := part["thoughtSignature"]; ok {
				if _, exists := current["thoughtSignature"]; !exists {
					current["thoughtSignature"] = sig
				}
			}
		} else {
			np := map[string]any{"text": toString(part["text"])}
			if isThought {
				np["thought"] = true
				if sig, ok := part["thoughtSignature"]; ok {
					np["thoughtSignature"] = sig
				}
			}
			merged = append(merged, np)
			current = np
		}
	}
	return merged
}

// cleanSimple 是用于内容块合并的轻量清洗：text 为空丢弃；functionCall 无名删除。
// 简化：不走结构校验，直接基础清洗。
func cleanSimple(part map[string]any) map[string]any {
	cleaned := copyMap(part)
	if t, ok := cleaned["text"]; ok {
		if toString(t) == "" {
			// 空 text 的 part 视为无效，丢弃。
			delete(cleaned, "text")
		}
	}
	if fcRaw, ok := cleaned["functionCall"]; ok {
		if fc, ok := fcRaw.(map[string]any); ok {
			if !truthyStr(fc["name"]) {
				delete(cleaned, "functionCall")
			}
		}
	}
	if len(cleaned) == 0 {
		return nil
	}
	return cleaned
}
