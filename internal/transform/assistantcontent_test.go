// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import "testing"

// splitAssistantContent：assistant 文本里的 markdown data-URI 图片必须重解析为 inlineData，
// 否则巨型 base64 markdown 作为文本进 model 角色，多轮改图被上游拒。
func TestSplitAssistantContent_ImageMarkdown(t *testing.T) {
	s := "这是图片：\n\n![image](data:image/png;base64,iVBORw0KGgoAAAANS) 完成"
	parts := splitAssistantContent(s)
	var hasInline, hasText bool
	for _, p := range parts {
		m := p.(map[string]any)
		if id, ok := m["inlineData"].(map[string]any); ok {
			if id["mimeType"] == "image/png" {
				hasInline = true
			}
		}
		if txt, ok := m["text"].(string); ok && txt != "" {
			hasText = true
		}
	}
	if !hasInline {
		t.Errorf("markdown 图片应重解析为 inlineData，got %v", parts)
	}
	if !hasText {
		t.Errorf("图片前后的文本应保留为 text part")
	}
}

func TestSplitAssistantContent_PlainText(t *testing.T) {
	parts := splitAssistantContent("纯文本回复")
	if len(parts) != 1 || parts[0].(map[string]any)["text"] != "纯文本回复" {
		t.Errorf("纯文本应原样为单个 text part，got %v", parts)
	}
}
