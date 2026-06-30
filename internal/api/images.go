// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log"
	"mime"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/transform"
)

// 本文件实现 OpenAI Images 三端点（文生图 / 图片编辑 / 图片变体）+ multipart 上传辅助
// + 共享图片请求执行器。
//
// 生图机制：构建 Gemini payload（生成走纯 prompt+imageConfig，编辑/变体走
// transform.BuildImagePayload 拼自然语言约束 + inlineData 输入图）→ vc.CompleteChatImage
// （标准非流式 + 抽图）→ 拼成 OpenAI Images 响应 {created, data:[{b64_json}|{url}]}。

// handleImageGenerations 处理 POST /v1/images/generations（JSON，文生图）。
func (s *Server) handleImageGenerations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	body, ok := s.readJSONObject(w, r)
	if !ok {
		return
	}

	model := getStr(body, "model", "")
	prompt := getStr(body, "prompt", "")
	size := getStr(body, "size", "1024x1024")
	respFmt := getStr(body, "response_format", "b64_json")

	log.Printf("[Server] [ImageGenerations] 收到请求: 模型=%s, 尺寸=%s, 格式=%s", model, size, respFmt)

	if model == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "缺少model字段", "type": "invalid_request_error", "code": nil}})
		return
	}
	if prompt == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "缺少 prompt 字段 (missing prompt)", "type": "invalid_request_error", "code": 400}})
		return
	}

	geminiPayload := map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}},
	}
	// 优先用 imageConfig.imageSize 档位控制（512/1K/2K/4K，默认 1K）；
	// 未命中则保留旧 flat imageSize=WxH 兼容老客户端。
	transform.ApplyImageConfig(geminiPayload, body)
	if !hasImageSize(geminiPayload) {
		gc, _ := geminiPayload["generationConfig"].(map[string]any)
		if gc == nil {
			gc = map[string]any{}
			geminiPayload["generationConfig"] = gc
		}
		ic, _ := gc["imageConfig"].(map[string]any)
		if ic == nil {
			ic = map[string]any{}
			gc["imageConfig"] = ic
		}
		ic["imageSize"] = "1K"
		if size != "" {
			gc["imageSize"] = size
		}
	}

	images, vErr := s.vc.CompleteChatImage(r.Context(), model, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		s.writeJSON(w, ve.Code, vertexErrorToOAI(ve))
		return
	}

	data := make([]any, 0, len(images))
	for _, img := range images {
		if img.B64JSON == "" {
			continue
		}
		// 生图端点 url 格式固定 image/png（`data:image/png;base64,`）。
		if respFmt == "url" {
			data = append(data, map[string]any{"url": "data:image/png;base64," + img.B64JSON})
		} else {
			data = append(data, map[string]any{"b64_json": img.B64JSON})
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": data})
}

// handleImageEdits 处理 POST /v1/images/edits（multipart，图片编辑 / image-to-image）。
func (s *Server) handleImageEdits(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(multipartMemoryLimit); err != nil {
		s.oaiBadRequest(w, "图片编辑请求解析失败，请检查 multipart 表单 (failed to parse edit request)")
		return
	}

	imageUploads := formUploads(r, "image")
	if len(imageUploads) == 0 {
		s.oaiBadRequest(w, "缺少 image 字段 (image is required)")
		return
	}
	images, err := uploadsToInlineImages(imageUploads)
	if err != nil {
		s.oaiBadRequest(w, err.Error())
		return
	}
	var mask *transform.InlineImage
	if maskUploads := formUploads(r, "mask"); len(maskUploads) > 0 {
		m, err := uploadToInlineImage(maskUploads[0]) //nolint:govet
		if err != nil {
			s.oaiBadRequest(w, err.Error())
			return
		}
		mask = &m
	}

	model := transform.ResolveImageModel(formValue(r, "model"))
	prompt := firstNonEmptyStr(formValue(r, "prompt"), "Edit the provided image.")
	prompt = transform.AppendNegativePrompt(prompt, formValue(r, "negative_prompt"))
	n := coerceOAIN(formValue(r, "n"))
	respFmt := firstNonEmptyStr(formValue(r, "response_format"), "b64_json")

	log.Printf("[Server] [ImageEdits] 收到请求: 模型=%s, 格式=%s, 图片数=%d", model, respFmt, len(images))

	geminiPayload := transform.BuildImagePayload(model, prompt, images, mask,
		formValue(r, "size"), formValue(r, "quality"), formValue(r, "style"),
		formValue(r, "background"), "edit")

	s.runOAIImageRequest(r.Context(), w, model, geminiPayload, n, respFmt)
}

// handleImageVariations 处理 POST /v1/images/variations（multipart，图片变体）。
func (s *Server) handleImageVariations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}
	if err := r.ParseMultipartForm(multipartMemoryLimit); err != nil {
		s.oaiBadRequest(w, "图片变体请求解析失败，请检查 multipart 表单 (failed to parse variation request)")
		return
	}

	imageUploads := formUploads(r, "image")
	if len(imageUploads) == 0 {
		s.oaiBadRequest(w, "缺少 image 字段 (image is required)")
		return
	}
	images, err := uploadsToInlineImages(imageUploads)
	if err != nil {
		s.oaiBadRequest(w, err.Error())
		return
	}

	model := transform.ResolveImageModel(formValue(r, "model"))
	prompt := firstNonEmptyStr(formValue(r, "prompt"), "Create a variation of the provided image.")
	prompt = transform.AppendNegativePrompt(prompt, formValue(r, "negative_prompt"))
	n := coerceOAIN(formValue(r, "n"))
	respFmt := firstNonEmptyStr(formValue(r, "response_format"), "b64_json")

	log.Printf("[Server] [ImageVariations] 收到请求: 模型=%s, 格式=%s, 图片数=%d", model, respFmt, len(images))

	// 变体无 mask、无 background。
	geminiPayload := transform.BuildImagePayload(model, prompt, images, nil,
		formValue(r, "size"), formValue(r, "quality"), formValue(r, "style"), "", "variation")

	s.runOAIImageRequest(r.Context(), w, model, geminiPayload, n, respFmt)
}

// runOAIImageRequest 是 edits/variations 共享的执行器。
// 串行调用 n 次并聚合（不并发，避免放大上游 429 压力），聚满 n 张即停。
func (s *Server) runOAIImageRequest(ctx context.Context, w http.ResponseWriter, model string, geminiPayload map[string]any, n int, responseFormat string) {
	wantURL := responseFormat == "url"
	items := make([]any, 0, n)
	for i := 0; i < n; i++ {
		log.Printf("[Server] [runOAIImageRequest] 开始获取图片 (第 %d/%d 张)", i+1, n)
		images, vErr := s.vc.CompleteChatImage(ctx, model, geminiPayload)
		if vErr != nil {
			ve := toVertexError(vErr)
			s.writeJSON(w, ve.Code, vertexErrorToOAI(ve))
			return
		}
		for _, img := range images {
			if img.B64JSON == "" {
				continue
			}
			if wantURL {
				mimeType := img.MimeType
				if mimeType == "" {
					mimeType = "image/png"
				}
				items = append(items, map[string]any{"url": "data:" + mimeType + ";base64," + img.B64JSON})
			} else {
				items = append(items, map[string]any{"b64_json": img.B64JSON})
			}
		}
		if len(items) >= n {
			break
		}
	}

	if len(items) == 0 {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "上游未返回图片数据 (no image returned)", "type": "server_error", "code": 502}})
		return
	}
	if len(items) > n {
		items = items[:n]
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"created": time.Now().Unix(), "data": items})
}

// ---- multipart / 通用辅助 ----

// multipartMemoryLimit 是 multipart 表单驻留内存的阈值；超过部分由 net/http 自动 spool 到磁盘临时文件
// （Go 内建的 SpooledTemporaryFile 等价物），使大图上传不全量进 RAM。8MB 留足常规图片在内存。
const multipartMemoryLimit = 8 << 20

// formValue 取 multipart 表单非文件字段的首个值（不存在返回 ""）。
func formValue(r *http.Request, key string) string {
	if r.MultipartForm == nil {
		return ""
	}
	if vs := r.MultipartForm.Value[key]; len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// formUploads 取出表单里 key / key[] / key[i] 的所有上传文件。
func formUploads(r *http.Request, key string) []*multipart.FileHeader {
	if r.MultipartForm == nil {
		return nil
	}
	var out []*multipart.FileHeader
	prefix := key + "["
	for k, fhs := range r.MultipartForm.File {
		if k == key || k == key+"[]" || strings.HasPrefix(k, prefix) {
			out = append(out, fhs...)
		}
	}
	return out
}

// uploadToInlineImage 把一个上传文件读成 Gemini inlineData（base64 + mime）。
//
// 使用流式 base64 编码避免全量原始字节常驻内存：从上传文件读取时直接经 base64 编码器写出，
// 编码后的 string 是最终内存中的唯一完整副本。
func uploadToInlineImage(fh *multipart.FileHeader) (transform.InlineImage, error) {
	f, err := fh.Open()
	if err != nil {
		return transform.InlineImage{}, &badRequestError{msg: "无法读取上传文件 (cannot read upload)"}
	}
	defer func() { _ = f.Close() }()

	var buf strings.Builder
	enc := base64.NewEncoder(base64.StdEncoding, &buf)
	written, err := io.Copy(enc, f)
	if err != nil {
		return transform.InlineImage{}, &badRequestError{msg: "无法读取上传文件 (cannot read upload)"}
	}
	_ = enc.Close()
	if written == 0 {
		name := fh.Filename
		if name == "" {
			name = "image"
		}
		return transform.InlineImage{}, &badRequestError{msg: name + " 内容为空 (empty file)"}
	}
	mimeType := fh.Header.Get("Content-Type")
	if mimeType == "" {
		if ext := filepath.Ext(fh.Filename); ext != "" {
			mimeType = mime.TypeByExtension(ext)
		}
	}
	if mimeType == "" {
		mimeType = "image/png"
	}
	return transform.InlineImage{MimeType: mimeType, Data: buf.String()}, nil
}

// uploadsToInlineImages 批量转换；任一文件为空/不可读则返回该错误。
func uploadsToInlineImages(fhs []*multipart.FileHeader) ([]transform.InlineImage, error) {
	out := make([]transform.InlineImage, 0, len(fhs))
	for _, fh := range fhs {
		img, err := uploadToInlineImage(fh)
		if err != nil {
			return nil, err
		}
		out = append(out, img)
	}
	return out, nil
}

// badRequestError 携带可直接回给客户端的 400 文案。
type badRequestError struct{ msg string }

func (e *badRequestError) Error() string { return e.msg }

// coerceOAIN 把 OpenAI Images 的 n 参数 clamp 到 [1,8]，非法值回退 1。
func coerceOAIN(value string) int {
	n, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 1
	}
	if n < 1 {
		return 1
	}
	if n > 8 {
		return 8
	}
	return n
}

// oaiBadRequest 发一条 400 invalid_request_error。
func (s *Server) oaiBadRequest(w http.ResponseWriter, message string) {
	s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
		"message": message, "type": "invalid_request_error", "code": 400}})
}

// readJSONObject 读请求体并解析为 JSON 对象；失败时写出 400 并返回 ok=false。
func (s *Server) readJSONObject(w http.ResponseWriter, r *http.Request) (map[string]any, bool) {
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "请求体必须是合法JSON", "type": "invalid_request_error", "code": nil}})
		return nil, false
	}
	return body, true
}

// getStr 取 body 里字符串字段，缺失或非字符串返回 def。
// 注意：键存在但值为非字符串时返回 def，键存在且为字符串（含空串）时原样返回。
func getStr(body map[string]any, key, def string) string {
	v, ok := body[key]
	if !ok {
		return def
	}
	s, ok := v.(string)
	if !ok {
		return def
	}
	return s
}

// firstNonEmptyStr 返回第一个非空字符串。
func firstNonEmptyStr(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return a
	}
	return b
}

// hasImageSize 判断 payload 是否已设 generationConfig.imageConfig.imageSize。
func hasImageSize(payload map[string]any) bool {
	gc, ok := payload["generationConfig"].(map[string]any)
	if !ok {
		return false
	}
	ic, ok := gc["imageConfig"].(map[string]any)
	if !ok {
		return false
	}
	v, ok := ic["imageSize"]
	if !ok {
		return false
	}
	s, _ := v.(string)
	return s != ""
}
