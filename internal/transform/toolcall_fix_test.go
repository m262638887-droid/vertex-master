// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// TestToNativeSchema_NumericConstraintsAsStrings 验证数值约束字段被转为字符串。
// Vertex AI 原生 Schema 要求 minItems/maxItems/minLength/maxLength/minProperties/maxProperties
// 必须是字符串类型，以数字发给上游会导致参数约束丢失、模型不调用工具。
func TestToNativeSchema_NumericConstraintsAsStrings(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"minItems": 1, "maxItems": float64(10),
		"properties": map[string]any{
			"name": map[string]any{
				"type":      "string",
				"minLength": 2, "maxLength": float64(50),
			},
		},
	}
	native := toNativeSchema(schema).(map[string]any)

	for _, field := range []string{"minItems", "maxItems"} {
		v, ok := native[field]
		if !ok {
			t.Fatalf("字段 %s 被删除", field)
		}
		if _, ok := v.(string); !ok {
			t.Errorf("%s 应为字符串，实际是 %T(%v)", field, v, v)
		}
	}
	props, _ := native["properties"].([]any)
	if len(props) > 0 {
		prop := props[0].(map[string]any)
		val := prop["value"].(map[string]any)
		for _, field := range []string{"minLength", "maxLength"} {
			v, ok := val[field]
			if !ok {
				t.Fatalf("嵌套字段 %s 被删除", field)
			}
			if _, ok := v.(string); !ok {
				t.Errorf("嵌套 %s 应为字符串，实际是 %T(%v)", field, v, v)
			}
		}
	}
}

// TestToNativeSchema_DefaultNullablePreserved 验证 Gemini 支持的 default/nullable/examples 不被误删。
func TestToNativeSchema_DefaultNullablePreserved(t *testing.T) {
	schema := map[string]any{
		"type":     "object",
		"default":  "hello",
		"nullable": true,
		"examples": []any{"ex1", "ex2"},
		"properties": map[string]any{
			"x": map[string]any{"type": "string", "default": "world"},
		},
	}
	native := toNativeSchema(schema).(map[string]any)
	for _, field := range []string{"default", "nullable", "examples"} {
		if _, ok := native[field]; !ok {
			t.Errorf("字段 %s 被错误剔除（Gemini 原生支持）", field)
		}
	}
	props, _ := native["properties"].([]any)
	if len(props) > 0 {
		val := props[0].(map[string]any)["value"].(map[string]any)
		if _, ok := val["default"]; !ok {
			t.Errorf("嵌套 property 的 default 被错误剔除")
		}
	}
}

// TestToNativeSchema_UnknownTypeFallsBackToSTRING 验证非标准 type 兜底 STRING。
func TestToNativeSchema_UnknownTypeFallsBackToSTRING(t *testing.T) {
	native := toNativeSchema(map[string]any{"type": "any", "properties": map[string]any{}}).(map[string]any)
	if native["type"] != "STRING" {
		t.Errorf("未知类型 'any' 应兜底为 STRING，实际: %v", native["type"])
	}
}

// TestMergeContiguousRoles 验证相邻同 role content 被合并。
func TestMergeContiguousRoles(t *testing.T) {
	contents := []any{
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "hi"}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "q1"}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "q2"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "bye"}}},
	}
	merged := mergeContiguousRoles(contents).([]any)
	if len(merged) != 3 {
		t.Fatalf("应合并为 3 个 content，实际 %d", len(merged))
	}
	user := merged[1].(map[string]any)
	if parts := user["parts"].([]any); len(parts) != 2 {
		t.Errorf("user content 应有 2 个 parts，实际 %d", len(parts))
	}
}

// TestMergeContiguousRoles_FunctionResponse 合并多轮工具结果（连续 function 角色）。
func TestMergeContiguousRoles_FunctionResponse(t *testing.T) {
	contents := []any{
		map[string]any{"role": "model", "parts": []any{
			map[string]any{"functionCall": map[string]any{"name": "get_weather", "args": map[string]any{"city": "NY"}}},
			map[string]any{"functionCall": map[string]any{"name": "get_time", "args": map[string]any{"tz": "EST"}}},
		}},
		map[string]any{"role": "function", "parts": []any{
			map[string]any{"functionResponse": map[string]any{"name": "get_weather", "response": map[string]any{"temp": 20}}},
		}},
		map[string]any{"role": "function", "parts": []any{
			map[string]any{"functionResponse": map[string]any{"name": "get_time", "response": map[string]any{"time": "10:00"}}},
		}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "done"}}},
	}
	merged := mergeContiguousRoles(contents).([]any)
	if len(merged) != 3 {
		t.Fatalf("应合并为 3 个 content，实际 %d", len(merged))
	}
	funcContent := merged[1].(map[string]any)
	if parts := funcContent["parts"].([]any); len(parts) != 2 {
		t.Errorf("function content 应合并两个 functionResponse 为 2 个 parts，实际 %d", len(parts))
	}
}

// TestConvertToolsFormat_NumericConstraints 端到端验证工具参数数值约束转字符串。
func TestConvertToolsFormat_NumericConstraints(t *testing.T) {
	geminiPayload := map[string]any{
		"tools": []any{map[string]any{
			"functionDeclarations": []any{map[string]any{
				"name": "list_items",
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{},
					"minItems":   1,
					"maxItems":   float64(100),
				},
			}},
		}},
	}
	vars := BuildVertexVariables("gemini-3-flash", geminiPayload, config.AppConfig{}) //nolint:exhaustruct
	dump, _ := json.Marshal(vars["tools"])
	if !strings.Contains(string(dump), `"minItems":"1"`) {
		t.Errorf("minItems 应转为字符串 \"1\": %s", dump)
	}
}
