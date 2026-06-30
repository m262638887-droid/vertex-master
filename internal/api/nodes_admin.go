// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/netx"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
	"github.com/bsfdsagfadg/vertex/internal/transport"
	"gopkg.in/yaml.v3"
)

func (s *Server) adminGetNodes(w http.ResponseWriter, _ *http.Request) {
	list := nodes.LoadNodes()
	var enabledCount, disabledCount int
	for _, n := range list {
		if n.Disabled {
			disabledCount++
		} else {
			enabledCount++
		}
	}
	sp := nodes.GetStickyPool()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"nodes":                 list,
		"health":                nodes.LoadHealth(),
		"total":                 len(list),
		"enabled_count":         enabledCount,
		"disabled_count":        disabledCount,
		"sticky_pool_available": sp.AvailableCount(),
		"sticky_pool_in_use":    sp.StaleCount(),
		"sticky_pool_enabled":   config.Load().StickyPoolEnabled,
	})
}

func (s *Server) adminFetchSub(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [FetchSub] 开始拉取订阅 URL: %s", body.URL)
	text, err := s.fetchSubscriptionText(r.Context(), body.URL)
	if err != nil {
		log.Printf("[Admin] [FetchSub] 拉取失败: %v", err)
		s.writeJSON(w, http.StatusBadRequest, adminErr("拉取失败: "+err.Error()))
		return
	}

	newNodes := parseImportedNodes(text)
	nodes.MergeNodes(newNodes)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(newNodes)})
}

func (s *Server) adminTestAll(w http.ResponseWriter, _ *http.Request) {
	log.Printf("[Admin] [TestAll] 开始触发全局并发测速（基于 recaptchaToken 耗时）")
	go func() {
		// 使用后台独立 Context 避免 http handler 返回后 context 被取消
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		list := nodes.LoadNodes()
		log.Printf("[Admin] [TestAll] 加载到节点总数: %d", len(list))

		var wg sync.WaitGroup
		sem := make(chan struct{}, 10) // 并发度限制为 10，保护网络不瞬间过载

		for _, n := range list {
			if n.Disabled {
				log.Printf("[Admin] [TestAll] 节点已被禁用，跳过测试: %s", n.Name)
				continue
			}
			wg.Add(1)
			go func(node nodes.Node) {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
				case <-ctx.Done():
					return
				}
				defer func() { <-sem }()

				start := time.Now()
				log.Printf("[Admin] [TestAll] 开始测试节点: %s (%s)", node.Name, node.Type)

				// 1. 创建节点专用的 Session
				sess, err := s.vc.Net().CreateSession(15, node.RawURI, "admin-test-all")
				var testErr error
				if err == nil {
					// 2. 模拟 recaptcha 完整的 token 获取流程，将其作为获取速度的实际指标
					testErr = fetchRecaptchaTokenWithSess(ctx, sess)
					sess.Close()
				} else {
					testErr = err
				}

				duration := float64(time.Since(start).Milliseconds())
				if testErr != nil {
					log.Printf("[Admin] [TestAll] 节点 %s 测试失败: %v, 耗时: %.0fms", node.Name, testErr, duration)
				} else {
					log.Printf("[Admin] [TestAll] 节点 %s 测试成功, recaptcha 耗时: %.0fms", node.Name, duration)
				}
				success := testErr == nil
				nodes.RecordTest(node.RawURI, success, duration, errToStr(testErr))
				if !success {
					nodes.BatchUpdateNodesDisabled([]string{node.RawURI}, true)
				}
			}(n)
		}
		wg.Wait()
		log.Printf("[Admin] [TestAll] 全局节点测试全部结束")
	}()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminTestNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI         string  `json:"raw_uri"`
		AutoDisable    bool    `json:"auto_disable"`
		TimeoutSeconds float64 `json:"timeout_seconds"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.TimeoutSeconds <= 0 {
		body.TimeoutSeconds = 25
	}
	timeout := time.Duration(body.TimeoutSeconds * float64(time.Second))
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	start := time.Now()
	sess, err := s.vc.Net().CreateSession(15, body.RawURI, "admin-test-node")
	var testErr error
	if err == nil {
		testErr = fetchRecaptchaTokenWithSess(ctx, sess)
		sess.Close()
	} else {
		testErr = err
	}
	elapsed := float64(time.Since(start).Milliseconds())

	errStr := ""
	ok := testErr == nil
	if testErr != nil {
		if ctx.Err() != nil || errors.Is(testErr, context.DeadlineExceeded) {
			errStr = "timeout"
		} else {
			errStr = testErr.Error()
		}
	}

	disabled := false
	if body.AutoDisable {
		nodes.UpdateNodeTestResult(body.RawURI, ok, elapsed, errStr)
		disabled = !ok
		if !ok {
			nodes.BatchUpdateNodesDisabled([]string{body.RawURI}, true)
		}
	}

	log.Printf("[Admin] [TestNode] 节点测试 %s: ok=%v elapsed=%.0fms error=%q disabled=%v", nodes.GetNodeName(body.RawURI), ok, elapsed, errStr, disabled)
	s.writeJSON(w, http.StatusOK, map[string]any{
		"ok":         ok,
		"elapsed_ms": elapsed,
		"error":      errStr,
		"disabled":   disabled,
	})
}

func (s *Server) adminEnableNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	ok := nodes.EnableNode(body.RawURI)
	log.Printf("[Admin] [EnableNode] 启用节点 %s: %v", nodes.GetNodeName(body.RawURI), ok)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": ok})
}

// 模拟实测 recaptchaToken
func fetchRecaptchaTokenWithSess(ctx context.Context, sess *transport.Session) error {
	const (
		recaptchaBase = "https://www.google.com"
		siteKey       = "6LdCjtspAAAAAMcV4TGdWLJqRTEk1TfpdLqEnKdj"
		recaptchaCo   = "aHR0cHM6Ly9jb25zb2xlLmNsb3VkLmdvb2dsZS5jb206NDQz"
		recaptchaHl   = "zh-CN"
		recaptchaV    = "jdMmXeCQEkPbnFDy9T04NbgJ"
		recaptchaVh   = "6581054572"
		randomCharset = "abcdefghijklmnopqrstuvwxyz0123456789"
	)
	var (
		tokenRe = regexp.MustCompile(`id="recaptcha-token"[^>]*value="([^"]+)"`)
		rrespRe = regexp.MustCompile(`rresp","(.*?)"`)
	)

	// 随机生成 10 位回调参数
	b := make([]byte, 10)
	for i := range b {
		b[i] = randomCharset[time.Now().UnixNano()%int64(len(randomCharset))]
	}
	cb := string(b)

	anchorURL := fmt.Sprintf(
		"%s/recaptcha/enterprise/anchor?ar=1&k=%s&co=%s&hl=%s&v=%s&size=invisible&anchor-ms=20000&execute-ms=15000&cb=%s",
		recaptchaBase, siteKey, recaptchaCo, recaptchaHl, recaptchaV, cb,
	)

	_, anchorBody, err := sess.DoAndRead(ctx, "GET", anchorURL, transport.AnchorHeaders(), nil)
	if err != nil {
		return fmt.Errorf("GET anchor 失败: %w", err)
	}
	m := tokenRe.FindSubmatch(anchorBody)
	if m == nil {
		return fmt.Errorf("从 anchor HTML 解析 recaptcha-token 失败")
	}
	baseToken := string(m[1])

	form := url.Values{
		"v":      {recaptchaV},
		"reason": {"q"},
		"k":      {siteKey},
		"c":      {baseToken},
		"co":     {recaptchaCo},
		"hl":     {recaptchaHl},
		"size":   {"invisible"},
		"vh":     {recaptchaVh},
		"chr":    {""},
		"bg":     {""},
	}
	reloadURL := recaptchaBase + "/recaptcha/enterprise/reload?k=" + siteKey
	header := transport.XHRHeaders(
		"application/x-www-form-urlencoded;charset=UTF-8", "*/*",
		recaptchaBase, anchorURL, "same-origin",
	)

	_, reloadBody, err := sess.DoAndRead(ctx, "POST", reloadURL, header, strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("POST reload 失败: %w", err)
	}
	rm := rrespRe.FindSubmatch(reloadBody)
	if rm == nil {
		return fmt.Errorf("从 reload 响应解析 rresp 失败")
	}
	return nil
}

func (s *Server) adminDedupNodes(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "removed_count": nodes.DedupNodes()})
}

func (s *Server) adminDeleteDisabledNodes(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "deleted_count": nodes.DeleteDisabled()})
}

func (s *Server) adminImportNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [ImportNodes] 收到优选节点文件导入请求, 替换模式: %v", body.Replace)

	newNodes := parseImportedNodes(strings.TrimSpace(body.Text))
	if body.Replace {
		log.Printf("[Admin] [ImportNodes] 替换模式，正在清除全部已有候选节点")
		for _, cn := range nodes.LoadNodes() {
			nodes.DeleteNode(cn.RawURI)
		}
	}

	log.Printf("[Admin] [ImportNodes] 正在合并导入的新节点数量: %d", len(newNodes))
	nodes.MergeNodes(newNodes)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(newNodes)})
}

func (s *Server) adminImportNodesJson(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Text    string `json:"text"`
		Replace bool   `json:"replace"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [ImportNodesJson] 收到旧版 nodes.json 导入请求, 替换模式: %v", body.Replace)

	var d struct {
		Nodes []nodes.Node `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body.Text), &d); err != nil {
		s.writeJSON(w, http.StatusBadRequest, adminErr("JSON 解析失败: "+err.Error()))
		return
	}

	if body.Replace {
		log.Printf("[Admin] [ImportNodesJson] 替换模式，正在清除全部已有候选节点")
		for _, cn := range nodes.LoadNodes() {
			nodes.DeleteNode(cn.RawURI)
		}
	}

	log.Printf("[Admin] [ImportNodesJson] 正在合并导入的新节点数量: %d", len(d.Nodes))
	nodes.MergeNodes(d.Nodes)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "count": len(d.Nodes)})
}

func (s *Server) adminUseNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.RawURI == "" {
		_ = config.WriteSettings(map[string]any{"active_node_uri": "", "parallel_pool_enabled": true})
	} else {
		_ = config.WriteSettings(map[string]any{"active_node_uri": body.RawURI, "parallel_pool_enabled": false})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminSortNodesByLatency(w http.ResponseWriter, _ *http.Request) {
	nodes.SortNodesByLatency()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminDeleteNode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		RawURI string `json:"raw_uri"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	nodes.DeleteNode(body.RawURI)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminBatchDisableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDisable] 批量禁用 %d 个节点", len(body.URIs))
	nodes.BatchUpdateNodesDisabled(body.URIs, true)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminBatchEnableNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchEnable] 批量启用 %d 个节点", len(body.URIs))
	nodes.BatchUpdateNodesDisabled(body.URIs, false)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminBatchDeleteNodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URIs []string `json:"uris"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	log.Printf("[Admin] [BatchDelete] 批量删除 %d 个节点", len(body.URIs))
	nodes.BatchDeleteNodes(body.URIs)
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func errToStr(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// decodeSubBase64 宽容解码订阅的 Base64 文本，兼容各种换行、空格及 URL 安全格式
func decodeSubBase64(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, " ", "")
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	if b, err := base64.URLEncoding.DecodeString(s); err == nil {
		return b, nil
	}
	t := strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(t) % 4; pad != 0 {
		t += strings.Repeat("=", 4-pad)
	}
	return base64.StdEncoding.DecodeString(t) //nolint:wrapcheck
}

// parseClashYamlToURIs has been removed because it is unused

func parseInlineYamlAttrs(s string) map[string]string {
	attrs := make(map[string]string)
	var currentKey, currentValue strings.Builder
	inQuotes := false
	var quoteChar rune
	isKey := true
	braceDepth := 0
	bracketDepth := 0

	runes := []rune(s)
	for i := 0; i < len(runes); i++ {
		r := runes[i]
		if inQuotes {
			if r == quoteChar {
				inQuotes = false
			} else if r == '\\' && i+1 < len(runes) {
				if isKey {
					currentKey.WriteRune(runes[i+1])
				} else {
					currentValue.WriteRune(runes[i+1])
				}
				i++
			} else {
				if isKey {
					currentKey.WriteRune(r)
				} else {
					currentValue.WriteRune(r)
				}
			}
			continue
		}

		if r == '"' || r == '\'' {
			inQuotes = true
			quoteChar = r
			continue
		}

		if isKey {
			if r == ':' {
				isKey = false
				// 略过冒号后的空格
				if i+1 < len(runes) && runes[i+1] == ' ' {
					i++
				}
			} else if r != ' ' && r != '\t' {
				currentKey.WriteRune(r)
			}
		} else {
			switch r {
			case '{':
				braceDepth++
				currentValue.WriteRune(r)
			case '}':
				if braceDepth > 0 {
					braceDepth--
				}
				currentValue.WriteRune(r)
			case '[':
				bracketDepth++
				currentValue.WriteRune(r)
			case ']':
				if bracketDepth > 0 {
					bracketDepth--
				}
				currentValue.WriteRune(r)
			case ',':
				if braceDepth > 0 || bracketDepth > 0 {
					currentValue.WriteRune(r)
					continue
				}
				key := strings.TrimSpace(currentKey.String())
				val := strings.TrimSpace(currentValue.String())
				if key != "" {
					attrs[key] = val
				}
				currentKey.Reset()
				currentValue.Reset()
				isKey = true
			default:
				currentValue.WriteRune(r)
			}
		}
	}

	// 最后一个 key-value
	key := strings.TrimSpace(currentKey.String())
	val := strings.TrimSpace(currentValue.String())
	if key != "" {
		attrs[key] = val
	}

	return attrs
}

func isTruthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

func parseInlineYamlObject(s string) map[string]string {
	trimmed := strings.TrimSpace(s)
	if strings.HasPrefix(trimmed, "{") && strings.HasSuffix(trimmed, "}") && len(trimmed) >= 2 {
		trimmed = strings.TrimSpace(trimmed[1 : len(trimmed)-1])
	}
	if trimmed == "" {
		return map[string]string{}
	}
	return parseInlineYamlAttrs(trimmed)
}

func buildProxyURI(scheme, credential, server, port, name string, query url.Values) string {
	u := &url.URL{ //nolint:exhaustruct
		Scheme:   scheme,
		User:     url.User(credential),
		Host:     net.JoinHostPort(server, port),
		Fragment: name,
	}
	if len(query) > 0 {
		u.RawQuery = query.Encode()
	}
	return u.String()
}

func intValue(v any) int {
	switch x := v.(type) {
	case int:
		return x
	case int8:
		return int(x)
	case int16:
		return int(x)
	case int32:
		return int(x)
	case int64:
		return int(x)
	case uint:
		return int(x)
	case uint8:
		return int(x)
	case uint16:
		return int(x)
	case uint32:
		return int(x)
	case uint64:
		return int(x)
	case float32:
		return int(x)
	case float64:
		return int(x)
	case string:
		n, _ := strconv.Atoi(strings.TrimSpace(x))
		return n
	default:
		return 0
	}
}

func boolValue(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return isTruthy(x)
	default:
		return false
	}
}

func mapValue(v any) map[string]any {
	out, _ := normalizeYAMLValue(v).(map[string]any)
	return out
}

func sliceValue(v any) []any {
	out, _ := normalizeYAMLValue(v).([]any)
	return out
}

func firstMapValue(v any) map[string]any {
	items := sliceValue(v)
	if len(items) == 0 {
		return nil
	}
	return mapValue(items[0])
}

func parseJSONMapString(s string) map[string]any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func nestedObject(obj map[string]any, keys ...string) map[string]any {
	for _, key := range keys {
		if key == "" {
			continue
		}
		if nested := mapValue(obj[key]); len(nested) > 0 {
			return nested
		}
		if nested := parseJSONMapString(valueToString(obj[key])); len(nested) > 0 {
			return nested
		}
	}
	return nil
}

func splitCSV(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func parseReservedBytes(s string) []int {
	parts := splitCSV(s)
	if len(parts) == 0 {
		return nil
	}
	out := make([]int, 0, len(parts))
	for _, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil {
			return nil
		}
		out = append(out, n)
	}
	return out
}

func splitInterfaceAddresses(s string) (string, string) {
	var ipv4, ipv6 string
	for _, part := range splitCSV(s) {
		if strings.Contains(part, ":") {
			if ipv6 == "" {
				ipv6 = part
			}
			continue
		}
		if ipv4 == "" {
			ipv4 = part
		}
	}
	return ipv4, ipv6
}

func normalizeImportedNetwork(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "", "raw", "tcp":
		return ""
	default:
		return s
	}
}

func importedAllowInsecure(v any) bool {
	switch x := v.(type) {
	case bool:
		return x
	case string:
		return strings.EqualFold(strings.TrimSpace(x), "true") || isTruthy(x)
	default:
		return false
	}
}

func applyCommonImportedProxyFields(proxy map[string]any, obj map[string]any) {
	if sni := strings.TrimSpace(valueToString(obj["Sni"])); sni != "" {
		proxy["sni"] = sni
		proxy["servername"] = sni
	}
	if fp := strings.TrimSpace(valueToString(obj["Fingerprint"])); fp != "" {
		proxy["client-fingerprint"] = fp
		proxy["fingerprint"] = fp
	}
	if importedAllowInsecure(obj["AllowInsecure"]) {
		proxy["skip-cert-verify"] = true
	}
	if alpn := splitCSV(valueToString(obj["Alpn"])); len(alpn) > 0 {
		proxy["alpn"] = alpn
	}
	if cert := strings.TrimSpace(valueToString(obj["Cert"])); cert != "" {
		proxy["certificate"] = cert
	}
	if privateKey := strings.TrimSpace(valueToString(obj["PrivateKey"])); privateKey != "" {
		proxy["private-key"] = privateKey
	}
}

func applyTransportExtras(proxy map[string]any, obj map[string]any, transport map[string]any) {
	network := normalizeImportedNetwork(valueToString(obj["Network"]))
	if network == "" {
		return
	}

	switch network {
	case "ws":
		proxy["network"] = "ws"
		wsOpts := map[string]any{}
		if path := strings.TrimSpace(valueToString(transport["Path"])); path != "" {
			wsOpts["path"] = path
		}
		if host := strings.TrimSpace(valueToString(transport["Host"])); host != "" {
			wsOpts["headers"] = map[string]any{"Host": host}
		}
		if len(wsOpts) > 0 {
			proxy["ws-opts"] = wsOpts
		}
	case "grpc":
		proxy["network"] = "grpc"
		grpcOpts := map[string]any{}
		if serviceName := strings.TrimSpace(valueToString(transport["GrpcServiceName"])); serviceName != "" {
			grpcOpts["grpc-service-name"] = serviceName
		}
		if len(grpcOpts) > 0 {
			proxy["grpc-opts"] = grpcOpts
		}
	case "http", "h2":
		proxy["network"] = "http"
		httpOpts := map[string]any{}
		if path := strings.TrimSpace(valueToString(transport["Path"])); path != "" {
			httpOpts["path"] = []string{path}
		}
		if host := strings.TrimSpace(valueToString(transport["Host"])); host != "" {
			httpOpts["headers"] = map[string][]string{"Host": []string{host}}
		}
		if len(httpOpts) > 0 {
			httpOpts["method"] = "GET"
			proxy["http-opts"] = httpOpts
		}
	case "xhttp":
		proxy["network"] = "xhttp"
		xhttpOpts := map[string]any{}
		if path := strings.TrimSpace(valueToString(transport["Path"])); path != "" {
			xhttpOpts["path"] = path
		}
		if host := strings.TrimSpace(valueToString(transport["Host"])); host != "" {
			xhttpOpts["host"] = host
		}
		if mode := strings.TrimSpace(valueToString(transport["XhttpMode"])); mode != "" {
			xhttpOpts["mode"] = mode
		}
		if headers := parseJSONMapString(valueToString(transport["XhttpExtra"])); len(headers) > 0 {
			xhttpOpts["extra"] = headers
		}
		if len(xhttpOpts) > 0 {
			proxy["xhttp-opts"] = xhttpOpts
		}
	default:
		proxy["network"] = network
	}
}

func applyImportedTLSFields(proxy map[string]any, obj map[string]any) {
	streamSecurity := strings.ToLower(strings.TrimSpace(valueToString(obj["StreamSecurity"])))
	switch streamSecurity {
	case "tls":
		proxy["tls"] = true
	case "reality":
		proxy["tls"] = true
		proxy["reality-opts"] = map[string]any{
			"public-key": strings.TrimSpace(valueToString(obj["PublicKey"])),
			"short-id":   strings.TrimSpace(valueToString(obj["ShortId"])),
		}
	}
}

func buildClashURI(proxy map[string]any) string {
	body, err := json.Marshal(proxy)
	if err != nil {
		return ""
	}
	return "clash://" + base64.StdEncoding.EncodeToString(body)
}

func v2rayNConfigType(v any) int {
	switch x := v.(type) {
	case string:
		switch strings.ToLower(strings.TrimSpace(x)) {
		case "vmess":
			return 1
		case "shadowsocks", "ss":
			return 3
		case "socks", "socks5":
			return 4
		case "vless":
			return 5
		case "trojan":
			return 6
		case "hysteria2", "hy2":
			return 7
		case "tuic":
			return 8
		case "wireguard":
			return 9
		case "http":
			return 10
		case "anytls":
			return 11
		case "naive":
			return 12
		default:
			return intValue(x)
		}
	default:
		return intValue(v)
	}
}

func buildImportedProxyFromV2RayNProfile(obj map[string]any) map[string]any {
	cfgType := v2rayNConfigType(obj["ConfigType"])
	if cfgType == 0 {
		return nil
	}

	name := firstNonEmpty(valueToString(obj["Remarks"]), valueToString(obj["Name"]))
	server := strings.TrimSpace(valueToString(obj["Address"]))
	port := intValue(obj["Port"])
	password := strings.TrimSpace(valueToString(obj["Password"]))
	username := strings.TrimSpace(valueToString(obj["Username"]))
	proto := nestedObject(obj, "ProtoExtraObj", "ProtoExtra")
	transportExtra := nestedObject(obj, "TransportExtraObj", "TransportExtra")

	switch cfgType {
	case 1:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":    name,
			"type":    "vmess",
			"server":  server,
			"port":    port,
			"uuid":    password,
			"cipher":  firstNonEmpty(valueToString(proto["VmessSecurity"]), "auto"),
			"alterId": intValue(proto["AlterId"]),
			"udp":     true,
		}
		applyCommonImportedProxyFields(proxy, obj)
		applyImportedTLSFields(proxy, obj)
		applyTransportExtras(proxy, obj, transportExtra)
		return proxy
	case 3:
		method := strings.TrimSpace(valueToString(proto["SsMethod"]))
		if server == "" || port == 0 || password == "" || method == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "ss",
			"server":   server,
			"port":     port,
			"cipher":   method,
			"password": password,
			"udp":      true,
		}
		return proxy
	case 4:
		if server == "" || port == 0 {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "socks5",
			"server":   server,
			"port":     port,
			"username": username,
			"password": password,
			"udp":      true,
		}
		return proxy
	case 5:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":       name,
			"type":       "vless",
			"server":     server,
			"port":       port,
			"uuid":       password,
			"encryption": firstNonEmpty(valueToString(proto["VlessEncryption"]), "none"),
			"udp":        true,
		}
		if flow := strings.TrimSpace(valueToString(proto["Flow"])); flow != "" {
			proxy["flow"] = flow
		}
		applyCommonImportedProxyFields(proxy, obj)
		applyImportedTLSFields(proxy, obj)
		applyTransportExtras(proxy, obj, transportExtra)
		return proxy
	case 6:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "trojan",
			"server":   server,
			"port":     port,
			"password": password,
			"udp":      true,
		}
		applyCommonImportedProxyFields(proxy, obj)
		applyImportedTLSFields(proxy, obj)
		applyTransportExtras(proxy, obj, transportExtra)
		return proxy
	case 7:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "hysteria2",
			"server":   server,
			"port":     port,
			"password": password,
			"udp":      true,
		}
		if ports := strings.TrimSpace(firstNonEmpty(valueToString(proto["Ports"]), valueToString(obj["Ports"]))); ports != "" {
			proxy["ports"] = strings.ReplaceAll(ports, ":", "-")
		}
		if obfsPassword := strings.TrimSpace(valueToString(proto["SalamanderPass"])); obfsPassword != "" {
			proxy["obfs"] = "salamander"
			proxy["obfs-password"] = obfsPassword
		}
		applyCommonImportedProxyFields(proxy, obj)
		return proxy
	case 8:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "tuic",
			"server":   server,
			"port":     port,
			"password": password,
			"udp":      true,
		}
		if username != "" {
			proxy["uuid"] = username
		} else {
			proxy["token"] = password
			delete(proxy, "password")
		}
		if cc := strings.TrimSpace(valueToString(proto["CongestionControl"])); cc != "" {
			proxy["congestion-controller"] = cc
		}
		applyCommonImportedProxyFields(proxy, obj)
		return proxy
	case 9:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":        name,
			"type":        "wireguard",
			"server":      server,
			"port":        port,
			"private-key": password,
			"public-key":  strings.TrimSpace(valueToString(proto["WgPublicKey"])),
			"udp":         true,
		}
		if preSharedKey := strings.TrimSpace(valueToString(proto["WgPresharedKey"])); preSharedKey != "" {
			proxy["pre-shared-key"] = preSharedKey
		}
		if reserved := parseReservedBytes(valueToString(proto["WgReserved"])); len(reserved) > 0 {
			proxy["reserved"] = reserved
		}
		if mtu := intValue(proto["WgMtu"]); mtu > 0 {
			proxy["mtu"] = mtu
		}
		ip, ipv6 := splitInterfaceAddresses(valueToString(proto["WgInterfaceAddress"]))
		if ip != "" {
			proxy["ip"] = ip
		}
		if ipv6 != "" {
			proxy["ipv6"] = ipv6
		}
		return proxy
	case 10:
		if server == "" || port == 0 {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "http",
			"server":   server,
			"port":     port,
			"username": username,
			"password": password,
		}
		applyCommonImportedProxyFields(proxy, obj)
		return proxy
	case 11:
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "anytls",
			"server":   server,
			"port":     port,
			"password": password,
			"udp":      true,
		}
		applyCommonImportedProxyFields(proxy, obj)
		return proxy
	default:
		return nil
	}
}

func applyV2RayStreamSettings(proxy map[string]any, stream map[string]any) {
	if len(stream) == 0 {
		return
	}

	network := strings.ToLower(strings.TrimSpace(valueToString(stream["network"])))
	switch network {
	case "ws":
		proxy["network"] = "ws"
		wsSettings := mapValue(stream["wsSettings"])
		wsOpts := map[string]any{}
		if path := strings.TrimSpace(valueToString(wsSettings["path"])); path != "" {
			wsOpts["path"] = path
		}
		if host := strings.TrimSpace(valueToString(mapValue(wsSettings["headers"])["Host"])); host != "" {
			wsOpts["headers"] = map[string]any{"Host": host}
		}
		if len(wsOpts) > 0 {
			proxy["ws-opts"] = wsOpts
		}
	case "grpc":
		proxy["network"] = "grpc"
		grpcSettings := mapValue(stream["grpcSettings"])
		if serviceName := strings.TrimSpace(firstNonEmpty(valueToString(grpcSettings["serviceName"]), valueToString(grpcSettings["grpc-service-name"]))); serviceName != "" {
			proxy["grpc-opts"] = map[string]any{"grpc-service-name": serviceName}
		}
	case "http", "h2":
		proxy["network"] = "http"
		httpSettings := mapValue(stream["httpSettings"])
		httpOpts := map[string]any{"method": "GET"}
		if path := strings.TrimSpace(valueToString(httpSettings["path"])); path != "" {
			httpOpts["path"] = []string{path}
		}
		hostValue := httpSettings["host"]
		if hosts := sliceValue(hostValue); len(hosts) > 0 {
			host := strings.TrimSpace(valueToString(hosts[0]))
			if host != "" {
				httpOpts["headers"] = map[string][]string{"Host": []string{host}}
			}
		} else if host := strings.TrimSpace(valueToString(hostValue)); host != "" {
			httpOpts["headers"] = map[string][]string{"Host": []string{host}}
		}
		proxy["http-opts"] = httpOpts
	}

	security := strings.ToLower(strings.TrimSpace(valueToString(stream["security"])))
	switch security {
	case "tls":
		proxy["tls"] = true
		tlsSettings := mapValue(stream["tlsSettings"])
		if sni := strings.TrimSpace(firstNonEmpty(valueToString(tlsSettings["serverName"]), valueToString(tlsSettings["sni"]))); sni != "" {
			proxy["servername"] = sni
			proxy["sni"] = sni
		}
		if fp := strings.TrimSpace(firstNonEmpty(valueToString(tlsSettings["fingerprint"]), valueToString(tlsSettings["fp"]))); fp != "" {
			proxy["client-fingerprint"] = fp
			proxy["fingerprint"] = fp
		}
		if boolValue(tlsSettings["allowInsecure"]) {
			proxy["skip-cert-verify"] = true
		}
		if alpn := splitCSV(valueToString(tlsSettings["alpn"])); len(alpn) > 0 {
			proxy["alpn"] = alpn
		}
	case "reality":
		proxy["tls"] = true
		realitySettings := mapValue(stream["realitySettings"])
		if sni := strings.TrimSpace(firstNonEmpty(valueToString(realitySettings["serverName"]), valueToString(realitySettings["sni"]))); sni != "" {
			proxy["servername"] = sni
			proxy["sni"] = sni
		}
		if fp := strings.TrimSpace(firstNonEmpty(valueToString(realitySettings["fingerprint"]), valueToString(realitySettings["fp"]))); fp != "" {
			proxy["client-fingerprint"] = fp
			proxy["fingerprint"] = fp
		}
		proxy["reality-opts"] = map[string]any{
			"public-key": strings.TrimSpace(valueToString(realitySettings["publicKey"])),
			"short-id":   strings.TrimSpace(firstNonEmpty(valueToString(realitySettings["shortId"]), valueToString(realitySettings["short-id"]))),
		}
	}
}

func buildImportedProxyFromV2RayOutbound(obj map[string]any) map[string]any {
	protocol := strings.ToLower(strings.TrimSpace(valueToString(obj["protocol"])))
	if protocol == "" {
		return nil
	}

	name := firstNonEmpty(valueToString(obj["remarks"]), valueToString(obj["tag"]), protocol)
	settings := mapValue(obj["settings"])
	streamSettings := mapValue(obj["streamSettings"])

	switch protocol {
	case "vmess", "vless":
		vnext := firstMapValue(settings["vnext"])
		user := firstMapValue(vnext["users"])
		server := strings.TrimSpace(valueToString(vnext["address"]))
		port := intValue(vnext["port"])
		id := strings.TrimSpace(valueToString(user["id"]))
		if server == "" || port == 0 || id == "" {
			return nil
		}
		proxy := map[string]any{
			"name":   name,
			"type":   protocol,
			"server": server,
			"port":   port,
			"uuid":   id,
			"udp":    true,
		}
		if protocol == "vmess" {
			proxy["cipher"] = firstNonEmpty(valueToString(user["security"]), "auto")
			proxy["alterId"] = intValue(user["alterId"])
		} else {
			proxy["encryption"] = firstNonEmpty(valueToString(user["encryption"]), "none")
			if flow := strings.TrimSpace(valueToString(user["flow"])); flow != "" {
				proxy["flow"] = flow
			}
		}
		applyV2RayStreamSettings(proxy, streamSettings)
		return proxy
	case "trojan":
		serverInfo := firstMapValue(settings["servers"])
		server := strings.TrimSpace(valueToString(serverInfo["address"]))
		port := intValue(serverInfo["port"])
		password := strings.TrimSpace(valueToString(serverInfo["password"]))
		if server == "" || port == 0 || password == "" {
			return nil
		}
		proxy := map[string]any{
			"name":     name,
			"type":     "trojan",
			"server":   server,
			"port":     port,
			"password": password,
			"udp":      true,
		}
		applyV2RayStreamSettings(proxy, streamSettings)
		return proxy
	case "shadowsocks":
		serverInfo := firstMapValue(settings["servers"])
		server := strings.TrimSpace(valueToString(serverInfo["address"]))
		port := intValue(serverInfo["port"])
		password := strings.TrimSpace(valueToString(serverInfo["password"]))
		method := strings.TrimSpace(valueToString(serverInfo["method"]))
		if server == "" || port == 0 || password == "" || method == "" {
			return nil
		}
		return map[string]any{
			"name":     name,
			"type":     "ss",
			"server":   server,
			"port":     port,
			"cipher":   method,
			"password": password,
			"udp":      true,
		}
	case "socks":
		serverInfo := firstMapValue(settings["servers"])
		server := strings.TrimSpace(valueToString(serverInfo["address"]))
		port := intValue(serverInfo["port"])
		if server == "" || port == 0 {
			return nil
		}
		return map[string]any{
			"name":     name,
			"type":     "socks5",
			"server":   server,
			"port":     port,
			"username": strings.TrimSpace(valueToString(serverInfo["user"])),
			"password": strings.TrimSpace(valueToString(serverInfo["pass"])),
			"udp":      true,
		}
	case "http":
		serverInfo := firstMapValue(settings["servers"])
		server := strings.TrimSpace(valueToString(serverInfo["address"]))
		port := intValue(serverInfo["port"])
		if server == "" || port == 0 {
			return nil
		}
		return map[string]any{
			"name":     name,
			"type":     "http",
			"server":   server,
			"port":     port,
			"username": strings.TrimSpace(valueToString(serverInfo["user"])),
			"password": strings.TrimSpace(valueToString(serverInfo["pass"])),
		}
	default:
		return nil
	}
}

func buildImportedProxyFromSIP008(obj map[string]any) map[string]any {
	server := strings.TrimSpace(valueToString(obj["server"]))
	port := intValue(obj["server_port"])
	method := strings.TrimSpace(firstNonEmpty(valueToString(obj["method"]), valueToString(obj["cipher"])))
	password := strings.TrimSpace(valueToString(obj["password"]))
	if server == "" || port == 0 || method == "" || password == "" {
		return nil
	}
	return map[string]any{
		"name":     firstNonEmpty(valueToString(obj["remarks"]), valueToString(obj["name"])),
		"type":     "ss",
		"server":   server,
		"port":     port,
		"cipher":   method,
		"password": password,
		"udp":      true,
	}
}

func supportedClashProxyType(typ string) bool {
	switch typ {
	case "ss", "ssr", "socks5", "http", "vmess", "vless", "snell", "trojan", "hysteria", "hysteria2", "wireguard", "tuic", "gost-relay", "ssh", "mieru", "anytls", "sudoku", "masque", "trusttunnel", "openvpn", "tailscale":
		return true
	default:
		return false
	}
}

func looksLikeClashProxyMap(obj map[string]any) bool {
	typ := strings.ToLower(strings.TrimSpace(valueToString(obj["type"])))
	if !supportedClashProxyType(typ) {
		return false
	}
	if typ == "wireguard" {
		return strings.TrimSpace(valueToString(obj["private-key"])) != "" &&
			(strings.TrimSpace(valueToString(obj["server"])) != "" || len(sliceValue(obj["peers"])) > 0)
	}
	return strings.TrimSpace(valueToString(obj["server"])) != "" && intValue(obj["port"]) > 0
}

func buildImportedNodeFromProxyMap(proxy map[string]any) (nodes.Node, bool) {
	if len(proxy) == 0 {
		return nodes.Node{}, false //nolint:exhaustruct
	}
	raw := buildClashURI(proxy)
	if raw == "" {
		return nodes.Node{}, false //nolint:exhaustruct
	}
	return parseImportedNodeLine(raw)
}

func buildImportedNodeFromMap(obj map[string]any) (nodes.Node, bool) {
	if proxy := buildImportedProxyFromV2RayNProfile(obj); len(proxy) > 0 {
		return buildImportedNodeFromProxyMap(proxy)
	}
	if proxy := buildImportedProxyFromV2RayOutbound(obj); len(proxy) > 0 {
		return buildImportedNodeFromProxyMap(proxy)
	}
	if proxy := buildImportedProxyFromSIP008(obj); len(proxy) > 0 {
		return buildImportedNodeFromProxyMap(proxy)
	}
	if looksLikeClashProxyMap(obj) {
		return buildClashNode(obj)
	}
	return nodes.Node{}, false //nolint:exhaustruct
}

func buildImportedNodesFromSlice(items []any) []nodes.Node {
	imported := make([]nodes.Node, 0, len(items))
	for _, item := range items {
		obj := mapValue(item)
		if len(obj) == 0 {
			continue
		}
		if node, ok2 := buildImportedNodeFromMap(obj); ok2 {
			imported = append(imported, node)
		}
	}
	return imported
}

func parseJSONImportedNodes(text string) []nodes.Node {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	var raw any
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return nil
	}

	normalized := normalizeYAMLValue(raw)
	if obj, ok := normalized.(map[string]any); ok {
		if proxies := buildImportedNodesFromSlice(sliceValue(obj["proxies"])); len(proxies) > 0 {
			return proxies
		}
		if outbounds := buildImportedNodesFromSlice(sliceValue(obj["outbounds"])); len(outbounds) > 0 {
			return outbounds
		}
		if servers := buildImportedNodesFromSlice(sliceValue(obj["servers"])); len(servers) > 0 {
			return servers
		}
		if node, ok2 := buildImportedNodeFromMap(obj); ok2 { //nolint:govet
			return []nodes.Node{node}
		}
		return nil
	}
	if items, ok := normalized.([]any); ok {
		return buildImportedNodesFromSlice(items)
	}
	return nil
}

func parseV2RayNNodeLine(line string) (nodes.Node, bool) {
	raw := strings.TrimSpace(line)
	if !strings.HasPrefix(strings.ToLower(raw), "v2rayn://") {
		return nodes.Node{}, false
	}

	body := raw[len("v2rayn://"):]
	slash := strings.IndexByte(body, '/')
	if slash <= 0 || slash+1 >= len(body) {
		return nodes.Node{}, false
	}

	encoded := body[slash+1:]
	encoded = strings.ReplaceAll(strings.ReplaceAll(encoded, "-", "+"), "_", "/")
	if pad := len(encoded) % 4; pad != 0 {
		encoded += strings.Repeat("=", 4-pad)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nodes.Node{}, false
	}

	var obj map[string]any
	if errUnm := json.Unmarshal(decoded, &obj); errUnm != nil { //nolint:govet
		return nodes.Node{}, false
	}

	normalized, _ := normalizeYAMLValue(obj).(map[string]any)
	if len(normalized) == 0 {
		return nodes.Node{}, false
	}
	return buildImportedNodeFromMap(normalized)
}

func parseFlexibleImportedNodeLine(line string) (nodes.Node, bool) {
	if node, ok := parseImportedNodeLine(line); ok {
		return node, true
	}
	return parseV2RayNNodeLine(line)
}

func clashProxyToURI(attrs map[string]string) string {
	typ := strings.ToLower(strings.TrimSpace(attrs["type"]))
	name := attrs["name"]
	server := attrs["server"]
	port := attrs["port"]

	if server == "" || port == "" {
		return ""
	}

	switch typ {
	case "ss":
		cipher := attrs["cipher"]
		password := attrs["password"]
		if cipher == "" || password == "" {
			return ""
		}
		// ss 格式: ss://base64(cipher:password)@server:port#name
		userinfo := base64.StdEncoding.EncodeToString([]byte(cipher + ":" + password))
		return "ss://" + userinfo + "@" + server + ":" + port + "#" + url.QueryEscape(name)

	case "vmess":
		uuid := attrs["uuid"]
		alterIdStr := attrs["alterId"]
		if alterIdStr == "" {
			alterIdStr = "0"
		}
		alterId, _ := strconv.Atoi(alterIdStr)

		tlsEnabled := false
		if attrs["tls"] == "true" {
			tlsEnabled = true
		}

		// 构造 vmess json 结构
		vmessJSON := map[string]any{
			"v":    "2",
			"ps":   name,
			"add":  server,
			"port": port,
			"id":   uuid,
			"aid":  alterId,
			"net":  "tcp",
			"type": "none",
			"host": "",
			"path": "",
			"tls":  "",
		}

		if attrs["network"] == "ws" {
			vmessJSON["net"] = "ws"
			if wsOpts, ok := attrs["ws-opts"]; ok {
				// 提取 path 和 Host
				// 极简提取 ws-opts 中的 path 和 headers
				path := "/"
				if idx := strings.Index(wsOpts, "path:"); idx != -1 {
					sub := wsOpts[idx+5:]
					if commaIdx := strings.Index(sub, ","); commaIdx != -1 {
						sub = sub[:commaIdx]
					}
					path = strings.Trim(strings.TrimSpace(sub), "\"'{}")
				}
				vmessJSON["path"] = path

				host := ""
				if idx := strings.Index(wsOpts, "Host:"); idx != -1 {
					sub := wsOpts[idx+5:]
					if commaIdx := strings.Index(sub, ","); commaIdx != -1 {
						sub = sub[:commaIdx]
					}
					if braceIdx := strings.Index(sub, "}"); braceIdx != -1 {
						sub = sub[:braceIdx]
					}
					host = strings.Trim(strings.TrimSpace(sub), "\"'{}")
				}
				vmessJSON["host"] = host
			}
		}

		if tlsEnabled {
			vmessJSON["tls"] = "tls"
		}

		jsonBytes, _ := json.Marshal(vmessJSON)
		b64Str := base64.StdEncoding.EncodeToString(jsonBytes)
		return "vmess://" + b64Str

	case "vless":
		uuid := attrs["uuid"]
		if uuid == "" {
			return ""
		}

		query := url.Values{}
		serverName := firstNonEmpty(attrs["servername"], attrs["sni"], server)
		realityOpts := parseInlineYamlObject(attrs["reality-opts"])
		if len(realityOpts) > 0 {
			query.Set("security", "reality")
			if publicKey := realityOpts["public-key"]; publicKey != "" {
				query.Set("pbk", publicKey)
			}
			if shortID := realityOpts["short-id"]; shortID != "" {
				query.Set("sid", shortID)
			}
		} else if isTruthy(attrs["tls"]) {
			query.Set("security", "tls")
		}
		if serverName != "" {
			query.Set("sni", serverName)
		}
		if isTruthy(attrs["skip-cert-verify"]) {
			query.Set("allowInsecure", "1")
		}
		if flow := attrs["flow"]; flow != "" {
			query.Set("flow", flow)
		}
		if fp := attrs["client-fingerprint"]; fp != "" {
			query.Set("fp", fp)
		}
		if network := strings.ToLower(strings.TrimSpace(attrs["network"])); network != "" {
			query.Set("type", network)
			switch network {
			case "ws":
				wsOpts := parseInlineYamlObject(attrs["ws-opts"])
				if path := wsOpts["path"]; path != "" {
					query.Set("path", path)
				}
				headers := parseInlineYamlObject(wsOpts["headers"])
				if host := firstNonEmpty(headers["Host"], headers["host"]); host != "" {
					query.Set("host", host)
				}
			case "grpc":
				grpcOpts := parseInlineYamlObject(attrs["grpc-opts"])
				if serviceName := firstNonEmpty(grpcOpts["grpc-service-name"], grpcOpts["serviceName"]); serviceName != "" {
					query.Set("serviceName", serviceName)
				}
			}
		}
		return buildProxyURI("vless", uuid, server, port, name, query)

	case "trojan":
		password := attrs["password"]
		if password == "" {
			return ""
		}

		query := url.Values{}
		if sni := firstNonEmpty(attrs["sni"], attrs["servername"], server); sni != "" {
			query.Set("sni", sni)
		}
		if isTruthy(attrs["skip-cert-verify"]) {
			query.Set("allowInsecure", "1")
		}
		if fp := attrs["client-fingerprint"]; fp != "" {
			query.Set("fp", fp)
		}
		if network := strings.ToLower(strings.TrimSpace(attrs["network"])); network != "" {
			query.Set("type", network)
			switch network {
			case "ws":
				wsOpts := parseInlineYamlObject(attrs["ws-opts"])
				if path := wsOpts["path"]; path != "" {
					query.Set("path", path)
				}
				headers := parseInlineYamlObject(wsOpts["headers"])
				if host := firstNonEmpty(headers["Host"], headers["host"]); host != "" {
					query.Set("host", host)
				}
			case "grpc":
				grpcOpts := parseInlineYamlObject(attrs["grpc-opts"])
				if serviceName := firstNonEmpty(grpcOpts["grpc-service-name"], grpcOpts["serviceName"]); serviceName != "" {
					query.Set("serviceName", serviceName)
				}
			}
		}
		return buildProxyURI("trojan", password, server, port, name, query)

	case "hysteria2", "hy2":
		password := attrs["password"]
		if password == "" {
			return ""
		}

		query := url.Values{}
		if sni := firstNonEmpty(attrs["sni"], attrs["servername"], server); sni != "" {
			query.Set("sni", sni)
		}
		if isTruthy(attrs["skip-cert-verify"]) {
			query.Set("insecure", "1")
		}
		if ports := firstNonEmpty(attrs["ports"], attrs["mport"]); ports != "" {
			query.Set("ports", ports)
		}
		if obfs := attrs["obfs"]; obfs != "" {
			query.Set("obfs", obfs)
		}
		if obfsPassword := attrs["obfs-password"]; obfsPassword != "" {
			query.Set("obfs-password", obfsPassword)
		}
		if fp := firstNonEmpty(attrs["client-fingerprint"], attrs["fingerprint"]); fp != "" {
			query.Set("fp", fp)
		}
		return buildProxyURI("hy2", password, server, port, name, query)
	}

	return ""
}

func parseImportedNodes(text string) []nodes.Node {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	normalized := maybeDecodeSubscriptionText(text)
	if imported := parseClashYAMLToNodes(normalized); len(imported) > 0 {
		return imported
	}
	if imported := parseJSONImportedNodes(normalized); len(imported) > 0 {
		return imported
	}

	var imported []nodes.Node
	for _, line := range strings.Split(normalized, "\n") {
		if node, ok := parseFlexibleImportedNodeLine(line); ok {
			imported = append(imported, node)
		}
	}
	return imported
}

func maybeDecodeSubscriptionText(text string) string {
	b, err := decodeSubBase64(text)
	if err != nil {
		return text
	}

	decoded := strings.TrimSpace(string(b))
	if decoded == "" {
		return text
	}
	if strings.Contains(decoded, "proxies:") || hasImportableNodeLine(decoded) || len(parseJSONImportedNodes(decoded)) > 0 {
		return decoded
	}
	return text
}

func hasImportableNodeLine(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if _, ok := parseFlexibleImportedNodeLine(line); ok {
			return true
		}
	}
	return false
}

func parseImportedNodeLine(line string) (nodes.Node, bool) {
	raw := strings.TrimSpace(line)
	if raw == "" {
		return nodes.Node{}, false
	}

	out, err := transport.ParseURI(raw)
	if err != nil {
		return nodes.Node{}, false
	}

	nodeType := strings.TrimSpace(valueToString(out["type"]))
	if nodeType == "" {
		return nodes.Node{}, false
	}

	nodeName := extractImportedNodeName(raw, out)
	if nodeName == "" {
		nodeName = raw[:min(len(raw), 40)]
	}
	return nodes.Node{Type: nodeType, Name: nodeName, RawURI: raw}, true //nolint:exhaustruct
}

func extractImportedNodeName(raw string, out map[string]any) string {
	if name := strings.TrimSpace(valueToString(out["name"])); name != "" {
		return name
	}

	if strings.HasPrefix(raw, "vmess://") {
		b64Str := raw[8:]
		if idx := strings.Index(b64Str, "?"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if idx := strings.Index(b64Str, "#"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		b64Str = strings.ReplaceAll(strings.ReplaceAll(b64Str, "-", "+"), "_", "/")
		if pad := len(b64Str) % 4; pad != 0 {
			b64Str += strings.Repeat("=", 4-pad)
		}
		if b, err := base64.StdEncoding.DecodeString(b64Str); err == nil {
			var d map[string]any
			if errUnm := json.Unmarshal(b, &d); errUnm == nil { //nolint:govet
				if ps, ok := d["ps"].(string); ok {
					return strings.TrimSpace(ps)
				}
			}
		}
	}

	if idx := strings.Index(raw, "#"); idx != -1 {
		escapedName := raw[idx+1:]
		if dec, err := url.QueryUnescape(escapedName); err == nil {
			return strings.TrimSpace(dec)
		}
		return strings.TrimSpace(escapedName)
	}

	return ""
}

func valueToString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprintf("%v", x)
	}
}

func parseClashYAMLToNodes(yamlText string) []nodes.Node {
	yamlText = strings.TrimSpace(yamlText)
	if yamlText == "" {
		return nil
	}

	if imported := parseStructuredClashYAMLNodes(yamlText); len(imported) > 0 {
		return imported
	}
	return parseInlineClashYAMLNodes(yamlText)
}

func parseStructuredClashYAMLNodes(yamlText string) []nodes.Node {
	var doc struct {
		Proxies []map[string]any `yaml:"proxies"`
	}
	if err := yaml.Unmarshal([]byte(yamlText), &doc); err == nil && len(doc.Proxies) > 0 {
		return buildClashNodes(doc.Proxies)
	}

	var proxies []map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &proxies); err == nil && len(proxies) > 0 {
		return buildClashNodes(proxies)
	}

	var proxy map[string]any
	if err := yaml.Unmarshal([]byte(yamlText), &proxy); err == nil && len(proxy) > 0 {
		if normalized, ok := normalizeYAMLValue(proxy).(map[string]any); ok && looksLikeClashProxyMap(normalized) {
			if node, ok2 := buildClashNode(normalized); ok2 { //nolint:govet
				return []nodes.Node{node}
			}
		}
	}

	return nil
}

func parseInlineClashYAMLNodes(yamlText string) []nodes.Node {
	var imported []nodes.Node
	lines := strings.Split(yamlText, "\n")
	inProxies := false

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "proxies:") {
			inProxies = true
			continue
		}
		if inProxies && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && strings.Contains(trimmed, ":") {
			inProxies = false
		}
		if !inProxies || !strings.HasPrefix(trimmed, "-") {
			continue
		}

		inline := strings.TrimSpace(strings.TrimPrefix(trimmed, "-"))
		if !strings.HasPrefix(inline, "{") || !strings.HasSuffix(inline, "}") {
			continue
		}

		var proxy map[string]any
		if err := yaml.Unmarshal([]byte(inline), &proxy); err == nil {
			if node, ok := buildClashNode(proxy); ok {
				imported = append(imported, node)
				continue
			}
		}

		cleaned := inline[1 : len(inline)-1]
		attrs := parseInlineYamlAttrs(cleaned)
		if uri := clashProxyToURI(attrs); uri != "" {
			if node, ok := parseImportedNodeLine(uri); ok {
				imported = append(imported, node)
			}
		}
	}

	return imported
}

func buildClashNodes(proxies []map[string]any) []nodes.Node {
	imported := make([]nodes.Node, 0, len(proxies))
	for _, proxy := range proxies {
		if node, ok := buildClashNode(proxy); ok {
			imported = append(imported, node)
		}
	}
	return imported
}

func buildClashNode(proxy map[string]any) (nodes.Node, bool) {
	normalized, ok := normalizeYAMLValue(proxy).(map[string]any)
	if !ok || len(normalized) == 0 {
		return nodes.Node{}, false
	}
	if !looksLikeClashProxyMap(normalized) {
		return nodes.Node{}, false
	}

	rawURI := clashProxyObjectToURI(normalized)
	if rawURI == "" {
		return nodes.Node{}, false
	}
	return parseImportedNodeLine(rawURI)
}

func clashProxyObjectToURI(proxy map[string]any) string {
	body, err := json.Marshal(proxy)
	if err != nil {
		return ""
	}
	return "clash://" + base64.StdEncoding.EncodeToString(body)
}

func normalizeYAMLValue(value any) any {
	switch v := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = normalizeYAMLValue(item)
		}
		return out
	case map[any]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[fmt.Sprintf("%v", key)] = normalizeYAMLValue(item)
		}
		return out
	case []any:
		out := make([]any, 0, len(v))
		for _, item := range v {
			out = append(out, normalizeYAMLValue(item))
		}
		return out
	default:
		return v
	}
}

const subscriptionFetchUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"

func (s *Server) fetchSubscriptionText(ctx context.Context, rawURL string) (string, error) {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return "", errors.New("subscription url is empty")
	}

	data, err := fetchSubscriptionDataDirect(ctx, rawURL)
	if err == nil {
		return strings.TrimSpace(string(data)), nil
	}

	cfg := config.Load()
	proxyURI := firstNonEmpty(cfg.ActiveNodeURI, cfg.ProxyURL)
	if proxyURI == "" || s.vc == nil || s.vc.Net() == nil {
		return "", err
	}

	log.Printf("[Admin] [FetchSub] direct fetch failed, retry via proxy: %v", err)
	data, proxyErr := fetchSubscriptionDataViaProxy(ctx, s.vc.Net(), rawURL, proxyURI)
	if proxyErr != nil {
		return "", fmt.Errorf("direct fetch failed: %v; proxy retry failed: %w", err, proxyErr)
	}

	log.Printf("[Admin] [FetchSub] proxy retry succeeded")
	return strings.TrimSpace(string(data)), nil
}

func fetchSubscriptionDataDirect(ctx context.Context, rawURL string) ([]byte, error) {
	client := netx.NewHTTPClient(30 * time.Second)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	req.Header.Set("User-Agent", subscriptionFetchUserAgent)
	req.Header.Set("Accept", "*/*")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	if resp == nil {
		return nil, fmt.Errorf("nil response received")
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}
	return data, nil
}

func fetchSubscriptionDataViaProxy(ctx context.Context, netClient *transport.NetworkClient, rawURL string, proxyURI string) ([]byte, error) {
	if netClient == nil {
		return nil, errors.New("network client unavailable")
	}

	sess, err := netClient.CreateSession(30, proxyURI, "admin-fetch-sub")
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	defer sess.Close()

	header := transport.Header{
		"user-agent": {subscriptionFetchUserAgent},
		"accept":     {"*/*"},
	}
	statusCode, data, err := sess.DoAndRead(ctx, http.MethodGet, rawURL, header, nil)
	if err != nil {
		return nil, fmt.Errorf("error: %w", err)

	}
	if statusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", statusCode)
	}
	return data, nil
}
