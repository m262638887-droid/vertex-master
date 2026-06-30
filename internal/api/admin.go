// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

// 本文件实现管理后台。
//
// 提供：会话登录鉴权、运行设置读写、API 密钥增删查、模型清单读写、自身指标/历史查看，
// 以及 go:embed 的静态前端面板（/admin）。鉴权独立于业务 API key——用一次性 session token
// 存 HttpOnly cookie，路由层把 /admin 与 /api/admin/ 排除在 withAPIKey 之外。
//
// 安全要点：密码用 crypto/subtle.ConstantTimeCompare 常量时间比较（防计时侧信道）；
// session token 用 crypto/rand 取 32 字节 hex；cookie 设 HttpOnly + SameSite=Lax + Path=/。
package api

import (
	cryptorand "crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/admin"
	"github.com/bsfdsagfadg/vertex/internal/config"
)

// ---- 会话 ----

const (
	adminCookieName = "admin_token"
	adminSessionTTL = 24 * time.Hour
)

// adminSessions 是 token → 过期时刻 的内存表。单进程服务用包级全局即可。
var (
	//nolint:gochecknoglobals // Session cache needs to be global for the process
	adminSessionsMu sync.Mutex
	//nolint:gochecknoglobals // Session cache needs to be global for the process
	adminSessions = map[string]time.Time{}
)

// issueAdminToken 生成一个新 session token（crypto/rand 32 字节 hex）并登记过期时刻。
func issueAdminToken() string {
	b := make([]byte, 32)
	_, _ = cryptorand.Read(b)
	tok := hex.EncodeToString(b)
	adminSessionsMu.Lock()
	adminSessions[tok] = time.Now().Add(adminSessionTTL)
	adminSessionsMu.Unlock()
	return tok
}

// checkAdminToken 校验 token 是否存在且未过期；过期则顺手删除。
func checkAdminToken(tok string) bool {
	if tok == "" {
		return false
	}
	adminSessionsMu.Lock()
	defer adminSessionsMu.Unlock()
	exp, ok := adminSessions[tok]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(adminSessions, tok)
		return false
	}
	return true
}

// dropAdminToken 删除某个 session（登出用）。
func dropAdminToken(tok string) {
	if tok == "" {
		return
	}
	adminSessionsMu.Lock()
	delete(adminSessions, tok)
	adminSessionsMu.Unlock()
}

// cleanupAdminSessions 清理所有已过期 token，返回清理数量。
func cleanupAdminSessions() int {
	now := time.Now()
	adminSessionsMu.Lock()
	defer adminSessionsMu.Unlock()
	n := 0
	for tok, exp := range adminSessions {
		if now.After(exp) {
			delete(adminSessions, tok)
			n++
		}
	}
	return n
}

// adminTokenFromRequest 取请求中的 admin token：cookie admin_token > Authorization: Bearer。
func adminTokenFromRequest(r *http.Request) string {
	if c, err := r.Cookie(adminCookieName); err == nil && c.Value != "" {
		return c.Value
	}
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
		return strings.TrimSpace(auth[7:])
	}
	return ""
}

// requireAdmin 报告请求是否携带有效的管理员会话。
func requireAdmin(r *http.Request) bool {
	return checkAdminToken(adminTokenFromRequest(r))
}

// StartAdminSessionCleanup 启动后台 goroutine，每 interval 清理一次过期 session。
// main 启动时调用一次即可；进程退出随之结束（无需显式 stop，内存表丢弃即可）。
func StartAdminSessionCleanup(interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for range t.C {
			if n := cleanupAdminSessions(); n > 0 {
				log.Printf("[Admin] 已清理 %d 个过期会话 token", n)
			}
		}
	}()
}

// ---- 启动期：确保有管理员密码 ----

// EnsureAdminPassword 确保存在管理员密码：config.admin_password 为空时用 crypto/rand 生成一个
// （base64url、约 12 字符），醒目打印一次，并写回 config.json 持久化。
func EnsureAdminPassword() {
	if strings.TrimSpace(config.Load().AdminPassword) != "" {
		return
	}
	b := make([]byte, 9) // 9 字节 → base64url 12 字符（无填充）
	if _, err := cryptorand.Read(b); err != nil {
		log.Printf("[Admin] 生成管理员密码失败：%v", err)
		return
	}
	pw := base64.RawURLEncoding.EncodeToString(b)
	if err := config.WriteSettings(map[string]any{"admin_password": pw}); err != nil {
		log.Printf("[Admin] 写入管理员密码到 config.json 失败：%v", err)
		return
	}
	bar := strings.Repeat("=", 60)
	log.Printf("%s", bar)
	log.Printf("[Admin] 首次启动，已自动生成管理员密码：")
	log.Printf("[Admin]     密码: %s", pw)
	log.Printf("[Admin]     访问: http://<host>:<port>/admin")
	log.Printf("[Admin]     密码已写入 config/config.json，登录后可在面板修改")
	log.Printf("%s", bar)
}

// ---- 路由分发 ----

// handleAdminAPI 是 /api/admin/ 子树的分发器：按 path + method 路由到各 handler。
// 除 login（公开）外的端点统一先过 requireAdmin。
func (s *Server) handleAdminAPI(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/api/admin")
	log.Printf("[Server] [AdminAPI] 收到请求: %s %s", r.Method, path)

	// login 和 check-auth 不需要已登录。
	if path == "/login" {
		s.adminLogin(w, r)
		return
	}
	if path == "/check-auth" {
		s.adminCheckAuth(w, r)
		return
	}

	// 删除密钥：DELETE /keys/{name}，name 作路径参数。
	if strings.HasPrefix(path, "/keys/") {
		if !requireAdmin(r) {
			s.adminUnauthorized(w)
			return
		}
		s.adminDeleteKey(w, r, strings.TrimPrefix(path, "/keys/"))
		return
	}

	if !requireAdmin(r) {
		s.adminUnauthorized(w)
		return
	}

	switch path {
	case "/nodes":
		switch r.Method {
		case http.MethodGet:
			s.adminGetNodes(w, r)
		case http.MethodDelete:
			s.adminDeleteNode(w, r)
		}
		return
	case "/nodes/test":
		s.adminTestNode(w, r)
		return
	case "/nodes/enable":
		s.adminEnableNode(w, r)
		return
	case "/nodes/test-all":
		s.adminTestAll(w, r)
		return
	case "/nodes/deduplicate":
		s.adminDedupNodes(w, r)
		return
	case "/nodes/disabled":
		s.adminDeleteDisabledNodes(w, r)
		return
	case "/nodes/import":
		s.adminImportNodes(w, r)
		return
	case "/nodes/import-json":
		s.adminImportNodesJson(w, r)
		return
	case "/subscriptions/fetch":
		s.adminFetchSub(w, r)
		return
	case "/use-node":
		s.adminUseNode(w, r)
		return
	case "/nodes/batch-disable":
		s.adminBatchDisableNodes(w, r)
		return
	case "/nodes/batch-enable":
		s.adminBatchEnableNodes(w, r)
		return
	case "/nodes/batch-delete":
		s.adminBatchDeleteNodes(w, r)
		return
	case "/nodes/sort":
		s.adminSortNodesByLatency(w, r)
		return
	case "/upload-bg":
		s.adminUploadBg(w, r)
		return
	case "/delete-bg":
		s.adminDeleteBg(w, r)
		return
	case "/list-bgs":
		if r.Method != http.MethodGet {
			s.adminMethodNotAllowed(w)
			return
		}
		s.adminListBgs(w, r)
		return
	}

	switch path {
	case "/logout":
		s.adminLogout(w, r)
	case "/settings":
		switch r.Method {
		case http.MethodGet:
			s.adminGetSettings(w, r)
		case http.MethodPut:
			s.adminPutSettings(w, r)
		default:
			s.adminMethodNotAllowed(w)
		}
	case "/stats":
		s.adminGetStats(w, r)
	case "/stats/reset":
		s.adminResetStats(w, r)
	case "/log":
		if r.Method != http.MethodGet {
			s.adminMethodNotAllowed(w)
			return
		}
		s.adminGetLog(w, r)
	case "/history":
		s.adminGetHistory(w, r)
	case "/keys":
		switch r.Method {
		case http.MethodGet:
			s.adminGetKeys(w, r)
		case http.MethodPost:
			s.adminAddKey(w, r)
		default:
			s.adminMethodNotAllowed(w)
		}
	case "/models":
		switch r.Method {
		case http.MethodGet:
			s.adminGetModels(w, r)
		case http.MethodPut:
			s.adminPutModels(w, r)
		default:
			s.adminMethodNotAllowed(w)
		}
	default:
		s.writeJSON(w, http.StatusNotFound, adminErr("未知接口 (not found)"))
	}
}

// ---- 端点：会话 ----

// adminLogin 处理 POST /api/admin/login：密码正确则签发 token + Set-Cookie，返回 {ok:true}。
func (s *Server) adminLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.adminMethodNotAllowed(w)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	expected := strings.TrimSpace(config.Load().AdminPassword)
	if expected == "" {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("管理员密码未初始化 (admin password not set)"))
		return
	}
	// 常量时间比较，防计时侧信道泄漏密码长度/前缀匹配信息。
	if subtle.ConstantTimeCompare([]byte(strings.TrimSpace(body.Password)), []byte(expected)) != 1 {
		log.Printf("[Security] 警告：后台登录失败，密码错误。来源 IP: %s", r.RemoteAddr)
		s.writeJSON(w, http.StatusUnauthorized, adminErr("密码错误 (invalid password)"))
		return
	}
	log.Printf("[Admin] 管理后台登录成功。来源 IP: %s", r.RemoteAddr)
	cleanupAdminSessions() // 登录时顺手清过期，避免内存里堆死 token
	tok := issueAdminToken()
	http.SetCookie(w, &http.Cookie{ //nolint:exhaustruct
		Name:     adminCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   int(adminSessionTTL / time.Second),
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminLogout 处理 POST /api/admin/logout：删该 session + 清 cookie。
func (s *Server) adminLogout(w http.ResponseWriter, r *http.Request) {
	dropAdminToken(adminTokenFromRequest(r))
	http.SetCookie(w, &http.Cookie{ //nolint:exhaustruct
		Name:     adminCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   r.TLS != nil || r.Header.Get("X-Forwarded-Proto") == "https",
		MaxAge:   -1, // 立即过期
	})
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 端点：检查认证 ----

// adminCheckAuth 处理 GET /api/admin/check-auth：不返回 401，避免浏览器 DevTools 红色标记。
func (s *Server) adminCheckAuth(w http.ResponseWriter, r *http.Request) {
	authenticated := requireAdmin(r)
	cfg := config.Load()
	s.writeJSON(w, http.StatusOK, map[string]any{
		"authenticated":     authenticated,
		"background_image":  cfg.BackgroundImage,
		"font_size":         cfg.FontSize,
		"font_color_type":   cfg.FontColorType,
		"font_color":        cfg.FontColor,
		"custom_bg_presets": cfg.CustomBgPresets,
	})
}

// ---- 端点：设置 ----

// adminGetSettings 处理 GET /api/admin/settings：返回 {"settings": {...}}（前端读 d.settings）。
// 字段集对齐前端 SETTINGS_FIELDS。
func (s *Server) adminGetSettings(w http.ResponseWriter, _ *http.Request) {
	cfg := config.Load()
	telEnabled := true
	if cfg.TelemetryEnabled != nil {
		telEnabled = *cfg.TelemetryEnabled
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"settings": map[string]any{
		"max_retries":       cfg.MaxRetries,
		"token_pool_size":   cfg.TokenPoolSize,
		"max_spill_mb":      cfg.MaxSpillMB,
		"max_request_mb":    cfg.MaxRequestMB,
		"max_n":             cfg.MaxN,
		"anti429_enabled":   cfg.Anti429Enabled,
		"anti429_target":    cfg.Anti429Target,
		"force_no_stream":   cfg.ForceNoStream,
		"anti_tracking":     cfg.AntiTracking,
		"drop_max_tokens":   cfg.DropMaxTokens,
		"telemetry_enabled": telEnabled,
		"proxy_url":         cfg.ProxyURL, "parallel_pool_enabled": cfg.ParallelPoolEnabled, "parallel_pool_size": cfg.ParallelPoolSize, "active_node_uri": cfg.ActiveNodeURI,
		"parallel_pool_delay_dynamic": cfg.ParallelPoolDelayDynamic,
		"parallel_pool_delay_ms":      cfg.ParallelPoolDelayMs,
		"recaptcha_expire_seconds":    cfg.RecaptchaExpireSeconds,
		"sticky_pool_enabled":         cfg.StickyPoolEnabled,
		"parallel_pool_retry_enabled": cfg.ParallelPoolRetryEnabled,
		"background_image":            cfg.BackgroundImage,
		"font_size":                   cfg.FontSize,
		"font_color_type":             cfg.FontColorType,
		"font_color":                  cfg.FontColor,
		"custom_bg_presets":           cfg.CustomBgPresets,
	}})
}

// adminAllowedSettings 是面板可写回 config.json 的字段白名单（避免前端塞任意键污染配置）。
//
//nolint:gochecknoglobals // Read-only configuration map
var adminAllowedSettings = map[string]bool{
	"max_retries": true, "token_pool_size": true, "max_spill_mb": true,
	"max_request_mb": true, "max_n": true, "anti429_enabled": true,
	"anti429_target": true, "force_no_stream": true, "anti_tracking": true,
	"drop_max_tokens": true, "proxy_url": true, "admin_password": true,
	"parallel_pool_enabled": true, "parallel_pool_size": true,
	"telemetry_enabled":           true,
	"parallel_pool_delay_dynamic": true,
	"parallel_pool_delay_ms":      true,
	"recaptcha_expire_seconds":    true,
	"active_node_uri":             true,
	"sticky_pool_enabled":         true,
	"parallel_pool_retry_enabled": true,
	"background_image":            true,
	"font_size":                   true,
	"font_color_type":             true,
	"font_color":                  true,
	"custom_bg_presets":           true,
}

// adminPutSettings 处理 PUT /api/admin/settings：合并 {settings:{...}} 写回 config.json 并清缓存。
func (s *Server) adminPutSettings(w http.ResponseWriter, r *http.Request) {
	//nolint:govet // Intentionally unaligned
	var body struct {
		Settings map[string]any `json:"settings"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	updates := map[string]any{}
	for k, v := range body.Settings {
		if !adminAllowedSettings[k] {
			continue
		}
		// 数字字段：前端 number 输入可能传 float64，强类型字段需为整数，统一收敛成 int。
		switch k {
		case "max_retries", "token_pool_size", "max_spill_mb", "max_request_mb", "max_n", "parallel_pool_size", "parallel_pool_delay_ms", "recaptcha_expire_seconds":
			if f, ok := v.(float64); ok {
				updates[k] = int(f)
				continue
			}
		case "admin_password":
			// 空密码不允许（避免误把密码清空导致无法登录）。存 TrimSpace 后的值，
			// 与登录端/EnsureAdminPassword 的 trim 对称，避免"存了带空格、登录比 trim 后值"的不一致。
			if pw, ok := v.(string); !ok || strings.TrimSpace(pw) == "" {
				continue
			} else {
				updates[k] = strings.TrimSpace(pw)
				continue
			}
		}
		updates[k] = v
	}
	if err := config.WriteSettings(updates); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("写入配置失败 (failed to write config)"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 端点：统计 / 历史 ----

// adminGetStats 处理 GET /api/admin/stats：与 handleMetrics 完全相同的结构（前端复用同一渲染）。
func (s *Server) adminGetStats(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, s.metricsBody())
}

// adminResetStats 处理 POST /api/admin/stats/reset：清零指标（保留 uptime）。
func (s *Server) adminResetStats(w http.ResponseWriter, _ *http.Request) {
	s.metrics.Reset()
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// adminGetHistory 处理 GET /api/admin/history：返回最近请求列表 {"history":[{time,path,success,latency}]}。
func (s *Server) adminGetHistory(w http.ResponseWriter, _ *http.Request) {
	recs := s.metrics.RecentRequests()
	history := make([]any, 0, len(recs))
	for _, rec := range recs {
		history = append(history, map[string]any{
			"time": rec.Time, "path": rec.Path, "success": rec.Success, "latency": rec.Latency,
		})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"history": history})
}

// ---- 端点：密钥 ----

// adminGetKeys 处理 GET /api/admin/keys：返回脱敏密钥列表（不回明文 key）。
func (s *Server) adminGetKeys(w http.ResponseWriter, _ *http.Request) {
	entries, err := s.keys.List()
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("读取密钥失败 (failed to read keys)"))
		return
	}
	out := make([]any, 0, len(entries))
	for _, e := range entries {
		out = append(out, map[string]any{"name": e.Name, "key": e.Key, "key_masked": maskKey(e.Key)})
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"keys": out})
}

// adminAddKey 处理 POST /api/admin/keys：{name,key}，key 空则自动生成 sk- 随机串；持久化后重载。
func (s *Server) adminAddKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Key         string `json:"key"`
		Description string `json:"description"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	name := strings.TrimSpace(body.Name)
	key := strings.TrimSpace(body.Key)
	if name == "" {
		s.writeJSON(w, http.StatusBadRequest, adminErr("名称不能为空 (name is required)"))
		return
	}
	if strings.Contains(name, ":") {
		s.writeJSON(w, http.StatusBadRequest, adminErr("名称不能包含冒号 (name must not contain ':')"))
		return
	}
	if key == "" {
		key = generateAPIKey()
	} else if !strings.HasPrefix(key, "sk-") {
		s.writeJSON(w, http.StatusBadRequest, adminErr("密钥必须以 sk- 开头 (key must start with 'sk-')"))
		return
	}
	if err := s.keys.Add(name, key, body.Description); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("写入密钥失败 (failed to write keys)"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "key": key})
}

// adminDeleteKey 处理 DELETE /api/admin/keys/{name}：删除并持久化；未找到返回 404。
func (s *Server) adminDeleteKey(w http.ResponseWriter, r *http.Request, rawName string) {
	if r.Method != http.MethodDelete {
		s.adminMethodNotAllowed(w)
		return
	}
	name := rawName
	if dec, err := url.PathUnescape(rawName); err == nil {
		name = dec
	}
	ok, err := s.keys.Delete(name)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("删除密钥失败 (failed to delete key)"))
		return
	}
	if !ok {
		s.writeJSON(w, http.StatusNotFound, adminErr("未找到该密钥 (key not found)"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 端点：模型 ----

// adminGetModels 处理 GET /api/admin/models：返回 {"models":[...], "alias_map":{...}}。
func (s *Server) adminGetModels(w http.ResponseWriter, _ *http.Request) {
	s.writeJSON(w, http.StatusOK, map[string]any{
		"models":    config.BaseModels(),
		"alias_map": config.AliasMap(),
	})
}

// adminPutModels 处理 PUT /api/admin/models：{models,alias_map} 写回 models.json 并热重载。
func (s *Server) adminPutModels(w http.ResponseWriter, r *http.Request) {
	var body struct { //nolint:govet
		Models   []string          `json:"models"`
		AliasMap map[string]string `json:"alias_map"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	cleaned := make([]string, 0, len(body.Models))
	for _, m := range body.Models {
		if m = strings.TrimSpace(m); m != "" {
			cleaned = append(cleaned, m)
		}
	}
	if len(cleaned) == 0 {
		s.writeJSON(w, http.StatusBadRequest, adminErr("模型列表不能为空 (models must not be empty)"))
		return
	}
	alias := map[string]string{}
	for k, v := range body.AliasMap {
		if k, v = strings.TrimSpace(k), strings.TrimSpace(v); k != "" && v != "" {
			alias[k] = v
		}
	}
	if err := config.WriteModels(cleaned, alias); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("写入模型失败 (failed to write models)"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- 静态前端 ----

// handleAdminPage 服务 /admin 与 /admin/ 子路径：
//   - GET /admin           → 302 到 /admin/（使前端相对路径的静态资源解析到 /admin/ 下）
//   - GET /admin/          → admin.html
//   - GET /admin/{file}    → assets 下的静态文件（如图片资源）
func (s *Server) handleAdminPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/admin" {
		http.Redirect(w, r, "/admin/", http.StatusFound)
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/admin/")
	if name == "" {
		name = "admin.html"
	}
	data, err := fs.ReadFile(admin.Assets, "assets/"+name)
	if err != nil {
		s.oaiError(w, http.StatusNotFound, "not found", "invalid_request_error")
		return
	}
	w.Header().Set("Content-Type", contentTypeFor(name))
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// contentTypeFor 据文件名后缀返回 Content-Type（只覆盖 admin 资源用到的几种）。
func contentTypeFor(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".jpg"), strings.HasSuffix(name, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(name, ".png"):
		return "image/png"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".js"):
		return "text/javascript; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

// ---- 小工具 ----

// maskKey 把密钥脱敏成 sk-····后4位（不回明文）。短于 4 位的整段打码。
func maskKey(key string) string {
	if len(key) <= 4 {
		return "sk-····"
	}
	return "sk-····" + key[len(key)-4:]
}

// generateAPIKey 生成一个 sk- 前缀的随机密钥（crypto/rand 24 字节 hex）。
func generateAPIKey() string {
	b := make([]byte, 24)
	_, _ = cryptorand.Read(b)
	return "sk-" + hex.EncodeToString(b)
}

// adminErr 构造统一的 admin 错误响应体（前端 api() 读 body.error.message 展示）。
func adminErr(msg string) map[string]any {
	return map[string]any{"error": map[string]any{"message": msg}}
}

// adminUnauthorized 返回 401（前端 api() 据此弹回登录页）。
func (s *Server) adminUnauthorized(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusUnauthorized, adminErr("未登录或会话已过期 (unauthorized)"))
}

// adminMethodNotAllowed 返回 405。
func (s *Server) adminMethodNotAllowed(w http.ResponseWriter) {
	s.writeJSON(w, http.StatusMethodNotAllowed, adminErr("方法不允许 (method not allowed)"))
}

// decodeAdminBody 解析 JSON 请求体到 dst；失败时写 400 并返回 false。
func (s *Server) decodeAdminBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if r.Body == nil {
		s.writeJSON(w, http.StatusBadRequest, adminErr("请求体为空 (empty body)"))
		return false
	}
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		s.writeJSON(w, http.StatusBadRequest, adminErr("请求格式错误 (invalid JSON)"))
		return false
	}
	return true
}

func (s *Server) adminUploadBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		s.adminMethodNotAllowed(w)
		return
	}

	err := r.ParseMultipartForm(10 << 20) // 10MB limit
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, adminErr("解析上传文件失败 (parse error)"))
		return
	}

	file, _, err := r.FormFile("file")
	if err != nil {
		s.writeJSON(w, http.StatusBadRequest, adminErr("未找到文件字段 (missing file)"))
		return
	}
	defer file.Close()

	assetsDir := filepath.Join(filepath.Dir(config.ConfigDir()), "assets")
	_ = os.MkdirAll(assetsDir, 0o755)

	filename := fmt.Sprintf("background%d.jpg", time.Now().UnixMilli())
	targetPath := filepath.Join(assetsDir, filename)

	out, err := os.Create(targetPath)
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("无法保存文件 (create error)"))
		return
	}
	defer out.Close()

	if _, err = io.Copy(out, file); err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("保存文件失败 (copy error)"))
		return
	}

	bgURL := "url('/assets/" + filename + "')"
	err = config.WriteSettings(map[string]any{"background_image": bgURL})
	if err != nil {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("更新配置失败 (save config error)"))
		return
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "url": bgURL})
}

func (s *Server) adminDeleteBg(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete && r.Method != http.MethodPost {
		s.adminMethodNotAllowed(w)
		return
	}
	var body struct {
		Filename string `json:"filename"`
	}
	if !s.decodeAdminBody(w, r, &body) {
		return
	}
	if body.Filename == "" || strings.Contains(body.Filename, "/") || strings.Contains(body.Filename, "\\") {
		s.writeJSON(w, http.StatusBadRequest, adminErr("文件名无效"))
		return
	}
	if !strings.HasPrefix(body.Filename, "background") {
		s.writeJSON(w, http.StatusForbidden, adminErr("无权删除该文件"))
		return
	}

	assetsDir := filepath.Join(filepath.Dir(config.ConfigDir()), "assets")
	targetPath := filepath.Join(assetsDir, body.Filename)
	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		s.writeJSON(w, http.StatusInternalServerError, adminErr("删除文件失败"))
		return
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) adminListBgs(w http.ResponseWriter, r *http.Request) {
	assetsDir := filepath.Join(filepath.Dir(config.ConfigDir()), "assets")
	files, err := os.ReadDir(assetsDir)
	if err != nil {
		s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": []string{}})
		return
	}

	var bgs []string
	for _, f := range files {
		if !f.IsDir() && strings.HasPrefix(f.Name(), "background") {
			bgs = append(bgs, f.Name())
		}
	}
	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "files": bgs})
}

// adminGetLog 处理 GET /api/admin/log：读取最新的日志文件并返回。
func (s *Server) adminGetLog(w http.ResponseWriter, r *http.Request) {
	logPath := filepath.Join(filepath.Dir(config.ConfigDir()), "logs", "logs_latest.log")

	// Check if file exists, if not, try to fall back to current directory logs
	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		logPath = "logs/logs_latest.log"
	}

	data, err := os.ReadFile(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": ""})
			return
		}
		s.writeJSON(w, http.StatusInternalServerError, adminErr("无法读取日志文件 (read error)"))
		return
	}

	// 限制返回大小，避免日志过大撑爆内存或前端
	const maxLogSize = 200 * 1024
	if len(data) > maxLogSize {
		data = data[len(data)-maxLogSize:]
	}

	s.writeJSON(w, http.StatusOK, map[string]any{"ok": true, "content": string(data)})
}
