// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"encoding/json"
	"strconv"
	"strings"
)

// 本文件汇集 chat 请求转换用到的查表常量与解析小工具
// （reasoning_effort / media_resolution / image_size / input_audio 映射、
// data URI 解析、tool_call/function 提取、schema 白名单清洗）。
// 拆出来与 request.go 的主流程分开，便于对照与测试。

// reasoningEffortToThinkingLevel 把 OpenAI reasoning_effort 映射到 Gemini 3.x
// thinkingConfig.thinkingLevel。
// minimal/low/medium/high 与 Gemini 四档 1:1；none→NONE、xhigh→HIGH 收敛。
//
//nolint:gochecknoglobals // Read-only mapping
var reasoningEffortToThinkingLevel = map[string]string{
	"none":    "NONE",
	"minimal": "MINIMAL",
	"low":     "LOW",
	"medium":  "MEDIUM",
	"high":    "HIGH",
	"xhigh":   "HIGH",
}

// audioFormatMIME 把 input_audio.format 映射到 Gemini inlineData mimeType。
// 未知格式由调用处兜底为 audio/wav。
//
//nolint:gochecknoglobals // Read-only mapping
var audioFormatMIME = map[string]string{
	"wav":  "audio/wav",
	"mp3":  "audio/mpeg",
	"mpeg": "audio/mpeg",
	"mpga": "audio/mpeg",
	"m4a":  "audio/aac",
	"aac":  "audio/aac",
	"ogg":  "audio/ogg",
	"oga":  "audio/ogg",
	"opus": "audio/ogg",
	"flac": "audio/flac",
	"webm": "audio/webm",
	"pcm":  "audio/L16",
	"l16":  "audio/L16",
}

// imageSizeAllowed 是 Gemini imageConfig.imageSize 接受的档位（大写 K 逐字节透传）。
//
//nolint:gochecknoglobals // Read-only set
var imageSizeAllowed = map[string]bool{"512": true, "1K": true, "2K": true, "4K": true}

// pixelToImageSize 把像素长边映射到档位（取较大维度、>= 阈值）。
//
//nolint:gochecknoglobals // Read-only mapping
var pixelToImageSize = []struct { //nolint:govet
	threshold int
	tier      string
}{
	{4096, "4K"},
	{2048, "2K"},
	{1024, "1K"},
	{512, "512"},
}

// mediaResolutionAllowed 是 generationConfig.mediaResolution 的完整枚举集合。
//
//nolint:gochecknoglobals // Read-only set
var mediaResolutionAllowed = map[string]bool{
	"MEDIA_RESOLUTION_UNSPECIFIED": true,
	"MEDIA_RESOLUTION_LOW":         true,
	"MEDIA_RESOLUTION_MEDIUM":      true,
	"MEDIA_RESOLUTION_HIGH":        true,
	"MEDIA_RESOLUTION_ULTRA_HIGH":  true,
}

// mediaResolutionShorthand 接受简写并归一到完整枚举。
//
//nolint:gochecknoglobals // Read-only mapping
var mediaResolutionShorthand = map[string]string{
	"low":         "MEDIA_RESOLUTION_LOW",
	"medium":      "MEDIA_RESOLUTION_MEDIUM",
	"med":         "MEDIA_RESOLUTION_MEDIUM",
	"high":        "MEDIA_RESOLUTION_HIGH",
	"unspecified": "MEDIA_RESOLUTION_UNSPECIFIED",
	"default":     "MEDIA_RESOLUTION_UNSPECIFIED",
	"ultra_high":  "MEDIA_RESOLUTION_ULTRA_HIGH",
	"ultra-high":  "MEDIA_RESOLUTION_ULTRA_HIGH",
	"ultrahigh":   "MEDIA_RESOLUTION_ULTRA_HIGH",
}

// geminiAllowedSchemaFields 是 functionDeclarations.parameters 的 JSON Schema 字段白名单。
// 剔除 $schema/additionalProperties/$ref 等上游不支持的关键字，避免 400。
//
//nolint:gochecknoglobals // Read-only set
var geminiAllowedSchemaFields = map[string]bool{
	"anyOf": true, "default": true, "description": true, "enum": true,
	"example": true, "format": true, "items": true,
	"maxItems": true, "maxLength": true, "maxProperties": true, "maximum": true,
	"minItems": true, "minLength": true, "minProperties": true, "minimum": true,
	"nullable": true, "pattern": true, "properties": true, "propertyOrdering": true,
	"required": true, "title": true, "type": true,
}

// normalizeMediaResolution 把任意写法规范成 Gemini 枚举，无法识别返回 ""。
func normalizeMediaResolution(value any) string {
	s, ok := value.(string)
	if !ok {
		return ""
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	upper := strings.ToUpper(s)
	if mediaResolutionAllowed[upper] {
		return upper
	}
	// 带前缀但未知后缀的也原样大写透传，由上游裁决。
	if strings.HasPrefix(upper, "MEDIA_RESOLUTION_") {
		return upper
	}
	return mediaResolutionShorthand[strings.ToLower(s)]
}

// normalizeImageSize 把任意分辨率输入规范成档位字符串（512/1K/2K/4K）或 ""。
func normalizeImageSize(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case float64:
		return pixelsToTier(int(v))
	case int:
		return pixelsToTier(v)
	case string:
		s := strings.TrimSpace(v)
		if s == "" {
			return ""
		}
		if imageSizeAllowed[strings.ToUpper(s)] {
			return strings.ToUpper(s)
		}
		low := strings.ToLower(s)
		if strings.Contains(low, "x") {
			parts := strings.SplitN(low, "x", 2)
			if len(parts) >= 2 {
				w, errW := strconv.Atoi(strings.TrimSpace(parts[0]))
				h, errH := strconv.Atoi(strings.TrimSpace(parts[1]))
				if errW != nil || errH != nil {
					return ""
				}
				return pixelsToTier(maxInt(w, h))
			}
		}
		if isAllDigits(s) {
			n, err := strconv.Atoi(s)
			if err != nil {
				return ""
			}
			return pixelsToTier(n)
		}
		return ""
	default:
		return ""
	}
}

// pixelsToTier 把像素长边映射到不超过它的最大档位；<512 返回 ""。
func pixelsToTier(px int) string {
	for _, p := range pixelToImageSize {
		if px >= p.threshold {
			return p.tier
		}
	}
	return ""
}

// ApplyImageConfig 原地把客户端分辨率/imageConfig 写入
// geminiPayload.generationConfig.imageConfig.imageSize。
// 优先级：body["imageConfig"] 顶层透传 > image_size/imageSize > size。全 additive。
func ApplyImageConfig(geminiPayload, body map[string]any) {
	var imageSize string
	var passthrough map[string]any

	if raw, ok := body["imageConfig"].(map[string]any); ok && len(raw) > 0 {
		passthrough = raw
	}

	if passthrough == nil {
		for _, key := range []string{"image_size", "imageSize"} {
			if v, ok := body[key]; ok && v != nil {
				if tier := normalizeImageSize(v); tier != "" {
					imageSize = tier
					break
				}
			}
		}
	}

	if passthrough == nil && imageSize == "" {
		if v, ok := body["size"]; ok && v != nil {
			if tier := normalizeImageSize(v); tier != "" {
				imageSize = tier
			}
		}
	}

	if passthrough == nil && imageSize == "" {
		return
	}

	genCfg, ok := geminiPayload["generationConfig"].(map[string]any)
	if !ok {
		genCfg = map[string]any{}
		geminiPayload["generationConfig"] = genCfg
	}

	if passthrough != nil {
		if existing, ok := genCfg["imageConfig"].(map[string]any); ok {
			for k, v := range passthrough {
				existing[k] = v
			}
		} else {
			genCfg["imageConfig"] = copyMap(passthrough)
		}
		return
	}

	imgCfg, ok := genCfg["imageConfig"].(map[string]any)
	if !ok {
		imgCfg = map[string]any{}
		genCfg["imageConfig"] = imgCfg
	}
	imgCfg["imageSize"] = imageSize
}

// parseDataURI 解析 "data:mime;base64,DATA"，返回 (mime, data)；失败返回 ("","")。
func parseDataURI(uri string) (string, string) {
	idx := strings.Index(uri, ",")
	if idx < 0 {
		return "", ""
	}
	header := uri[:idx]
	data := uri[idx+1:]
	// header 形如 "data:image/png;base64"
	colon := strings.Index(header, ":")
	if colon < 0 {
		return "", ""
	}
	mime := header[colon+1:]
	if semi := strings.Index(mime, ";"); semi >= 0 {
		mime = mime[:semi]
	}
	return mime, data
}

// guessMIMEFromURL 按扩展名猜图片 mime（默认 image/png）。
func guessMIMEFromURL(url string) string {
	lower := trimLowerSuffix(url)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	default:
		return "image/png"
	}
}

// guessMIMEFromURI 按扩展名猜 mime，覆盖图/视频/音频/pdf/txt（默认 image/png）。
// 比 guessMIMEFromURL 多了非图片类型，供 normalizePart 的 media/file 用。
func guessMIMEFromURI(uri string) string {
	lower := trimLowerSuffix(uri)
	switch {
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".webp"):
		return "image/webp"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".mp4"):
		return "video/mp4"
	case strings.HasSuffix(lower, ".mov"):
		return "video/quicktime"
	case strings.HasSuffix(lower, ".webm"):
		return "video/webm"
	case strings.HasSuffix(lower, ".mp3"):
		return "audio/mpeg"
	case strings.HasSuffix(lower, ".wav"):
		return "audio/wav"
	case strings.HasSuffix(lower, ".ogg"):
		return "audio/ogg"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	default:
		return "image/png"
	}
}

// oaiToolCall 是从 OpenAI tool_call 提取出的归一结果。
type oaiToolCall struct { //nolint:govet
	id   string // 可能为空
	name string // 非空（否则不生成）
	args any    // dict/对象（字符串已 JSON 解析）
}

// extractOAIToolCall 健壮解析 OpenAI tool_call，兼容标准与常见非标准形态。
// 无名（空 name）返回 (nil)。
func extractOAIToolCall(tc any) *oaiToolCall {
	m, ok := tc.(map[string]any)
	if !ok {
		return nil
	}
	id := firstTruthyString(m["id"], m["tool_call_id"], m["call_id"])

	var name string
	var args any
	if fn, ok := m["function"].(map[string]any); ok {
		name = firstTruthyString(fn["name"], m["name"])
		args = firstPresent(fn, "arguments", m, "arguments", "args")
	} else {
		name = firstTruthyString(m["name"], m["function_name"])
		args = firstPresentIn(m, "arguments", "args")
	}
	if name == "" {
		return nil
	}
	return &oaiToolCall{id: id, name: name, args: coerceFunctionArgs(args)}
}

// extractOAIFunctionTool 从 tools 项提取 function 声明，兼容标准与非标准形态。
// 返回带 name 的 function map。
func extractOAIFunctionTool(tool any) map[string]any {
	m, ok := tool.(map[string]any)
	if !ok {
		return nil
	}
	if m["type"] == "function" {
		if fn, ok := m["function"].(map[string]any); ok {
			if truthyStr(fn["name"]) {
				return fn
			}
			return nil
		}
	}
	if fnStr, ok := m["function"].(string); ok && fnStr != "" {
		copied := copyMap(m)
		delete(copied, "function")
		copied["name"] = fnStr
		if truthyStr(copied["name"]) {
			return copied
		}
		return nil
	}
	if m["type"] == "function" && truthyStr(m["name"]) {
		return m
	}
	if truthyStr(m["name"]) {
		_, hasParams := m["parameters"]
		_, hasDesc := m["description"]
		if hasParams || hasDesc {
			return m
		}
	}
	return nil
}

// coerceFunctionArgs 把 tool_call.arguments 规范成 dict/对象（字符串则尝试 JSON 解析）。
func coerceFunctionArgs(args any) any {
	if s, ok := args.(string); ok {
		var parsed any
		if err := json.Unmarshal([]byte(s), &parsed); err == nil {
			return parsed
		}
		return map[string]any{"raw": s}
	}
	if args == nil {
		return map[string]any{}
	}
	return args
}

// cleanFunctionParameters 递归用 Gemini 白名单清洗 JSON Schema，剔除上游不支持的字段。
func cleanFunctionParameters(schema any) any {
	switch s := schema.(type) {
	case []any:
		out := make([]any, len(s))
		for i, item := range s {
			out[i] = cleanFunctionParameters(item)
		}
		return out
	case map[string]any:
		cleaned := map[string]any{}
		for key, value := range s {
			if !geminiAllowedSchemaFields[key] {
				continue
			}
			switch key {
			case "properties":
				if vm, ok := value.(map[string]any); ok {
					props := map[string]any{}
					for k, v := range vm {
						props[k] = cleanFunctionParameters(v)
					}
					cleaned[key] = props
					continue
				}
				cleaned[key] = value
			case "items":
				if _, ok := value.(map[string]any); ok {
					cleaned[key] = cleanFunctionParameters(value)
					continue
				}
				cleaned[key] = value
			case "anyOf":
				if vl, ok := value.([]any); ok {
					out := make([]any, len(vl))
					for i, item := range vl {
						out[i] = cleanFunctionParameters(item)
					}
					cleaned[key] = out
					continue
				}
				cleaned[key] = value
			default:
				cleaned[key] = value
			}
		}
		return cleaned
	default:
		return schema
	}
}

// schemaUnsupportedKeys 是 Vertex AI 原生 Schema 不支持、需剥离的 JSON-Schema 关键字。
// 注意：Gemini API 原生支持 default/nullable/examples（官方 Schema 规范有定义），
// 只删真正不支持的 $schema/$defs/allOf 等 JSON-Schema 元关键字，否则会丢失参数语义。
//
//nolint:gochecknoglobals // Read-only set
var schemaUnsupportedKeys = map[string]bool{
	"$schema": true, "$id": true, "$defs": true, "$ref": true, "definitions": true,
	"additionalProperties": true, "patternProperties": true, "unevaluatedProperties": true,
	"dependentSchemas": true, "if": true, "then": true, "else": true,
	"allOf": true, "anyOf": true, "oneOf": true, "not": true,
	"title": true,
}

// toNativeSchema 把标准 JSON Schema 转为 Vertex AI 匿名 UI 端点要求的原生 Map-style Schema。
// 关键差异（不这么转上游会 400 或忽略 schema）：
//   - type 必须大写枚举（STRING/OBJECT/INTEGER...）；type 联合（["string","null"]）取首个非 null；
//     未知类型兜底 STRING（防上游静默忽略整个工具声明）。
//   - properties 从 dict 转为 [{key, value}] 列表（递归 value）。
//   - items 递归转换；剥离不支持关键字。
//   - 数值约束（minItems/maxItems/minLength/maxLength/minProperties/maxProperties）转字符串
//     （原生 schema 规范要求 string，不转会被静默忽略导致参数约束丢失、模型不调工具）。
func toNativeSchema(schema any) any {
	m, ok := schema.(map[string]any)
	if !ok {
		return schema
	}
	out := map[string]any{}
	for k, v := range m {
		if schemaUnsupportedKeys[k] {
			continue
		}
		out[k] = v
	}

	// type：联合归一 + 大写 + 未知值兜底 STRING。
	switch t := out["type"].(type) {
	case []any:
		picked := "string"
		for _, item := range t {
			if s, ok := item.(string); ok && s != "null" {
				picked = s
				break
			}
		}
		out["type"] = strings.ToUpper(picked)
	case string:
		out["type"] = strings.ToUpper(t)
	default:
		out["type"] = "OBJECT"
	}
	// 未知类型兜底 STRING（只接受 6 种合法枚举，其余一律 STRING）。
	validTypes := map[string]bool{
		"STRING": true, "INTEGER": true, "NUMBER": true,
		"BOOLEAN": true, "ARRAY": true, "OBJECT": true,
	}
	if !validTypes[out["type"].(string)] {
		out["type"] = "STRING"
	}

	// properties：dict → [{key, value}] 列表（递归）。
	if props, ok := out["properties"].(map[string]any); ok {
		nativeProps := make([]any, 0, len(props))
		for key, value := range props {
			// 默认保留原值；仅当 value 是嵌套 schema(map)时才递归转原生格式，非 map 值不丢失。
			converted := value
			if vm, ok := value.(map[string]any); ok {
				converted = toNativeSchema(vm)
			}
			nativeProps = append(nativeProps, map[string]any{"key": key, "value": converted})
		}
		out["properties"] = nativeProps
	}

	// items：递归。
	if items, ok := out["items"].(map[string]any); ok {
		out["items"] = toNativeSchema(items)
	}

	// 数值约束 → 字符串（原生 schema 规范要求 string）。
	numericConstraints := []string{"minItems", "maxItems", "minProperties", "maxProperties", "minLength", "maxLength"}
	for _, field := range numericConstraints {
		if v, ok := out[field]; ok && v != nil {
			switch n := v.(type) {
			case float64:
				out[field] = strconv.FormatFloat(n, 'f', 0, 64)
			case int:
				out[field] = strconv.Itoa(n)
			case int64:
				out[field] = strconv.FormatInt(n, 10)
			case string:
				// 已经是字符串，保持
			}
		}
	}

	return out
}

// ---- 通用小工具 ----

// firstTruthyString 返回参数里第一个非空字符串（按「第一个非空者优先」语义）。
func firstTruthyString(vals ...any) string {
	for _, v := range vals {
		if s, ok := v.(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// firstPresent 在两个 map 里依次查 keys，返回第一个存在的值；都没有返回 {}。
// 回退顺序：m1[k1] → m2[k2] → m2[k3] → {}。
func firstPresent(m1 map[string]any, k1 string, m2 map[string]any, k2, k3 string) any {
	if v, ok := m1[k1]; ok {
		return v
	}
	if v, ok := m2[k2]; ok {
		return v
	}
	if v, ok := m2[k3]; ok {
		return v
	}
	return map[string]any{}
}

// firstPresentIn 在一个 map 里依次查 keys，返回第一个存在的值；都没有返回 {}。
func firstPresentIn(m map[string]any, keys ...string) any {
	for _, k := range keys {
		if v, ok := m[k]; ok {
			return v
		}
	}
	return map[string]any{}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
