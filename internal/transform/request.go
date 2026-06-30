// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"strings"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// safetyCategories 是默认安全设置覆盖的 5 个类别（缺省全 BLOCK_NONE）。
var safetyCategories = []string{ //nolint:gochecknoglobals
	"HARM_CATEGORY_HARASSMENT",
	"HARM_CATEGORY_HATE_SPEECH",
	"HARM_CATEGORY_SEXUALLY_EXPLICIT",
	"HARM_CATEGORY_DANGEROUS_CONTENT",
	"HARM_CATEGORY_CIVIC_INTEGRITY",
}

// supportedVarFields 是从 geminiPayload 透传进 variables 的字段（统一 camelCase）。
var supportedVarFields = []string{ //nolint:gochecknoglobals
	"contents", "tools", "toolConfig", "systemInstruction", "safetySettings", "generationConfig",
}

// ConvertChatRequest 将 OpenAI ChatCompletion 请求体转为 (model, geminiPayload)。
// 全功能版。
//
// 覆盖：
//   - 角色：system/developer→systemInstruction；user 多模态内容；assistant 文本 +
//     tool_calls→functionCall（穿 id）；tool/function→functionResponse（穿 id）。
//   - 工具：tools/functions→functionDeclarations（parameters 白名单清洗）；
//     tool_choice/function_call→toolConfig（none/auto/required/指定函数）。
//   - generationConfig：温度/top_p/top_k/penalty/seed/logprobs/max_tokens/stop/
//     response_format/reasoning_effort→thinkingLevel/thinking/mediaResolution。
//   - 顶层：safety_settings 透传；parallel_tool_calls 优雅接受。
//
// 出错（messages 空、max_tokens<1、tool_choice 校验）返回普通 error，由 api 层映射为 400。
func ConvertChatRequest(body map[string]any, cfg config.AppConfig) (string, map[string]any, error) {
	model, _ := body["model"].(string)

	if cfg.DebugMode {
		geminiPayloadStr, _ := json.Marshal(body)
		log.Printf("[DEBUG] Payload 打印: ConvertChatRequest 转换前 payload: %s", string(geminiPayloadStr))
	}

	messagesRaw, ok := body["messages"].([]any)
	if !ok || len(messagesRaw) == 0 {
		return "", nil, fmt.Errorf("messages 不能为空 (messages must be a non-empty array)")
	}

	contents := []any{}
	var systemParts []any
	// tool_call_id → 函数名映射：Gemini 要求 functionResponse.name 与 functionCall.name 一致，
	// 而 OpenAI 的 tool 结果消息只带 tool_call_id，需回查对应 assistant tool_call 的函数名。
	toolIDToName := map[string]string{}

	for _, msgRaw := range messagesRaw {
		msg, ok := msgRaw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		content := msg["content"]

		switch role {
		case "system", "developer":
			switch c := content.(type) {
			case string:
				systemParts = append(systemParts, map[string]any{"text": c})
			case []any:
				for _, item := range c {
					if im, ok := item.(map[string]any); ok {
						if t, _ := im["type"].(string); t == "text" || t == "input_text" {
							systemParts = append(systemParts, map[string]any{"text": im["text"]})
						}
					} else if s, ok := item.(string); ok {
						systemParts = append(systemParts, map[string]any{"text": s})
					}
				}
			}
		case "user":
			parts := convertUserContent(content)
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "user", "parts": parts})
			}
		case "assistant":
			var parts []any
			if isTruthy(content) {
				// assistant 文本里可能嵌着模型上一轮生成的 markdown data-URI 图片
				// （多轮改图场景）。必须重新解析回 inlineData，否则整段 base64 markdown
				// 作为巨型文本塞进 model 角色被上游拒。
				parts = append(parts, splitAssistantContent(content)...)
			}
			// assistant.tool_calls → functionCall（穿 id 锚点，供 functionResponse 回查 name）。
			if toolCalls, ok := msg["tool_calls"].([]any); ok {
				for _, tc := range toolCalls {
					parsed := extractOAIToolCall(tc)
					if parsed == nil {
						continue // 空 tool_call 守卫：无非空 name 则跳过
					}
					if parsed.id != "" {
						toolIDToName[parsed.id] = parsed.name
					}
					fc := map[string]any{"name": parsed.name, "args": parsed.args}
					// id 锚点（学 v1.0.4）：把 tool_call.id 穿进 functionCall，
					// 供并行工具调用时 functionResponse 按 id 精确反查 name。
					if parsed.id != "" {
						fc["id"] = parsed.id
					}
					parts = append(parts, map[string]any{"functionCall": fc})
				}
			}
			if len(parts) > 0 {
				contents = append(contents, map[string]any{"role": "model", "parts": parts})
			}
		case "tool":
			// functionResponse.name 必须匹配对应 functionCall.name。
			// 优先：显式 name > 同批 assistant tool_call 的 id→name 映射。
			// 都没有时**留空**，交给下游 cleanPartWithID 用 id 锚点/位置兜底推断（更鲁棒，
			// 尤其并行工具调用乱序回传）。
			tcID, _ := msg["tool_call_id"].(string)
			name := firstTruthyString(msg["name"], toolIDToName[tcID])
			fr := map[string]any{"response": coerceFunctionResponse(msg["content"])}
			if name != "" {
				fr["name"] = name
			}
			// id 锚点：把 tool_call_id 穿进 functionResponse，配合 clean_part 的 call_id_map 反查。
			if tcID != "" {
				fr["id"] = tcID
			}
			// 并行工具调用：OpenAI 把每个 tool 结果拆成独立 message，但 Gemini 要求一个
			// 并行调用 turn 的所有 functionResponse 在**同一个 function content** 里（否则上游报
			// "number of function response parts ... equal to ... function call parts"）。
			// 故把连续的 tool/function 消息合并进尾部的 function content。
			contents = appendFunctionResponse(contents, map[string]any{"functionResponse": fr})
		case "function":
			// 已废弃的 OpenAI legacy function 角色：name 显式给出，content 即返回值。
			name := firstTruthyString(msg["name"])
			if name == "" {
				name = "unknown"
			}
			contents = appendFunctionResponse(contents, map[string]any{"functionResponse": map[string]any{
				"name": name, "response": coerceFunctionResponse(msg["content"]),
			}})
		}
	}

	geminiPayload := map[string]any{"contents": contents}
	if len(systemParts) > 0 {
		geminiPayload["systemInstruction"] = map[string]any{"parts": systemParts}
	}

	// tools / functions（legacy）→ functionDeclarations
	declaredToolNames, err := convertTools(body, geminiPayload)
	if err != nil {
		return "", nil, err
	}

	// tool_choice / function_call（legacy）→ toolConfig
	if err := convertToolChoice(body, geminiPayload, declaredToolNames); err != nil {
		return "", nil, err
	}

	// generationConfig（键直接用 camelCase）
	genCfg := map[string]any{}
	for _, m := range []struct{ oai, gem string }{
		{"temperature", "temperature"},
		{"top_p", "topP"},
		{"top_k", "topK"},
		{"presence_penalty", "presencePenalty"},
		{"frequency_penalty", "frequencyPenalty"},
		{"seed", "seed"},
	} {
		if v, ok := body[m.oai]; ok && v != nil {
			genCfg[m.gem] = v
		}
	}

	// logprobs 透传：OpenAI bool → Gemini responseLogprobs；top_logprobs → logprobs 计数。
	if v, ok := body["logprobs"]; ok && v != nil {
		genCfg["responseLogprobs"] = isTruthy(v)
	}
	if v, ok := body["top_logprobs"]; ok && v != nil {
		genCfg["logprobs"] = v
	}

	// max_tokens / max_completion_tokens（OpenAI 规范要求 >=1）
	var maxTokens any
	if v, ok := body["max_tokens"]; ok && v != nil {
		maxTokens = v
	} else if v, ok := body["max_completion_tokens"]; ok && v != nil {
		maxTokens = v
	}
	if maxTokens != nil {
		// JSON 数字解码为 float64；bool 不是 float64，会落到错误分支。
		f, ok := maxTokens.(float64)
		if !ok || f < 1 {
			return "", nil, fmt.Errorf("max_tokens must be an integer >= 1")
		}
		if !cfg.DropMaxTokens {
			genCfg["maxOutputTokens"] = maxTokens
		}
	}

	// stop → stopSequences
	if stop, ok := body["stop"]; ok && stop != nil {
		switch s := stop.(type) {
		case string:
			genCfg["stopSequences"] = []any{s}
		case []any:
			genCfg["stopSequences"] = s
		}
	}

	// response_format → responseMimeType 与 responseSchema（已完美支持结构化输出翻译）
	if rf, ok := body["response_format"].(map[string]any); ok {
		if t, _ := rf["type"].(string); t == "json_object" || t == "json_schema" {
			genCfg["responseMimeType"] = "application/json"
			if t == "json_schema" {
				if js, ok := rf["json_schema"].(map[string]any); ok {
					if sch, ok := js["schema"].(map[string]any); ok {
						// 暂存标准 JSON Schema，后续在 BuildVertexVariables 转换时再通过 toNativeSchema 归一
						genCfg["responseSchema"] = sch
					}
				}
			}
		}
	}

	// safety_settings 透传（顶层字段，两种命名都接受，必须是数组）
	if sl, ok := body["safety_settings"].([]any); ok {
		geminiPayload["safetySettings"] = sl
	} else if sl, ok := body["safetySettings"].([]any); ok {
		geminiPayload["safetySettings"] = sl
	}

	if len(genCfg) > 0 {
		geminiPayload["generationConfig"] = genCfg
	}

	// media_resolution 透传：控制图/视频处理粒度（generationConfig.mediaResolution）。
	// 接受顶层 media_resolution / mediaResolution，或嵌在 extra_body 里（OpenAI SDK 常这么塞）。
	mrRaw := firstPresentRaw(body, "media_resolution", "mediaResolution")
	if mrRaw == nil {
		if extra, ok := body["extra_body"].(map[string]any); ok {
			mrRaw = firstPresentRaw(extra, "media_resolution", "mediaResolution")
		}
	}
	if mrRaw != nil {
		if mr := normalizeMediaResolution(mrRaw); mr != "" {
			ensureGenCfg(geminiPayload)["mediaResolution"] = mr
		}
	}

	// reasoning_effort → thinkingConfig.thinkingLevel（OpenAI 标准参数，优先支持）。
	// 放在 thinking 块之前：若客户端同时给了 thinking，则 thinking 作为更具体的覆盖项
	// 后写生效（last-writer-wins）。
	if re, ok := body["reasoning_effort"].(string); ok {
		if level, ok := reasoningEffortToThinkingLevel[strings.ToLower(strings.TrimSpace(re))]; ok {
			gc := ensureGenCfg(geminiPayload)
			tc, ok := gc["thinkingConfig"].(map[string]any)
			if !ok {
				tc = map[string]any{}
				gc["thinkingConfig"] = tc
			}
			tc["thinkingLevel"] = level
		}
	}

	// thinking 配置: {"thinking": {"type": "enabled", "budget_tokens": 10000}}
	if thinking, ok := body["thinking"].(map[string]any); ok {
		if tt, _ := thinking["type"].(string); tt == "enabled" || tt == "disabled" {
			tc := map[string]any{"thinkingLevel": "MEDIUM"}
			if tt == "disabled" {
				tc["thinkingLevel"] = "NONE"
			}
			if budget, ok := thinking["budget_tokens"]; ok && budget != nil {
				tc["thinkingBudget"] = budget
			}
			ensureGenCfg(geminiPayload)["thinkingConfig"] = tc
		}
	}

	return model, geminiPayload, nil
}

// appendFunctionResponse 把一个 functionResponse part 追加进 contents：若尾部已是 function
// content 则并入其 parts（合并并行工具调用的多个响应到一个 turn），否则新建一个 function content。
func appendFunctionResponse(contents []any, part map[string]any) []any {
	if n := len(contents); n > 0 {
		if last, ok := contents[n-1].(map[string]any); ok && last["role"] == "function" {
			parts, _ := last["parts"].([]any)
			last["parts"] = append(parts, part)
			return contents
		}
	}
	return append(contents, map[string]any{"role": "function", "parts": []any{part}})
}

// coerceFunctionResponse 把 tool/function 角色的 content 规范成 Gemini functionResponse.response
// 要求的 JSON object：字符串先尝试 JSON 解析；非对象（数字/布尔/数组/字符串）包成 {"result":...}。
// 处理 tool/function 分支的返回值规范化。
func coerceFunctionResponse(raw any) map[string]any {
	obj := raw
	if s, ok := raw.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			obj = parsed
		} else {
			obj = map[string]any{"result": s}
		}
	}
	if m, ok := obj.(map[string]any); ok {
		return m
	}
	return map[string]any{"result": obj}
}

// convertTools 把 OpenAI tools（或 legacy functions）转为 functionDeclarations，写入
// geminiPayload["tools"]。返回已声明的工具名集合（供 tool_choice 校验）。
func convertTools(body, geminiPayload map[string]any) (map[string]bool, error) {
	oaiTools, _ := body["tools"].([]any)
	// 兼容已废弃的顶层 functions 字段（无 tools 时回退）。
	if len(oaiTools) == 0 {
		if fns, ok := body["functions"].([]any); ok {
			for _, f := range fns {
				oaiTools = append(oaiTools, map[string]any{"type": "function", "function": f})
			}
		}
	}
	declared := map[string]bool{}
	if len(oaiTools) == 0 {
		return declared, nil
	}

	var funcDecls []any
	for _, t := range oaiTools {
		f := extractOAIFunctionTool(t)
		if f == nil {
			continue
		}
		decl := map[string]any{"name": f["name"]}
		declared[toString(f["name"])] = true
		if isTruthy(f["description"]) {
			decl["description"] = f["description"]
		}
		if params, ok := f["parameters"].(map[string]any); ok && len(params) > 0 {
			// 对 parameters 递归白名单清洗，剔除 Gemini 不支持的 schema 字段。
			decl["parameters"] = cleanFunctionParameters(deepCopyAny(params))
		} else {
			// 缺省 parameters 时补默认空对象 schema，满足 Gemini functionDeclarations 要求。
			decl["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		funcDecls = append(funcDecls, decl)
	}
	if len(funcDecls) > 0 {
		geminiPayload["tools"] = []any{map[string]any{"functionDeclarations": funcDecls}}
	}
	return declared, nil
}

// convertToolChoice 把 tool_choice（或 legacy function_call）转为 toolConfig。
func convertToolChoice(body, geminiPayload map[string]any, declared map[string]bool) error {
	tc := firstPresentRaw(body, "tool_choice", "function_call")
	if tc == nil || !isTruthy(tc) {
		return nil
	}
	switch v := tc.(type) {
	case string:
		switch v {
		case "none":
			geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "NONE"}}
		case "auto":
			geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "AUTO"}}
		case "required":
			// required 必须有可调用工具，否则上游 ANY 模式无函数可选 → 行为未定义。
			if len(declared) == 0 {
				return fmt.Errorf("tool_choice='required' requires at least one tool")
			}
			geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{"mode": "ANY"}}
		}
	case map[string]any:
		var fnName string
		if v["type"] == "function" {
			if fn, ok := v["function"].(map[string]any); ok {
				fnName, _ = fn["name"].(string)
			}
		} else if n, ok := v["name"].(string); ok {
			fnName = n
		}
		if fnName != "" {
			// 指定函数名必须在已声明工具集中，否则 400（避免引用未知函数）。
			if len(declared) > 0 && !declared[fnName] {
				return fmt.Errorf("tool_choice references unknown function: %s", fnName)
			}
			geminiPayload["toolConfig"] = map[string]any{"functionCallingConfig": map[string]any{
				"mode": "ANY", "allowedFunctionNames": []any{fnName},
			}}
		}
	}
	return nil
}

// ensureGenCfg 返回 geminiPayload["generationConfig"]（不存在则创建）。
func ensureGenCfg(geminiPayload map[string]any) map[string]any {
	gc, ok := geminiPayload["generationConfig"].(map[string]any)
	if !ok {
		gc = map[string]any{}
		geminiPayload["generationConfig"] = gc
	}
	return gc
}

// firstPresentRaw 在 map 里依次查 keys，返回第一个存在的原始值（不存在返回 nil）。
func firstPresentRaw(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return nil
}

// deepCopyAny 深拷贝 map/slice 结构（用于 function parameters 清洗前复制，避免改动原 body）。
func deepCopyAny(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[k] = deepCopyAny(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = deepCopyAny(item)
		}
		return out
	default:
		return v
	}
}

// convertUserContent 把 OpenAI user message content 转为 Gemini parts。
// 全功能版。
//
//   - 字符串 → [{text}]
//   - text → {text}
//   - image_url：data: URI → inlineData（按 mime 透传，含 video/*）；远程 URL → fileData
//   - video_url / input_video：data: URI → inlineData（video/*，默认 video/mp4）
//   - input_audio：{data,format} 或 data: URI → inlineData（audio/*，默认 audio/wav）
//
// base64 一律经 NormalizeBase64 规范化（URL-safe→标准、补 padding），与下游
// HandleBase64InContents 同一来源、幂等。
func convertUserContent(content any) []any {
	if content == nil {
		return nil
	}
	if s, ok := content.(string); ok {
		return []any{map[string]any{"text": s}}
	}
	list, ok := content.([]any)
	if !ok {
		return nil
	}

	var parts []any
	for _, itemRaw := range list {
		if s, ok := itemRaw.(string); ok {
			parts = append(parts, map[string]any{"text": s})
			continue
		}
		item, ok := itemRaw.(map[string]any)
		if !ok {
			continue
		}
		t, _ := item["type"].(string)
		switch t {
		case "text":
			parts = append(parts, map[string]any{"text": item["text"]})

		case "image_url":
			url := imageURLString(item["image_url"])
			if strings.HasPrefix(url, "data:") {
				// image_url 的 data: URI 可携带任意 mime（含 video/*），按解析出的 mimeType 透传。
				if mime, b64 := parseDataURI(url); mime != "" && b64 != "" {
					parts = append(parts, inlineDataPart(mime, b64))
				}
			} else if hasRemotePrefix(url) {
				// 远程图片 URL → fileData（按扩展名猜 mime）。
				parts = append(parts, map[string]any{"fileData": map[string]any{
					"mimeType": guessMIMEFromURL(url), "fileUri": url,
				}})
			}

		case "video_url", "input_video":
			// 显式视频内容块：两种字段名都兼容，holder 可为字符串或 {url}。
			url := holderURLString(item[t])
			if strings.HasPrefix(url, "data:") {
				if mime, b64 := parseDataURI(url); b64 != "" {
					// 仅接受 base64 内联视频（运营决定：不走 Files API/服务端缓存）。
					if mime == "" || !strings.HasPrefix(mime, "video/") {
						mime = "video/mp4"
					}
					parts = append(parts, inlineDataPart(mime, b64))
				}
			}

		case "input_audio":
			// OpenAI gpt-4o-audio 风格音频输入块：{data, format} 或 data: URI。
			mime, b64 := parseInputAudio(item["input_audio"])
			if b64 != "" {
				if mime == "" || !strings.HasPrefix(mime, "audio/") {
					mime = "audio/wav"
				}
				parts = append(parts, inlineDataPart(mime, b64))
			}
		}
	}
	return parts
}

// assistantImageMarkdownRe 匹配 assistant 文本里嵌的 markdown data-URI 图片。
var assistantImageMarkdownRe = regexp.MustCompile(`!\[[^\]]*\]\((data:[^()\s;,]+;base64,[A-Za-z0-9+/=_\-]+)\)`)

// splitAssistantContent 把 assistant 文本切成 text / inlineData 混合 parts。
func splitAssistantContent(content any) []any {
	s, ok := content.(string)
	if !ok {
		return []any{map[string]any{"text": content}}
	}
	locs := assistantImageMarkdownRe.FindAllStringSubmatchIndex(s, -1)
	if len(locs) == 0 {
		return []any{map[string]any{"text": s}}
	}
	var parts []any
	last := 0
	for _, m := range locs {
		if pre := strings.TrimSpace(s[last:m[0]]); pre != "" {
			parts = append(parts, map[string]any{"text": pre})
		}
		if mime, b64 := parseDataURI(s[m[2]:m[3]]); mime != "" && b64 != "" {
			parts = append(parts, inlineDataPart(mime, b64))
		}
		last = m[1]
	}
	if post := strings.TrimSpace(s[last:]); post != "" {
		parts = append(parts, map[string]any{"text": post})
	}
	if len(parts) == 0 {
		parts = append(parts, map[string]any{"text": ""})
	}
	return parts
}

// inlineDataPart 构造 inlineData part，data 经 NormalizeBase64 规范化。
func inlineDataPart(mime, b64 string) map[string]any {
	return map[string]any{"inlineData": map[string]any{
		"mimeType": mime, "data": NormalizeBase64(b64),
	}}
}

// imageURLString 从 image_url 字段取出 url 字符串（兼容 {url} 与字符串两种形态）。
func imageURLString(v any) string {
	if m, ok := v.(map[string]any); ok {
		s, _ := m["url"].(string)
		return s
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// holderURLString 从 video_url/input_video 的 holder 取出 url（兼容 {url} 与字符串）。
func holderURLString(holder any) string {
	switch h := holder.(type) {
	case string:
		return h
	case map[string]any:
		s, _ := h["url"].(string)
		return s
	default:
		return ""
	}
}

// parseInputAudio 从 input_audio holder 解析 (mime, base64)。
// 兼容：① {data, format}（format 经 audioFormatMIME 映射）；② {data: "data:..."} ;
// ③ {url: "data:..."}；④ holder 直接 is data: URI 字符串。处理 input_audio 分支。
func parseInputAudio(holder any) (string, string) {
	switch h := holder.(type) {
	case string:
		if strings.HasPrefix(h, "data:") {
			return parseDataURI(h)
		}
	case map[string]any:
		if rawData, ok := h["data"].(string); ok && rawData != "" {
			if strings.HasPrefix(rawData, "data:") {
				return parseDataURI(rawData)
			}
			fmtStr, _ := h["format"].(string)
			return audioFormatMIME[strings.ToLower(fmtStr)], rawData
		}
		// 兼容 {"input_audio":{"url":"data:audio/...;base64,..."}}
		if url, ok := h["url"].(string); ok && strings.HasPrefix(url, "data:") {
			return parseDataURI(url)
		}
	}
	return "", ""
}

// hasRemotePrefix 判断 URL 是否为远程引用（http/https/gs）。
func hasRemotePrefix(url string) bool {
	return strings.HasPrefix(url, "http://") ||
		strings.HasPrefix(url, "https://") ||
		strings.HasPrefix(url, "gs://")
}

// BuildVertexVariables 由 geminiPayload 构建发往上游的 variables（不含 querySignature/
// operationName/region/recaptchaToken 包壳，那些由 vertex 层注入）。
// 负责 new_variables 的构建部分。
func BuildVertexVariables(model string, geminiPayload map[string]any, cfg config.AppConfig) map[string]any {
	vars := map[string]any{}
	vars["model"] = parseModelName(model)

	for _, field := range supportedVarFields {
		if v, ok := geminiPayload[field]; ok {
			vars[field] = v
		} else {
			// 尝试 snake_case 版本
			if v, ok := geminiPayload[CamelToSnake(field)]; ok {
				vars[field] = v
			}
		}
	}

	// systemInstruction：无 user content 时降级为首条 user message
	handleSystemInstruction(vars)

	// contents 归一链（两次 normalize 夹 inlineData 归一，
	// 再 base64 规范化 / 连续同角色合并 / 过滤空 parts / thoughtSignature 编码）
	if c, ok := vars["contents"]; ok {
		c = normalizeContents(c)
		c = handleInlineDataCase(c)
		c = normalizeContents(c)
		c = HandleBase64InContents(c)
		c = mergeContiguousRoles(c) // 相邻同 role 的 content 合并为一个（多轮工具结果防 400）
		c = filterEmptyContents(c)
		c = EncodeThoughtSignature(c, 0)
		vars["contents"] = c
	}

	// tools 归一：转 camelCase + 把裸 FunctionDeclaration 聚合进一个 functionDeclarations Tool，
	// 空结果时移除 tools 与 toolConfig（避免上游因 tools=[] 报错）。
	if rawTools, ok := vars["tools"]; ok {
		normalized := normalizeToolsFormat(rawTools)
		if len(normalized) > 0 {
			vars["tools"] = normalized
		} else {
			delete(vars, "tools")
			delete(vars, "toolConfig")
		}
	}
	if tc, ok := vars["toolConfig"]; ok {
		vars["toolConfig"] = convertToolsFormat(tc)
	}

	// generationConfig：camelCase 化 + topK 上限
	if genCfg := buildGenerationConfig(geminiPayload); len(genCfg) > 0 {
		vars["generationConfig"] = genCfg
	}

	// safetySettings 缺省注入（全 BLOCK_NONE 或 config 自定义 threshold）
	if _, ok := vars["safetySettings"]; !ok {
		if _, ok2 := geminiPayload["safety_settings"]; !ok2 {
			vars["safetySettings"] = buildSafetySettings(cfg)
		}
	}

	return vars
}

// handleSystemInstruction 把 systemInstruction 在无 user content 时降级为首条 user message。
func handleSystemInstruction(vars map[string]any) {
	siRaw, ok := vars["systemInstruction"]
	if !ok || !isTruthy(siRaw) {
		return
	}
	contents, _ := vars["contents"].([]any)
	for _, c := range contents {
		if cm, ok := c.(map[string]any); ok {
			if r, _ := cm["role"].(string); r == "user" {
				return // 已有 user role，无需降级
			}
		}
	}
	text := extractTextFromInstruction(siRaw)
	if text == "" {
		return
	}
	userMsg := map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}
	vars["contents"] = append([]any{userMsg}, contents...)
	delete(vars, "systemInstruction")
}

func extractTextFromInstruction(instruction any) string {
	if s, ok := instruction.(string); ok {
		return s
	}
	if m, ok := instruction.(map[string]any); ok {
		if parts, ok := m["parts"].([]any); ok {
			var sb strings.Builder
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if t, ok := pm["text"]; ok {
						sb.WriteString(toString(t))
					}
				}
			}
			return sb.String()
		}
	}
	return ""
}

// normalizeContents 把 contents 归一为 Gemini content 列表。
func normalizeContents(contents any) any {
	switch c := contents.(type) {
	case nil:
		return []any{}
	case string:
		return []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": c}}}}
	case map[string]any:
		return []any{normalizeContent(c)}
	case []any:
		normalized := []any{}
		var pendingText []any
		for _, item := range c {
			if s, ok := item.(string); ok {
				pendingText = append(pendingText, map[string]any{"text": s})
			} else if m, ok := item.(map[string]any); ok {
				if len(pendingText) > 0 {
					normalized = append(normalized, map[string]any{"role": "user", "parts": pendingText})
					pendingText = nil
				}
				normalized = append(normalized, normalizeContent(m))
			}
		}
		if len(pendingText) > 0 {
			normalized = append(normalized, map[string]any{"role": "user", "parts": pendingText})
		}
		return normalized
	default:
		return contents
	}
}

// normalizeContent 归一单个 content（role 映射 + content→parts + str→text）。
func normalizeContent(content map[string]any) map[string]any {
	n := copyMap(content)
	_, hasContent := n["content"]
	_, hasParts := n["parts"]
	switch {
	case hasContent && !hasParts:
		n["parts"] = normalizeParts(n["content"])
		delete(n, "content")
	case hasParts:
		n["parts"] = normalizeParts(n["parts"])
	default:
		if t, hasText := n["text"]; hasText {
			n["parts"] = []any{map[string]any{"text": toString(t)}}
			delete(n, "text")
		} else {
			n["parts"] = []any{}
		}
	}
	switch role, _ := n["role"].(string); role {
	case "assistant":
		n["role"] = "model"
	case "tool":
		n["role"] = "function"
	case "":
		n["role"] = "user"
	}
	return n
}

// normalizeParts 把 parts 归一为 part 列表。
func normalizeParts(parts any) []any {
	switch p := parts.(type) {
	case nil:
		return []any{}
	case string:
		return []any{map[string]any{"text": p}}
	case map[string]any:
		return []any{normalizePart(p)}
	case []any:
		out := []any{}
		for _, item := range p {
			if s, ok := item.(string); ok {
				out = append(out, map[string]any{"text": s})
			} else if m, ok := item.(map[string]any); ok {
				if np := normalizePart(m); len(np) > 0 {
					out = append(out, np)
				}
			}
		}
		return out
	default:
		return []any{map[string]any{"text": toString(parts)}}
	}
}

// normalizePart 把 OpenAI 风格 part 归一为 Gemini part：
// text/input_text → {text}；image_url/input_image → inlineData(data:) 或 fileData(http/gs:)；
// media/file/file_data → fileData；inline_data/inlineData → inlineData；其余键 camelCase 透传。
// 供原生 Gemini 端点接收混入 OpenAI 风格 part 时正确转换（OpenAI chat 端点走 convertUserContent，不经此）。
func normalizePart(part map[string]any) map[string]any {
	pt, _ := part["type"].(string)
	switch pt {
	case "text", "input_text":
		return map[string]any{"text": toString(part["text"])}

	case "image_url", "input_image":
		var url string
		switch u := firstNonEmpty(part["image_url"], part["input_image"]).(type) {
		case map[string]any:
			url = toString(u["url"])
		case string:
			url = u
		}
		if strings.HasPrefix(url, "data:") {
			if mime, data := parseDataURI(url); mime != "" && data != "" {
				return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
			}
		}
		if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "gs://") {
			return map[string]any{"fileData": map[string]any{"mimeType": guessMIMEFromURI(url), "fileUri": url}}
		}

	case "media", "file", "file_data":
		fileURI := toString(firstNonEmpty(part["fileUri"], part["file_uri"], part["uri"], part["url"]))
		if fileURI != "" {
			mime := firstTruthyString(part["mimeType"], part["mime_type"])
			if mime == "" {
				mime = guessMIMEFromURI(fileURI)
			}
			return map[string]any{"fileData": map[string]any{"mimeType": mime, "fileUri": fileURI}}
		}

	case "inline_data", "inlineData":
		inline := part
		if m, ok := part["inlineData"].(map[string]any); ok {
			inline = m
		} else if m, ok := part["inline_data"].(map[string]any); ok {
			inline = m
		}
		data := toString(inline["data"])
		mime := firstTruthyString(inline["mimeType"], inline["mime_type"], part["mimeType"], part["mime_type"])
		if data != "" && mime != "" {
			return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
		}
	}

	// 默认：键 camelCase 透传（未知 part 类型保持原行为）。
	out := map[string]any{}
	for k, v := range part {
		if k == "type" {
			continue
		}
		out[SnakeToCamel(k)] = v
	}
	return out
}

// handleInlineDataCase 递归把键 camelCase 化，修正 inlineData 内层字段名，并**保留
// functionCall/functionResponse 上的 id 字段**（id 锚点：穿线给 filterEmptyContents 的
// callIDMap 按 id 反查 name）。
func handleInlineDataCase(contents any) any {
	switch c := contents.(type) {
	case []any:
		out := make([]any, len(c))
		for i, item := range c {
			out[i] = handleInlineDataCase(item)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, v := range c {
			camelK := SnakeToCamel(k)
			switch camelK {
			case "inlineData":
				if vm, ok := v.(map[string]any); ok {
					nid := map[string]any{}
					for ik, iv := range vm {
						nid[SnakeToCamel(ik)] = iv
					}
					out["inlineData"] = nid
					continue
				}
				out[camelK] = handleInlineDataCase(v)
			case "functionCall":
				if vm, ok := v.(map[string]any); ok {
					out["functionCall"] = camelizeFunctionRef(vm, "args")
					continue
				}
				out[camelK] = handleInlineDataCase(v)
			case "functionResponse":
				if vm, ok := v.(map[string]any); ok {
					out["functionResponse"] = camelizeFunctionRef(vm, "response")
					continue
				}
				out[camelK] = handleInlineDataCase(v)
			default:
				out[camelK] = handleInlineDataCase(v)
			}
		}
		return out
	default:
		return contents
	}
}

// camelizeFunctionRef 处理 functionCall/functionResponse 分支：
// 保留 id（穿线锚点），payloadKey（args/response）原样保留，其余键 camelCase 化并递归。
// id/toolCallId 不重复写入（id 已在前置提取并保留为 "id"）。
func camelizeFunctionRef(v map[string]any, payloadKey string) map[string]any {
	out := map[string]any{}
	// id 锚点：优先 id / tool_call_id / toolCallId，统一存为 "id"。
	if fid := firstTruthyString(v["id"], v["tool_call_id"], v["toolCallId"]); fid != "" {
		out["id"] = fid
	}
	for ik, iv := range v {
		cik := SnakeToCamel(ik)
		switch cik {
		case payloadKey:
			out[cik] = iv // args/response 原样保留，不递归改键
		case "id", "toolCallId":
			// 已在上方统一处理为 "id"，跳过避免重复。
		default:
			out[cik] = handleInlineDataCase(iv)
		}
	}
	return out
}

// filterEmptyContents 对每个 content 的 parts 逐个清洗，删除清洗后无有效 part 的 content
// （Vertex 要求每个 content 至少有一个 part）。id 锚点增强版。
//
// 工具调用 name 回填（组合最优解）：
//   - role==model 时收集该消息内 functionCall 的 name 列表，并按 id 建 callIDMap（id→name）。
//   - role==function 时把 responseIndex 归零。
//   - 每个 functionResponse 缺 name 时：① 先按其 id 在 callIDMap 反查（并行工具调用不错配）；
//     ② 再按 responseIndex 在当前 model 的 name 列表按位置兜底。
//
// 这比纯顺序推断鲁棒：并行工具调用各自带 id，按 id 反查能精确配对。
func filterEmptyContents(contents any) any {
	list, ok := contents.([]any)
	if !ok {
		return contents
	}

	callIDMap := map[string]string{}    // id → functionCall.name（跨 content 累积）
	var lastModelFunctionCalls []string // 最近一个 model 消息里的 functionCall name（按出现顺序）
	responseIndex := 0                  // 自最近 model 消息以来 functionResponse 的累计位置（按位置兜底用）

	filtered := []any{}
	for _, c := range list {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		role, _ := cm["role"].(string)
		parts := asAnySlice(cm["parts"])

		if role == "model" {
			// 新一轮工具调用：重置 name 列表 + 位置计数，收集本 model 消息的 functionCall。
			// 位置计数随 functionResponse 全局推进（不按 function 消息重置）——因为 OpenAI 把
			// 每个 tool 结果拆成独立 message（各成一个 function content、各含 1 个 functionResponse），
			// 按 content 重置会让多个响应都落到 index 0、配错 name。
			lastModelFunctionCalls = nil
			responseIndex = 0
			for _, p := range parts {
				if pm, ok := p.(map[string]any); ok {
					if fc, ok := pm["functionCall"].(map[string]any); ok {
						if name, _ := fc["name"].(string); strings.TrimSpace(name) != "" {
							lastModelFunctionCalls = append(lastModelFunctionCalls, name)
							if fid, _ := fc["id"].(string); fid != "" {
								callIDMap[fid] = name
							}
						}
					}
				}
			}
		}

		var cleanedParts []any
		for _, p := range parts {
			pm, ok := p.(map[string]any)
			if !ok {
				continue
			}
			_, isFuncResponse := pm["functionResponse"]
			idx := -1
			if isFuncResponse {
				idx = responseIndex
				responseIndex++
			}
			if cleaned, ok := cleanPartWithID(pm, lastModelFunctionCalls, idx, callIDMap); ok {
				cleanedParts = append(cleanedParts, cleaned)
			}
		}
		if len(cleanedParts) > 0 {
			nc := copyMap(cm)
			nc["parts"] = cleanedParts
			filtered = append(filtered, nc)
		}
	}
	return filtered
}

// mergeContiguousRoles 合并相邻同 role 的 content。
//
// OpenAI 多轮工具调用天然产生连续同角色结构：
//
//	role=model:    [functionCall_A, functionCall_B]   ← 一次两个工具调用
//	role=function: [functionResponse_A]               ← 工具 A 结果（独立 message）
//	role=function: [functionResponse_B]               ← 工具 B 结果（独立 message，连续同 role！）
//
// Vertex AI 要求相邻 content 的 role 必须交替，连续同 role 会 400。
// 此函数把相邻同 role 的 parts 合并到第一个 content 里，删除后续重复 content。
func mergeContiguousRoles(contents any) any {
	list, ok := contents.([]any)
	if !ok || len(list) == 0 {
		return contents
	}

	merged := []any{list[0]}
	for _, c := range list[1:] {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		prev, ok := merged[len(merged)-1].(map[string]any)
		if !ok {
			merged = append(merged, cm)
			continue
		}
		role, _ := cm["role"].(string)
		prevRole, _ := prev["role"].(string)
		if role == prevRole {
			prevParts := asAnySlice(prev["parts"])
			curParts := asAnySlice(cm["parts"])
			prev["parts"] = append(prevParts, curParts...)
		} else {
			merged = append(merged, cm)
		}
	}
	return merged
}

// buildGenerationConfig 构建 generationConfig（里程碑1 无 kwargs）。
func buildGenerationConfig(geminiPayload map[string]any) map[string]any {
	final := map[string]any{}
	if ugc, ok := geminiPayload["generationConfig"].(map[string]any); ok {
		for k, v := range ugc {
			final[k] = v
		}
	} else if ugc, ok := geminiPayload["generation_config"].(map[string]any); ok {
		for k, v := range ugc {
			final[k] = v
		}
	}
	return convertToGeminiFormat(final)
}

// convertToGeminiFormat 把 generationConfig 转为 Gemini 期望格式。
func convertToGeminiFormat(cfg map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range cfg {
		camelKey := SnakeToCamel(k)
		switch camelKey {
		case "thinkingConfig":
			if vm, ok := v.(map[string]any); ok {
				tc, _ := camelizeNested(vm).(map[string]any)
				if lvl, ok := tc["thinkingLevel"].(string); ok {
					tc["thinkingLevel"] = strings.ToUpper(lvl)
				}
				out[camelKey] = tc
				continue
			}
			out[camelKey] = v
		case "imageConfig", "speechConfig", "audioTimestamp", "routingConfig":
			if vm, ok := v.(map[string]any); ok {
				out[camelKey] = camelizeNested(vm)
				continue
			}
			out[camelKey] = v
		case "responseSchema":
			out[camelKey] = toNativeSchema(v)
		case "topK":
			out[camelKey] = clampTopK(v)
		default:
			out[camelKey] = v
		}
	}
	return out
}

// clampTopK 把 topK 限制到 ≤63。
func clampTopK(v any) any {
	switch n := v.(type) {
	case float64:
		if n > 63 {
			return 63
		}
		return int(n)
	case int:
		if n > 63 {
			return 63
		}
		return n
	default:
		return v
	}
}

// camelizeNested 深度遍历键 camelCase 化。
func camelizeNested(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			out[SnakeToCamel(k)] = camelizeNested(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = camelizeNested(item)
		}
		return out
	default:
		return v
	}
}

// buildSafetySettings 构建默认安全设置。
func buildSafetySettings(cfg config.AppConfig) []any {
	out := make([]any, 0, len(safetyCategories))
	for _, cat := range safetyCategories {
		threshold := "BLOCK_NONE"
		if t, ok := cfg.SafetySettings[cat]; ok && t != "" {
			threshold = t
		}
		out = append(out, map[string]any{"category": cat, "threshold": threshold})
	}
	return out
}

// parseModelName 解析模型名：经 models.json 的 alias_map 重映射。
// 命中别名返回真名，否则原样透传。
func parseModelName(model string) string {
	return config.ResolveModelName(model)
}

// toolKeys 是 Vertex AI tools 列表里可与 functionDeclarations 共存的内置工具键集合。
var toolKeys = map[string]bool{ //nolint:gochecknoglobals
	"functionDeclarations": true, "googleSearch": true, "googleSearchRetrieval": true,
	"codeExecution": true, "retrieval": true, "urlContext": true,
}

// normalizeToolsFormat 把 tools 归一为 Vertex AI 期望的 List[Tool]：
// 先 camelCase 化，再把裸 FunctionDeclaration 聚合进一个 functionDeclarations Tool，
// 其余携带 tool_keys 的条目（内置工具/已包好的 Tool）原样保留，二者可同时存在。
func normalizeToolsFormat(tools any) []any {
	converted := convertToolsFormat(tools)

	// dict 形态：含 tool_keys 任一 → 包成列表；单个裸 FunctionDeclaration → 包两层。
	if cm, ok := converted.(map[string]any); ok {
		for k := range cm {
			if toolKeys[k] {
				return []any{cm}
			}
		}
		if _, ok := cm["name"]; ok {
			return []any{map[string]any{"functionDeclarations": []any{cm}}}
		}
		return nil
	}

	list, ok := converted.([]any)
	if !ok || len(list) == 0 {
		return nil
	}

	var normalized []any
	var funcDecls []any
	for _, item := range list {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		hasToolKey := false
		for k := range im {
			if toolKeys[k] {
				hasToolKey = true
				break
			}
		}
		if hasToolKey {
			normalized = append(normalized, im)
		} else if _, ok := im["name"]; ok {
			funcDecls = append(funcDecls, im)
		}
	}
	if len(funcDecls) > 0 {
		normalized = append([]any{map[string]any{"functionDeclarations": funcDecls}}, normalized...)
	}
	return normalized
}

// convertToolsFormat 递归把工具结构转为 camelCase。
// 统一 function_declarations→functionDeclarations、内置工具键 camelCase；空 name 字段剔除；
// parameters 已是清洗过的标准 JSON Schema，原样保留（Gemini functionDeclarations 接受标准 schema）。
func convertToolsFormat(data any) any {
	switch d := data.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, v := range d {
			switch k {
			case "function_declarations", "functionDeclarations":
				out["functionDeclarations"] = convertToolsFormat(v)
			case "google_search", "googleSearch":
				out["googleSearch"] = convertToolsFormatLeaf(v)
			case "google_search_retrieval", "googleSearchRetrieval":
				out["googleSearchRetrieval"] = convertToolsFormatLeaf(v)
			case "code_execution", "codeExecution":
				out["codeExecution"] = convertToolsFormatLeaf(v)
			case "url_context", "urlContext":
				out["urlContext"] = convertToolsFormatLeaf(v)
			case "name":
				if isTruthy(v) { // Vertex AI function name 不能为空，空则剔除
					out["name"] = v
				}
			case "parameters", "parametersJsonSchema", "parameters_json_schema":
				// 统一拦截所有 schema 键名，转为原生 schema 格式，防止属性被误转为驼峰
				out["parameters"] = toNativeSchema(v)
			default:
				camelKey := k
				if strings.Contains(k, "_") {
					camelKey = SnakeToCamel(k)
				}
				out[camelKey] = convertToolsFormatLeaf(v)
			}
		}
		return out
	case []any:
		out := make([]any, len(d))
		for i, item := range d {
			out[i] = convertToolsFormat(item)
		}
		return out
	default:
		return data
	}
}

// convertToolsFormatLeaf 仅对 dict/list 递归，标量原样返回。
func convertToolsFormatLeaf(v any) any {
	switch v.(type) {
	case map[string]any, []any:
		return convertToolsFormat(v)
	default:
		return v
	}
}

// asAnySlice 把 any 规整为 []any（非数组返回 nil）。
func asAnySlice(v any) []any {
	if arr, ok := v.([]any); ok {
		return arr
	}
	return nil
}
