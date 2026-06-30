// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package vertex

import (
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transform"
)

// batchGraphql 请求的固定包壳常量（逐字节对齐 PoC body.json / vertex_client）。
const (
	querySignature = "2/l8eCsMMY49imcDQ/lwwXyL8cYtTjxZBF2dNqy69LodY="
	operationName  = "StreamGenerateContentAnonymous"
)

// buildRequestPayload 构建发往上游的完整请求体（对齐 _build_request_payload）：
// 用 transform 构建 variables，再强制注入 region=global 与 recaptchaToken，最后包壳。
func buildRequestPayload(model string, geminiPayload map[string]any, recaptchaToken string, cfg config.AppConfig) map[string]any {
	vars := transform.BuildVertexVariables(model, geminiPayload, cfg)
	vars["region"] = "global"
	vars["recaptchaToken"] = recaptchaToken
	return map[string]any{
		"requestContext": map[string]any{
			"clientVersion": "boq_cloud-boq-clientweb-vertexaistudio_20260402.09_p0",
			"pagePath":      "/vertex-ai/studio/multimodal",
			"jurisdiction":  "global",
			"localizationData": map[string]any{
				"locale":   "zh_CN",
				"timezone": "Asia/Shanghai",
			},
		},
		"querySignature": querySignature,
		"operationName":  operationName,
		"variables":      vars,
	}
}
