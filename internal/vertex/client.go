// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package vertex

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/metrics"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/recaptcha"
	"github.com/bsfdsagfadg/vertex/internal/spool"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

const (
	anonBaseURL      = "https://cloudconsole-pa.clients6.google.com"
	batchGraphqlPath = "/v3/entityServices/AiplatformEntityService/schemas/AIPLATFORM_GRAPHQL:batchGraphql"
	anonAPIKey       = "AIzaSyCI-zsRP85UVOi0DjtiCwWBwQ1djDy741g"
)

var batchGraphqlURL = anonBaseURL + batchGraphqlPath + "?key=" + anonAPIKey + "&prettyPrint=false" //nolint:gochecknoglobals

var defaultSafetySettings = []any{ //nolint:gochecknoglobals
	map[string]any{"category": "HARM_CATEGORY_HARASSMENT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_HATE_SPEECH", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_SEXUALLY_EXPLICIT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_DANGEROUS_CONTENT", "threshold": "BLOCK_NONE"},
	map[string]any{"category": "HARM_CATEGORY_CIVIC_INTEGRITY", "threshold": "BLOCK_NONE"},
}

// RequestIDKey 是 context 中存储 reqID 的键类型。
type RequestIDKey struct{}

// RequestIDFromContext 取请求上下文里的 request-id（无则空串）。
func RequestIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(RequestIDKey{}).(string); ok {
		return v
	}
	return ""
}

type VertexAIClient struct {
	net  *transport.NetworkClient
	pool *recaptcha.TokenPool
}

func NewVertexAIClient() *VertexAIClient {
	net := transport.NewNetworkClient()
	return &VertexAIClient{
		net:  net,
		pool: recaptcha.NewTokenPoolSize(net, config.Load().TokenPoolSize),
	}
}

func (c *VertexAIClient) StartTokenPool()                  { c.pool.Start() }
func (c *VertexAIClient) StopTokenPool()                   { c.pool.Stop() }
func (c *VertexAIClient) TokenPoolStats() (size, fill int) { return c.pool.Stats() }

func (c *VertexAIClient) CompleteChat(ctx context.Context, model string, geminiPayload map[string]any) (map[string]any, error) {
	return RunParallel(ctx, config.Load(), func(ctx context.Context, proxyURI string) (map[string]any, error) {
		return c.completeInner(ctx, model, geminiPayload, proxyURI)
	})
}

const largePayloadThreshold = 1 << 20 // 1MB

func (c *VertexAIClient) CompleteChatN(ctx context.Context, model string, geminiPayload map[string]any, n int) ([]map[string]any, error) {
	if n > 1 {
		if b, err := json.Marshal(geminiPayload); err == nil && len(b) > largePayloadThreshold {
			log.Printf("[Vertex] [CompleteChatN] 大 payload (%d bytes) 降级为串行", len(b))
			return c.completeChatNSerial(ctx, model, geminiPayload, n)
		}
	}

	type res struct {
		resp map[string]any
		err  error
	}
	results := make([]res, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			defer func() {
				if rec := recover(); rec != nil {
					results[idx] = res{err: NewInternalError(fmt.Sprintf("candidate panic: %v", rec))} //nolint:exhaustruct
				}
			}()
			r, err := c.CompleteChat(ctx, model, geminiPayload)
			results[idx] = res{resp: r, err: err}
		}(i)
	}
	wg.Wait()

	var ok []map[string]any
	var firstErr error
	for _, r := range results {
		if r.err != nil {
			if firstErr == nil {
				firstErr = r.err
			}
			continue
		}
		ok = append(ok, r.resp)
	}
	if len(ok) == 0 {
		if firstErr == nil {
			firstErr = NewInternalError("All candidates failed")
		}
		return nil, firstErr
	}
	return ok, nil
}

func (c *VertexAIClient) completeChatNSerial(ctx context.Context, model string, geminiPayload map[string]any, n int) ([]map[string]any, error) {
	var ok []map[string]any
	var firstErr error
	for i := 0; i < n; i++ {
		r, err := c.CompleteChat(ctx, model, geminiPayload)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		ok = append(ok, r)
	}
	if len(ok) == 0 {
		if firstErr == nil {
			firstErr = NewInternalError("All candidates failed")
		}
		return nil, firstErr
	}
	return ok, nil
}

func (c *VertexAIClient) completeInner(ctx context.Context, model string, geminiPayload map[string]any, proxyURI string) (map[string]any, error) {
	cfg := config.Load()
	maxRetries := cfg.MaxRetries
	if cfg.ParallelPoolEnabled && !cfg.ParallelPoolRetryEnabled && ctx.Value(stickyModeKey{}) == nil {
		maxRetries = 0
	}
	recaptchaToken := ""
	isFirstAuth := true
	attempt := 0

	reqID := RequestIDFromContext(ctx)
	sess, err := c.net.CreateSession(180, proxyURI, reqID)
	if err != nil {
		// 节点初始化失败，属于严重的内部网络错误，直接退出熔断
		return nil, NewInternalError("create session: " + err.Error())
	}
	defer sess.Close()

	for attempt <= maxRetries {
		log.Printf("[Vertex] [CompleteChat] 开始尝试 (Attempt %d/%d), 模型=%s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, reqID, nodes.GetNodeName(sess.ProxyURI))
		if recaptchaToken == "" {
			tok, _ := c.pool.GetTokenWithProxy(proxyURI)
			recaptchaToken = tok
			isFirstAuth = true
		}
		if recaptchaToken == "" {
			if attempt == maxRetries {
				return nil, NewAuthenticationError("Could not fetch recaptcha token.")
			}
			attempt++
			if err := sleepCtx(ctx, time.Second); err != nil {
				return nil, ctxCanceledError(err)
			}
			continue
		}

		result, reqErr := c.executeCompleteRequest(ctx, sess, model, geminiPayload, recaptchaToken, isFirstAuth)
		if reqErr == nil {
			if _, hasSafety := geminiPayload["safetySettings"]; candidateFinish(result) == "SAFETY" && !hasSafety {
				retryPayload := shallowCopy(geminiPayload)
				retryPayload["safetySettings"] = defaultSafetySettings
				result, reqErr = c.executeCompleteRequest(ctx, sess, model, retryPayload, recaptchaToken, false)
			}
		}
		if reqErr == nil {
			return result, nil
		}

		ve := asVertexError(reqErr)
		switch {
		case ve != nil && ve.Kind == "auth":
			isVerifyFail := strings.Contains(ve.Message, "Failed to verify action") ||
				strings.Contains(ve.Message, "The caller does not have permission")
			if isFirstAuth && isVerifyFail {
				isFirstAuth = false
				if err := sleepCtx(ctx, 500*time.Millisecond); err != nil {
					return nil, ctxCanceledError(err)
				}
				continue
			}
			metrics.Default.IncUpstreamAuth()
			recaptchaToken = ""
			isFirstAuth = true
			if attempt < maxRetries {
				attempt++
				if err := sleepCtx(ctx, time.Second); err != nil {
					return nil, ctxCanceledError(err)
				}
				continue
			}
			return nil, ve

		case ve != nil && ve.Kind == "ratelimit":
			metrics.Default.IncUpstream429()
			if attempt >= maxRetries {
				log.Printf("[Vertex] [CompleteChat] (Attempt %d/%d) 节点 %s 触发 429 失败, 请求ID=%s, 代理=%s", attempt, maxRetries, model, reqID, nodes.GetNodeName(sess.ProxyURI))
				return nil, ve
			}
			sess.Close()
			newSess, e := c.net.CreateSession(180, proxyURI, reqID)
			if e != nil {
				return nil, NewInternalError("recreate session: " + e.Error())
			}
			sess = newSess
			recaptchaToken = ""
			wait := ve.RetryAfter
			if wait <= 0 {
				wait = min(10, 1+attempt)
			}
			log.Printf("[Vertex] [CompleteChat] (Attempt %d/%d) 节点 %s 触发 429 将重试 (延迟 %ds), 请求ID=%s, 代理=%s", attempt, maxRetries, model, wait, reqID, nodes.GetNodeName(sess.ProxyURI))
			attempt++
			if err := sleepCtx(ctx, time.Duration(wait)*time.Second); err != nil {
				return nil, ctxCanceledError(err)
			}
			continue

		case ve != nil:
			if ve.Kind == "empty" {
				metrics.Default.IncUpstreamEmpty()
			}
			// 【关键改动】：如果是网络连接断开等内部错误，直接熔断不重试
			if ve.Kind == "internal" || !ve.IsRetryable() || attempt >= maxRetries {
				log.Printf("[Vertex] [CompleteChat] (Attempt %d/%d) 节点 %s 触发异常错误失败: [%s] %s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, ve.Kind, ve.Message, reqID, nodes.GetNodeName(sess.ProxyURI))
				return nil, ve
			}
			log.Printf("[Vertex] [CompleteChat] (Attempt %d/%d) 节点 %s 触发异常错误将重试: [%s] %s, 请求ID=%s, 代理=%s", attempt, maxRetries, model, ve.Kind, ve.Message, reqID, nodes.GetNodeName(sess.ProxyURI))
			attempt++
			if err := sleepCtx(ctx, backoff(attempt)); err != nil {
				return nil, ctxCanceledError(err)
			}
			continue

		default:
			// 【关键改动】：其他未预期异常，不进行重试，立刻退出
			return nil, NewInternalError("Internal error: " + reqErr.Error())
		}
	}
	return nil, NewInternalError("All retries exhausted")
}

func (c *VertexAIClient) executeCompleteRequest(ctx context.Context, sess *transport.Session, model string, geminiPayload map[string]any, recaptchaToken string, _ bool) (map[string]any, error) {
	reqID := RequestIDFromContext(ctx)
	log.Printf("[Vertex] [executeCompleteRequest] 准备发送请求: 模型=%s, 请求ID=%s, 代理=%s", model, reqID, nodes.GetNodeName(sess.ProxyURI))
	cfg := config.Load()
	newBody := buildRequestPayload(model, geminiPayload, recaptchaToken, cfg)
	buf, err := spool.EncodeJSON(newBody)
	if err != nil {
		return nil, NewInternalError("marshal payload: " + err.Error())
	}
	defer func() { _ = buf.Close() }()
	reader, err := buf.Reader()
	if err != nil {
		return nil, NewInternalError("spool reader: " + err.Error())
	}
	header := transport.XHRHeaders(
		"application/json", "*/*",
		"https://console.cloud.google.com", "https://console.cloud.google.com/", "cross-site",
	)

	status, raw, err := sess.DoAndRead(ctx, "POST", batchGraphqlURL, header, reader)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return nil, fmt.Errorf("error: %w", err)

		}
		return nil, NewInternalError("upstream request: " + err.Error())
	}

	if status != 200 {
		errText := string(raw)
		if cfg.DebugMode {
			debugReq, _ := json.Marshal(newBody)
			log.Printf("[DEBUG] [CompleteChat] HTTP 报错! 状态码: %d", status)
			log.Printf("[DEBUG] [CompleteChat] 完整请求体: %s", string(debugReq))
			log.Printf("[DEBUG] [CompleteChat] 上游回复: %s", errText)
		} else if status == 400 {
			debugBody, _ := json.Marshal(newBody["variables"])
			log.Printf("[Vertex] 收到 400 Bad Request, Variables Payload: %s", string(debugBody))
		}

		if status == 401 || status == 403 ||
			strings.Contains(errText, "Failed to verify action") ||
			strings.Contains(errText, "The caller does not have permission") {
			return nil, NewAuthenticationError("Authentication/Recaptcha failed: " + errText)
		}
		if parsed := parseErrorResponse(errText); parsed != nil {
			parsed.UpstreamResponse = errText
			return nil, parsed
		}
		return nil, raiseForStatus(status, "", "Upstream Error: "+errText, nil, errText)
	}

	if len(raw) == 0 {
		return nil, NewEmptyResponseError("Upstream returned no data")
	}

	result := ParseUpstreamData(string(raw))

	if result.HasError && len(result.Parts) == 0 {
		errMsg := result.ErrorMessage

		if cfg.DebugMode {
			debugReq, _ := json.Marshal(newBody)
			log.Printf("[DEBUG] [CompleteChat] 业务层报错! errorMessage: %s", errMsg)
			log.Printf("[DEBUG] [CompleteChat] 完整请求体: %s", string(debugReq))
			log.Printf("[DEBUG] [CompleteChat] 上游回复: %s", string(raw))
		}

		isAuth := strings.Contains(errMsg, "Failed to verify action") ||
			strings.Contains(errMsg, "The caller does not have permission")
		if isAuth {
			return nil, NewAuthenticationError("Authentication/Recaptcha failed: " + errMsg)
		}
		if result.ErrorObj != nil {
			return nil, result.ErrorObj
		}
		lower := strings.ToLower(errMsg)
		isRate := strings.Contains(lower, "resource has been exhausted") || strings.Contains(lower, "quota")
		switch {
		case strings.Contains(lower, "not found"):
			return nil, NewNotFoundError(errMsg)
		case isRate:
			return nil, NewRateLimitError(errMsg, 0)
		default:
			return nil, NewInvalidArgumentError(errMsg)
		}
	}

	return c.buildCompleteResponse(result)
}

func (c *VertexAIClient) buildCompleteResponse(r *ParseResult) (map[string]any, error) {
	if len(r.Parts) == 0 && !r.HasError && len(r.PromptFeedback) == 0 {
		return nil, NewEmptyResponseError("Upstream returned empty response (no content)")
	}

	allParts := r.Parts
	if len(allParts) == 0 {
		allParts = []map[string]any{{"text": " "}}
	}
	candidate := map[string]any{
		"index":   r.CandidateIndex,
		"content": map[string]any{"parts": toAnySlice(allParts), "role": "model"},
	}
	if r.FinishReason != "" {
		candidate["finishReason"] = strings.ToUpper(r.FinishReason)
	}
	setIfPresent(candidate, "finishMessage", r.FinishMessage)
	setIfPresent(candidate, "safetyRatings", r.SafetyRatings)
	setIfPresent(candidate, "citationMetadata", r.CitationMetadata)
	setIfPresent(candidate, "groundingMetadata", r.GroundingMetadata)
	setIfPresent(candidate, "tokenCount", r.TokenCount)
	setIfPresent(candidate, "avgLogprobs", r.AvgLogprobs)
	setIfPresent(candidate, "logprobsResult", r.LogprobsResult)

	resp := map[string]any{"candidates": []any{candidate}}
	setIfPresent(resp, "createTime", r.CreateTime)
	setIfPresent(resp, "modelVersion", r.ModelVersion)
	if len(r.PromptFeedback) > 0 {
		resp["promptFeedback"] = r.PromptFeedback
	}
	setIfPresent(resp, "responseId", r.ResponseID)
	if len(r.UsageMetadata) > 0 {
		resp["usageMetadata"] = r.UsageMetadata
	}
	setIfPresent(resp, "modelStatus", r.ModelStatus)
	return resp, nil
}

func candidateFinish(result map[string]any) string {
	if cands, ok := result["candidates"].([]any); ok && len(cands) > 0 {
		if c, ok := cands[0].(map[string]any); ok {
			return toStr(c["finishReason"])
		}
	}
	return ""
}

func shallowCopy(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

func asVertexError(err error) *VertexError {
	if ve, ok := err.(*VertexError); ok {
		return ve
	}
	return nil
}

func setIfPresent(m map[string]any, key string, v any) {
	if v == nil {
		return
	}
	switch x := v.(type) {
	case string:
		if x == "" {
			return
		}
	case []any:
		if len(x) == 0 {
			return
		}
	case map[string]any:
		if len(x) == 0 {
			return
		}
	}
	m[key] = v
}

func backoff(attempt int) time.Duration {
	v := math.Pow(1.5, float64(attempt))
	if v > 15 {
		v = 15
	}
	return time.Duration(v * float64(time.Second))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err() //nolint:wrapcheck
	case <-t.C:
		return nil
	}
}

func ctxCanceledError(err error) error {
	return NewInternalError("request canceled: " + err.Error())
}
