// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"math/rand"
)

// injectAnti429 预处理 Gemini payload：drop_max_tokens 移除 maxOutputTokens + anti429 随机数注入。
// 原地修改 payload。
//
// 用于 Gemini 原生端点与 TTS（它们直接透传/构建 gemini_payload，未经 ConvertChatRequest 的
// drop_max_tokens 处理）。OpenAI chat 端点的 drop_max_tokens 已在 ConvertChatRequest 内处理，
// 但 anti429 注入对所有路径一致——为保持各路径行为一致，这里独立实现。
func (s *Server) injectAnti429(payload map[string]any) {
	if payload == nil {
		return
	}
	cfg := s.cfg

	// drop_max_tokens：移除 generationConfig.maxOutputTokens。
	if cfg.DropMaxTokens {
		if gc, ok := payload["generationConfig"].(map[string]any); ok {
			delete(gc, "maxOutputTokens")
		}
	}

	if !cfg.Anti429Enabled {
		return
	}

	randStr := randomDigits(100)
	target := cfg.Anti429Target
	if target == "" {
		target = "system"
	}

	if target == "system" {
		si, _ := payload["systemInstruction"].(map[string]any)
		if si == nil {
			si = map[string]any{}
		}
		parts, _ := si["parts"].([]any)
		parts = append([]any{map[string]any{"text": randStr}}, parts...)
		si["parts"] = parts
		payload["systemInstruction"] = si
		return
	}

	// target=user：在首条 user content 前插入随机数 part；无 user content 则新建。
	contents, _ := payload["contents"].([]any)
	inserted := false
	for _, cRaw := range contents {
		c, ok := cRaw.(map[string]any)
		if !ok {
			continue
		}
		if c["role"] == "user" {
			parts, _ := c["parts"].([]any)
			c["parts"] = append([]any{map[string]any{"text": randStr}}, parts...)
			inserted = true
			break
		}
	}
	if !inserted {
		newUser := map[string]any{"role": "user", "parts": []any{map[string]any{"text": randStr}}}
		payload["contents"] = append([]any{newUser}, contents...)
	}
}

// randomDigits 生成 n 位随机数字串。
func randomDigits(n int) string {
	const digits = "0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = digits[rand.Intn(len(digits))]
	}
	return string(b)
}
