// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"testing"

	"github.com/bsfdsagfadg/vertex/internal/config"
)

// ---- randomDigits：n 位、全数字 ----

func TestRandomDigits(t *testing.T) {
	s := randomDigits(100)
	if len(s) != 100 {
		t.Fatalf("randomDigits(100) 长度应为 100，实际 %d", len(s))
	}
	for i, r := range s {
		if r < '0' || r > '9' {
			t.Fatalf("位置 %d 非数字字符: %q", i, r)
		}
	}

	if got := len(randomDigits(0)); got != 0 {
		t.Errorf("randomDigits(0) 长度应为 0，实际 %d", got)
	}
	if got := len(randomDigits(5)); got != 5 {
		t.Errorf("randomDigits(5) 长度应为 5，实际 %d", got)
	}
}

// firstPartText 取 contents[0].parts[0].text。
func firstPartText(t *testing.T, contents []any) string {
	t.Helper()
	c0, ok := contents[0].(map[string]any)
	if !ok {
		t.Fatalf("contents[0] 非 map")
	}
	parts, _ := c0["parts"].([]any)
	if len(parts) == 0 {
		t.Fatalf("contents[0].parts 为空")
	}
	p0, _ := parts[0].(map[string]any)
	s, _ := p0["text"].(string)
	return s
}

func is100Digits(s string) bool {
	if len(s) != 100 {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ---- injectAnti429：target=system ----

func TestInjectAnti429System(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: true, Anti429Target: "system"}} //nolint:exhaustruct
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
		},
	}
	s.injectAnti429(payload)

	si, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("应注入 systemInstruction")
	}
	parts, ok := si["parts"].([]any)
	if !ok || len(parts) == 0 {
		t.Fatalf("systemInstruction.parts 应非空")
	}
	p0, _ := parts[0].(map[string]any)
	txt, _ := p0["text"].(string)
	if !is100Digits(txt) {
		t.Errorf("systemInstruction.parts[0].text 应为 100 位数字串，实际 %q (len=%d)", txt, len(txt))
	}
}

// 默认 target 为空字符串时应回退 system。
func TestInjectAnti429DefaultTargetSystem(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: true, Anti429Target: ""}} //nolint:exhaustruct
	payload := map[string]any{}
	s.injectAnti429(payload)
	si, ok := payload["systemInstruction"].(map[string]any)
	if !ok {
		t.Fatalf("默认 target 应注入 systemInstruction")
	}
	parts, _ := si["parts"].([]any)
	p0, _ := parts[0].(map[string]any)
	if txt, _ := p0["text"].(string); !is100Digits(txt) {
		t.Errorf("应为 100 位数字串，实际 %q", txt)
	}
}

// system 注入应保留已有 parts（前插）。
func TestInjectAnti429SystemPrepends(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: true, Anti429Target: "system"}} //nolint:exhaustruct
	payload := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []any{map[string]any{"text": "original-system"}},
		},
	}
	s.injectAnti429(payload)
	si := payload["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("应有 2 个 parts（随机数 + 原文），实际 %d", len(parts))
	}
	p0 := parts[0].(map[string]any)
	if txt, _ := p0["text"].(string); !is100Digits(txt) {
		t.Errorf("首 part 应为随机数，实际 %q", txt)
	}
	p1 := parts[1].(map[string]any)
	if p1["text"] != "original-system" {
		t.Errorf("原 part 应保留在第二位，实际 %v", p1["text"])
	}
}

// ---- injectAnti429：target=user ----

func TestInjectAnti429User(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: true, Anti429Target: "user"}}
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}},
		},
	}
	s.injectAnti429(payload)

	contents := payload["contents"].([]any)
	c0 := contents[0].(map[string]any)
	parts := c0["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("user content 应有 2 个 parts（随机数前插），实际 %d", len(parts))
	}
	p0 := parts[0].(map[string]any)
	if txt, _ := p0["text"].(string); !is100Digits(txt) {
		t.Errorf("首 part 应为 100 位数字串，实际 %q", txt)
	}
	p1 := parts[1].(map[string]any)
	if p1["text"] != "hi" {
		t.Errorf("原 part 应保留在第二位，实际 %v", p1["text"])
	}
	// 不应误注入 systemInstruction。
	if _, ok := payload["systemInstruction"]; ok {
		t.Errorf("target=user 不应注入 systemInstruction")
	}
}

// target=user 且无 user content → 新建一条 user content 置于首位。
func TestInjectAnti429UserNoExisting(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: true, Anti429Target: "user"}}
	payload := map[string]any{
		"contents": []any{
			map[string]any{"role": "model", "parts": []any{map[string]any{"text": "prev"}}},
		},
	}
	s.injectAnti429(payload)
	contents := payload["contents"].([]any)
	if len(contents) != 2 {
		t.Fatalf("应新增一条 user content，实际 %d 条", len(contents))
	}
	c0 := contents[0].(map[string]any)
	if c0["role"] != "user" {
		t.Errorf("新建的首条 content role 应为 user，实际 %v", c0["role"])
	}
	if got := firstPartText(t, contents); !is100Digits(got) {
		t.Errorf("新建 user content 的首 part 应为随机数，实际 %q", got)
	}
}

// Anti429Enabled=false 时不注入。
func TestInjectAnti429Disabled(t *testing.T) {
	s := &Server{cfg: config.AppConfig{Anti429Enabled: false, Anti429Target: "system"}}
	payload := map[string]any{"contents": []any{}}
	s.injectAnti429(payload)
	if _, ok := payload["systemInstruction"]; ok {
		t.Errorf("禁用时不应注入 systemInstruction")
	}
}

// DropMaxTokens=true 时移除 generationConfig.maxOutputTokens。
func TestInjectAnti429DropMaxTokens(t *testing.T) {
	s := &Server{cfg: config.AppConfig{DropMaxTokens: true, Anti429Enabled: false}}
	payload := map[string]any{
		"generationConfig": map[string]any{"maxOutputTokens": 100, "temperature": 0.5},
	}
	s.injectAnti429(payload)
	gc := payload["generationConfig"].(map[string]any)
	if _, ok := gc["maxOutputTokens"]; ok {
		t.Errorf("DropMaxTokens 应移除 maxOutputTokens")
	}
	if _, ok := gc["temperature"]; !ok {
		t.Errorf("应保留其它 generationConfig 字段")
	}
}
