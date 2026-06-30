// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package transform

import "testing"

// TestNormalizePartMultimodal 验证 normalizePart 对 OpenAI 风格多模态 part 的归一（Fix 7）：
// image_url(data:/http)/input_image、media/file/file_data、inline_data → 对应 Gemini part。
func TestNormalizePartMultimodal(t *testing.T) {
	// image_url + data: URI → inlineData
	got := normalizePart(map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,QQ=="},
	})
	id, _ := got["inlineData"].(map[string]any)
	if id == nil || id["mimeType"] != "image/png" || id["data"] != "QQ==" {
		t.Fatalf("image_url data: 应转 inlineData，got %v", got)
	}

	// image_url + http URL → fileData，mime 按扩展名猜
	got = normalizePart(map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "https://x.com/a.mp4"},
	})
	fd, _ := got["fileData"].(map[string]any)
	if fd == nil || fd["fileUri"] != "https://x.com/a.mp4" || fd["mimeType"] != "video/mp4" {
		t.Fatalf("image_url http 应转 fileData(video/mp4)，got %v", got)
	}

	// media/file → fileData，显式 mimeType 优先
	got = normalizePart(map[string]any{
		"type": "file", "file_uri": "gs://b/x.pdf", "mime_type": "application/pdf",
	})
	fd, _ = got["fileData"].(map[string]any)
	if fd == nil || fd["fileUri"] != "gs://b/x.pdf" || fd["mimeType"] != "application/pdf" {
		t.Fatalf("file 应转 fileData，got %v", got)
	}

	// inline_data → inlineData
	got = normalizePart(map[string]any{
		"type": "inline_data", "inline_data": map[string]any{"mime_type": "audio/wav", "data": "ZGF0YQ=="},
	})
	id, _ = got["inlineData"].(map[string]any)
	if id == nil || id["mimeType"] != "audio/wav" || id["data"] != "ZGF0YQ==" {
		t.Fatalf("inline_data 应转 inlineData，got %v", got)
	}

	// text → {text}
	got = normalizePart(map[string]any{"type": "text", "text": "hi"})
	if got["text"] != "hi" {
		t.Fatalf("text part，got %v", got)
	}

	// 未知类型 → 键 camelCase 透传
	got = normalizePart(map[string]any{"type": "weird", "some_key": "v"})
	if got["someKey"] != "v" {
		t.Fatalf("未知 part 应 camelCase 透传，got %v", got)
	}
}

// TestGuessMIMEFromURI 验证多类型 mime 猜测覆盖图/视频/音频/pdf/txt。
func TestGuessMIMEFromURI(t *testing.T) {
	cases := map[string]string{
		"a.jpg": "image/jpeg", "a.png": "image/png", "a.webp": "image/webp", "a.gif": "image/gif",
		"a.mp4": "video/mp4", "a.mov": "video/quicktime", "a.webm": "video/webm",
		"a.mp3": "audio/mpeg", "a.wav": "audio/wav", "a.ogg": "audio/ogg",
		"a.pdf": "application/pdf", "a.txt": "text/plain", "a.xyz": "image/png",
		"http://x/a.MP4?t=1": "video/mp4", // 大写 + query 串
	}
	for in, want := range cases {
		if got := guessMIMEFromURI(in); got != want {
			t.Errorf("guessMIMEFromURI(%q)=%q，want %q", in, got, want)
		}
	}
}
