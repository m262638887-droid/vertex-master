// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

func TestSnakeToCamel(t *testing.T) {
	cases := map[string]string{
		"max_output_tokens": "maxOutputTokens",
		"top_p":             "topP",
		"topK":              "topK", // 无下划线原样
		"temperature":       "temperature",
		"thinking_config":   "thinkingConfig",
	}
	for in, want := range cases {
		if got := SnakeToCamel(in); got != want {
			t.Errorf("SnakeToCamel(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestCamelToSnake(t *testing.T) {
	if got := CamelToSnake("topP"); got != "top_p" {
		t.Errorf("CamelToSnake(topP)=%q", got)
	}
	if got := CamelToSnake("maxOutputTokens"); got != "max_output_tokens" {
		t.Errorf("CamelToSnake(maxOutputTokens)=%q", got)
	}
}

func TestNormalizeBase64(t *testing.T) {
	if got := NormalizeBase64("data:image/png;base64,AAAA"); got != "AAAA" {
		t.Errorf("data URI 剥离失败: %q", got)
	}
	if got := NormalizeBase64("a-b_c"); got != "a+b/c===" {
		t.Errorf("URL-safe+padding: %q, want a+b/c===", got)
	}
}

func TestConvertChatRequest_PlainText(t *testing.T) {
	cfg := config.DefaultConfig()
	body := map[string]any{
		"model": "gemini-3.1-flash",
		"messages": []any{
			map[string]any{"role": "system", "content": "你是助手"},
			map[string]any{"role": "user", "content": "你好"},
		},
		"temperature": 0.7,
		"max_tokens":  float64(100),
	}
	model, payload, err := ConvertChatRequest(body, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if model != "gemini-3.1-flash" {
		t.Errorf("model=%q", model)
	}
	contents, _ := payload["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d, want 1", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("role=%v, want user", c0["role"])
	}
	if c0["parts"].([]any)[0].(map[string]any)["text"] != "你好" {
		t.Errorf("user text mismatch")
	}
	si, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatal("missing systemInstruction")
	}
	if si["parts"].([]any)[0].(map[string]any)["text"] != "你是助手" {
		t.Error("system text mismatch")
	}
	gc := payload["generationConfig"].(map[string]any)
	if gc["temperature"] != 0.7 {
		t.Errorf("temperature=%v", gc["temperature"])
	}
	if gc["maxOutputTokens"] != float64(100) {
		t.Errorf("maxOutputTokens=%v", gc["maxOutputTokens"])
	}
}

func TestConvertChatRequest_EmptyMessages(t *testing.T) {
	_, _, err := ConvertChatRequest(map[string]any{"model": "m", "messages": []any{}}, config.DefaultConfig())
	if err == nil {
		t.Error("expected error for empty messages")
	}
}

func TestConvertChatRequest_MaxTokensInvalid(t *testing.T) {
	body := map[string]any{
		"model":      "m",
		"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		"max_tokens": float64(0),
	}
	if _, _, err := ConvertChatRequest(body, config.DefaultConfig()); err == nil {
		t.Error("expected error for max_tokens=0")
	}
}

func TestBuildVertexVariables_SafetyDefault(t *testing.T) {
	cfg := config.DefaultConfig()
	payload := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
	}}
	vars := BuildVertexVariables("gemini-3.1-flash", payload, cfg)
	if vars["model"] != "gemini-3.1-flash" {
		t.Error("model")
	}
	ss, ok := vars["safetySettings"].([]any)
	if !ok || len(ss) != 5 {
		t.Errorf("safetySettings=%v, want 5 BLOCK_NONE", vars["safetySettings"])
	}
	first := ss[0].(map[string]any)
	if first["threshold"] != "BLOCK_NONE" {
		t.Errorf("threshold=%v", first["threshold"])
	}
}

func TestBuildVertexVariables_SystemDemote(t *testing.T) {
	cfg := config.DefaultConfig()
	payload := map[string]any{
		"contents":          []any{},
		"systemInstruction": map[string]any{"parts": []any{map[string]any{"text": "sys"}}},
	}
	vars := BuildVertexVariables("m", payload, cfg)
	if _, ok := vars["systemInstruction"]; ok {
		t.Error("systemInstruction 应在无 user 时被降级删除")
	}
	contents := vars["contents"].([]any)
	if len(contents) != 1 {
		t.Fatalf("contents len=%d, want 1", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("降级后 role=%v, want user", c0["role"])
	}
}

func TestGeminiJSONToOAIJSON(t *testing.T) {
	resp := map[string]any{
		"candidates": []any{map[string]any{
			"content":      map[string]any{"parts": []any{map[string]any{"text": "Hello"}}, "role": "model"},
			"finishReason": "STOP",
		}},
		"usageMetadata": map[string]any{
			"promptTokenCount":     float64(5),
			"candidatesTokenCount": float64(1),
			"totalTokenCount":      float64(6),
		},
	}
	oai := GeminiJSONToOAIJSON(resp, "gemini-3.1-flash")
	if oai["object"] != "chat.completion" {
		t.Errorf("object=%v", oai["object"])
	}
	c0 := oai["choices"].([]any)[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Errorf("finish_reason=%v", c0["finish_reason"])
	}
	if c0["message"].(map[string]any)["content"] != "Hello" {
		t.Errorf("content=%v", c0["message"].(map[string]any)["content"])
	}
	usage := oai["usage"].(map[string]any)
	if usage["prompt_tokens"] != 5 || usage["completion_tokens"] != 1 || usage["total_tokens"] != 6 {
		t.Errorf("usage=%v", usage)
	}
}

func TestMapFinishReason(t *testing.T) {
	cases := []struct {
		in   string
		tool bool
		want string
	}{
		{"STOP", false, "stop"},
		{"FINISH_REASON_UNSPECIFIED", false, "stop"}, // 未知 → stop
		{"SAFETY", false, "content_filter"},
		{"MAX_TOKENS", false, "length"},
		{"STOP", true, "tool_calls"}, // 有工具调用覆盖
		{"", false, "stop"},
	}
	for _, c := range cases {
		if got := MapFinishReason(c.in, c.tool); got != c.want {
			t.Errorf("MapFinishReason(%q,%v)=%q, want %q", c.in, c.tool, got, c.want)
		}
	}
}

func TestMergeContentBlocks(t *testing.T) {
	merged := MergeContentBlocks([]map[string]any{
		{"text": "Hello "},
		{"text": "World"},
	})
	if len(merged) != 1 {
		t.Fatalf("merged len=%d, want 1", len(merged))
	}
	if merged[0]["text"] != "Hello World" {
		t.Errorf("merged text=%q", merged[0]["text"])
	}
}
