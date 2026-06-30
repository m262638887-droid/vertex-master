// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// 本文件实现 OpenAI TTS 端点 /v1/audio/speech 及其全部辅助：voice 映射、PCM 采样率解析、
// WAV 头包裹、Gemini TTS payload 构建。
//
// 上游机制：responseModalities=["AUDIO"] + speechConfig.voiceConfig；Gemini 把整段音频切成
// 多段 L16 PCM inlineData（由 vertex.CompleteChatAudio 拼接，血泪教训：只取首段会被截断）。
// 纯 Go 无法转 mp3/opus → 统一退化为 WAV，Content-Type 据实标 audio/wav。

// ttsDefaultModel：OpenAI 请求给 gpt-4o-mini-tts/tts-1 之类时统一回退到它。
const ttsDefaultModel = "gemini-3.1-flash-tts-preview"

const ttsDefaultVoice = "Kore"

// ttsVoiceMap：OpenAI voice → Gemini prebuilt voiceName。
//
//nolint:gochecknoglobals // Read-only map
var ttsVoiceMap = map[string]string{
	"alloy": "Kore", "echo": "Puck", "fable": "Charon", "onyx": "Fenrir",
	"nova": "Aoede", "shimmer": "Leda", "ash": "Orus", "ballad": "Zephyr",
	"coral": "Aoede", "sage": "Charon", "verse": "Puck",
}

// ttsGeminiVoices：Gemini 原生 voiceName 集合（命中直接透传，不走映射表）。
//
//nolint:gochecknoglobals // Read-only set
var ttsGeminiVoices = map[string]bool{
	"Kore": true, "Puck": true, "Charon": true, "Aoede": true, "Fenrir": true, "Leda": true,
	"Orus": true, "Zephyr": true, "Autonoe": true, "Enceladus": true, "Iapetus": true,
	"Umbriel": true, "Algieba": true, "Despina": true, "Erinome": true, "Algenib": true,
	"Rasalgethi": true, "Laomedeia": true, "Achernar": true, "Alnilam": true, "Schedar": true,
	"Gacrux": true, "Pulcherrima": true, "Achird": true, "Zubenelgenubi": true,
	"Vindemiatrix": true, "Sadachbia": true, "Sadaltager": true, "Sulafat": true,
}

// ttsFormatInfo：response_format → (Content-Type, 是否需 WAV 头包裹)。
// 纯 Go 无法转 mp3/opus/aac/flac → 统一退化为 WAV，故 Content-Type 也据实标 audio/wav。
type ttsFormat struct {
	contentType string
	wrapWAV     bool
}

//nolint:gochecknoglobals // Read-only configuration map
var ttsFormatInfo = map[string]ttsFormat{
	"mp3":  {"audio/wav", true},
	"wav":  {"audio/wav", true},
	"pcm":  {"audio/L16", false},
	"opus": {"audio/wav", true},
	"aac":  {"audio/wav", true},
	"flac": {"audio/wav", true},
}

// handleAudioSpeech 处理 POST /v1/audio/speech（JSON in，audio bytes out）。
func (s *Server) handleAudioSpeech(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.oaiError(w, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error")
		return
	}

	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "请求体必须是合法JSON (invalid JSON)", "type": "invalid_request_error", "code": 400}})
		return
	}

	// 模型：剥假流式前缀；非 gemini 开头（含空）回退默认 TTS 模型。
	actualModel, _ := stripFakePrefix(getStr(body, "model", ""))
	if actualModel == "" || !strings.HasPrefix(actualModel, "gemini") {
		actualModel = ttsDefaultModel
	}

	text, _ := body["input"].(string)
	if strings.TrimSpace(text) == "" {
		s.writeJSON(w, http.StatusBadRequest, map[string]any{"error": map[string]any{
			"message": "缺少 input 字段 (missing input text)", "type": "invalid_request_error", "code": 400}})
		return
	}

	voice := ttsResolveVoice(body["voice"])
	respFmt := strings.ToLower(firstNonEmptyStr(getStr(body, "response_format", ""), "mp3"))

	log.Printf("[Server] [AudioSpeech] 收到请求: 模型=%s, 语音=%s, 格式=%s", actualModel, voice, respFmt)

	fmtInfo, ok := ttsFormatInfo[respFmt]
	if !ok {
		fmtInfo = ttsFormat{"audio/wav", true}
	}

	geminiPayload := ttsBuildGeminiPayload(text, voice, body["speed"])
	s.injectAnti429(geminiPayload)

	audio, vErr := s.vc.CompleteChatAudio(r.Context(), actualModel, geminiPayload)
	if vErr != nil {
		ve := toVertexError(vErr)
		s.writeJSON(w, ve.Code, vertexErrorToOAI(ve))
		return
	}

	if audio.Data == "" {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "上游未返回音频数据 (no audio returned)", "type": "server_error", "code": 502}})
		return
	}
	raw, err := base64.StdEncoding.DecodeString(audio.Data)
	if err != nil {
		s.writeJSON(w, http.StatusBadGateway, map[string]any{"error": map[string]any{
			"message": "音频解码失败 (audio decode failed)", "type": "server_error", "code": 502}})
		return
	}

	out := raw
	if fmtInfo.wrapWAV {
		sampleRate := ttsParsePCMRate(audio.MimeType)
		out = append(ttsWAVHeader(len(raw), sampleRate), raw...)
	}

	w.Header().Set("Content-Type", fmtInfo.contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

// ttsResolveVoice 把 OpenAI voice 名映射到 Gemini prebuilt voiceName。
func ttsResolveVoice(voice any) string {
	v, ok := voice.(string)
	if !ok || strings.TrimSpace(v) == "" {
		return ttsDefaultVoice
	}
	v = strings.TrimSpace(v)
	if ttsGeminiVoices[v] {
		return v
	}
	if mapped, ok2 := ttsVoiceMap[strings.ToLower(v)]; ok2 { //nolint:govet
		return mapped
	}
	return ttsDefaultVoice
}

// ttsParsePCMRate 从 inlineData mimeType 解析采样率（如 "audio/L16;rate=24000"，默认 24000）。
func ttsParsePCMRate(mimeType string) int {
	const def = 24000
	if mimeType == "" {
		return def
	}
	for _, token := range strings.Split(mimeType, ";") {
		token = strings.ToLower(strings.TrimSpace(token))
		if strings.HasPrefix(token, "rate=") {
			if n, err := strconv.Atoi(token[5:]); err == nil {
				return n
			}
		}
	}
	return def
}

// ttsWAVHeader 构造 44 字节 RIFF/WAVE 头，包裹裸 PCM(L16, 16-bit LE 单声道)。
func ttsWAVHeader(dataLen, sampleRate int) []byte {
	const bits, channels = 16, 1
	byteRate := sampleRate * channels * bits / 8
	blockAlign := channels * bits / 8

	h := make([]byte, 0, 44)
	h = append(h, "RIFF"...)
	h = appendU32LE(h, uint32(36+dataLen))
	h = append(h, "WAVE"...)
	h = append(h, "fmt "...)
	h = appendU32LE(h, 16)               // fmt chunk size
	h = appendU16LE(h, 1)                // PCM
	h = appendU16LE(h, uint16(channels)) // channels
	h = appendU32LE(h, uint32(sampleRate))
	h = appendU32LE(h, uint32(byteRate))
	h = appendU16LE(h, uint16(blockAlign))
	h = appendU16LE(h, uint16(bits))
	h = append(h, "data"...)
	h = appendU32LE(h, uint32(dataLen))
	return h
}

func appendU32LE(b []byte, v uint32) []byte {
	var tmp [4]byte
	binary.LittleEndian.PutUint32(tmp[:], v)
	return append(b, tmp[:]...)
}

func appendU16LE(b []byte, v uint16) []byte {
	var tmp [2]byte
	binary.LittleEndian.PutUint16(tmp[:], v)
	return append(b, tmp[:]...)
}

// ttsBuildGeminiPayload 把 OpenAI speech 请求转成 Gemini generateContent payload。
// responseModalities=["AUDIO"] + speechConfig；
// speed≠1.0 时匿名 Vertex 不保证支持 speakingRate，改用自然语言风格指令最稳。
func ttsBuildGeminiPayload(text, voice string, speed any) map[string]any {
	prompt := text
	spd := coerceSpeed(speed)
	if spd != 0 && abs(spd-1.0) > 1e-6 {
		pace := "faster"
		if spd < 1.0 {
			pace = "more slowly"
		}
		prompt = "Say the following " + pace + ": " + text
	}
	return map[string]any{
		"contents": []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": prompt}}}},
		"generationConfig": map[string]any{
			"responseModalities": []any{"AUDIO"},
			"speechConfig": map[string]any{
				"voiceConfig": map[string]any{
					"prebuiltVoiceConfig": map[string]any{"voiceName": voice},
				},
			},
		},
	}
}

// coerceSpeed 把 speed 转 float（nil/非法 → 1.0）。
func coerceSpeed(speed any) float64 {
	switch v := speed.(type) {
	case nil:
		return 1.0
	case float64:
		return v
	case int:
		return float64(v)
	case string:
		if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
			return f
		}
	}
	return 1.0
}

func abs(f float64) float64 {
	if f < 0 {
		return -f
	}
	return f
}
