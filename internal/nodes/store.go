// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package nodes

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"math/rand"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/db"
)

type Node struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	RawURI   string `json:"raw_uri"`
	Disabled bool   `json:"disabled"`
}

type NodeHealth struct { //nolint:govet
	SuccessCount        int     `json:"success_count"`
	FailCount           int     `json:"fail_count"`
	ConsecutiveFailures int     `json:"consecutive_failures"`
	LastTestMs          float64 `json:"last_test_ms"`
	LastTestError       string  `json:"last_test_error"`
	LastSuccessAt       int64   `json:"last_success_at"`
	LastFailAt          int64   `json:"last_fail_at"`
	CooldownUntil       int64   `json:"cooldown_until"`
}

var (
	mu                 sync.Mutex                     //nolint:gochecknoglobals
	nodeList           []Node                         //nolint:gochecknoglobals
	healthMap          = make(map[string]*NodeHealth) //nolint:gochecknoglobals
	loaded             bool                           //nolint:gochecknoglobals
	DeleteNodeCallback func(uri string)               //nolint:gochecknoglobals
)

func ensureLoaded() {
	if loaded {
		return
	}
	loaded = true

	if db.GlobalDB == nil {
		return
	}

	// Load nodes
	rows, err := db.GlobalDB.Query("SELECT raw_uri, type, name, disabled FROM nodes")
	if err == nil {
		defer func() {
			_ = rows.Close()
		}()
		nodes := []Node{}
		for rows.Next() {
			var n Node
			if err := rows.Scan(&n.RawURI, &n.Type, &n.Name, &n.Disabled); err == nil {
				nodes = append(nodes, n)
			}
		}
		nodeList = nodes
	}

	// Load health
	hRows, err := db.GlobalDB.Query("SELECT raw_uri, success_count, fail_count, consecutive_failures, last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until FROM node_health")
	if err == nil {
		defer func() {
			_ = hRows.Close()
		}()
		for hRows.Next() {
			var uri string
			h := &NodeHealth{} //nolint:exhaustruct
			if err := hRows.Scan(&uri, &h.SuccessCount, &h.FailCount, &h.ConsecutiveFailures, &h.LastTestMs, &h.LastTestError, &h.LastSuccessAt, &h.LastFailAt, &h.CooldownUntil); err == nil {
				healthMap[uri] = h
			}
		}
	}
}

func LoadNodes() []Node {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	log.Printf("[Nodes] 获取所有节点 (数量: %d)", len(nodeList))
	return nodeList
}

func LoadHealth() map[string]*NodeHealth {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	return healthMap
}

// writeAtomicJSON has been removed because it is unused

func saveNodesUnsafe() {
	if db.GlobalDB == nil {
		return
	}
	tx, err := db.GlobalDB.Begin()
	if err != nil {
		return
	}
	// 为了简单起见，可以先全量删除再插入，但最好的方式是逐个插入或在添加删除时调用单个 SQL
	// 这里保持原来 saveNodesUnsafe 的全量保存语义，执行全量同步
	_, _ = tx.Exec("DELETE FROM nodes")
	stmt, _ := tx.Prepare("INSERT INTO nodes (raw_uri, type, name, disabled) VALUES (?, ?, ?, ?)")
	for _, n := range nodeList {
		if stmt != nil {
			_, _ = stmt.Exec(n.RawURI, n.Type, n.Name, n.Disabled)
		}
	}
	if stmt != nil {
		_ = stmt.Close()
	}
	_ = tx.Commit()
}

func saveHealthUnsafe() {
	if db.GlobalDB == nil {
		return
	}
	tx, err := db.GlobalDB.Begin()
	if err != nil {
		return
	}
	stmt, _ := tx.Prepare(`INSERT OR REPLACE INTO node_health 
		(raw_uri, success_count, fail_count, consecutive_failures, last_test_ms, last_test_error, last_success_at, last_fail_at, cooldown_until) 
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if stmt == nil {
		_ = tx.Rollback()
		return
	}
	for uri, h := range healthMap {
		_, _ = stmt.Exec(uri, h.SuccessCount, h.FailCount, h.ConsecutiveFailures, h.LastTestMs, h.LastTestError, h.LastSuccessAt, h.LastFailAt, h.CooldownUntil)
	}
	_ = stmt.Close()
	_ = tx.Commit()
}

func MergeNodes(newNodes []Node) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	existing := make(map[string]bool)
	for _, n := range nodeList {
		existing[n.RawURI] = true
	}
	for _, n := range newNodes {
		if !existing[n.RawURI] {
			nodeList = append(nodeList, n)
			existing[n.RawURI] = true
		}
	}
	saveNodesUnsafe()
}

func DeleteNode(uri string) {
	mu.Lock()
	ensureLoaded()
	var kept []Node
	for _, n := range nodeList {
		if n.RawURI != uri {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	delete(healthMap, uri)
	saveNodesUnsafe()
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 必须先解锁，避免底层的销毁回调查找节点名称时发生死锁
	if cb != nil {
		cb(uri)
	}
}

func DedupNodes() int {
	mu.Lock()
	ensureLoaded()
	keepMap := make(map[string]bool)
	var kept []Node
	removed := 0
	var removedURIs []string
	for _, n := range nodeList {
		key := n.RawURI
		if scheme, userinfo, host, port, ok := parseNodeIdentity(n.RawURI); ok {
			key = scheme + "://" + userinfo + "@" + host + ":" + strconv.Itoa(port)
		}
		if !keepMap[key] {
			keepMap[key] = true
			kept = append(kept, n)
		} else {
			removed++
			removedURIs = append(removedURIs, n.RawURI)
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 先解锁再通知销毁连接池

	if cb != nil {
		for _, u := range removedURIs {
			cb(u)
		}
	}
	return removed
}

func DeleteDisabled() int {
	mu.Lock()
	ensureLoaded()
	var kept []Node
	removed := 0
	var removedURIs []string
	for _, n := range nodeList {
		if !n.Disabled {
			kept = append(kept, n)
		} else {
			removed++
			removedURIs = append(removedURIs, n.RawURI)
			delete(healthMap, n.RawURI)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock()

	if cb != nil {
		for _, u := range removedURIs {
			cb(u)
		}
	}
	return removed
}

func BatchUpdateNodesDisabled(uris []string, disabled bool) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
	}
	for i, n := range nodeList {
		if targets[n.RawURI] {
			nodeList[i].Disabled = disabled
		}
	}
	saveNodesUnsafe()
}

func BatchDeleteNodes(uris []string) {
	mu.Lock()
	ensureLoaded()
	targets := make(map[string]bool)
	for _, u := range uris {
		targets[u] = true
		delete(healthMap, u)
	}
	var kept []Node
	for _, n := range nodeList {
		if !targets[n.RawURI] {
			kept = append(kept, n)
		}
	}
	nodeList = kept
	saveNodesUnsafe()
	saveHealthUnsafe()
	cb := DeleteNodeCallback
	mu.Unlock() // 防止在批量删除时引发卡死死锁

	if cb != nil {
		for _, u := range uris {
			cb(u)
		}
	}
}

func SortNodesByLatency() {
	mu.Lock()
	ensureLoaded()

	sort.Slice(nodeList, func(i, j int) bool {
		n1 := nodeList[i]
		n2 := nodeList[j]

		// 禁用的排在最后面
		if n1.Disabled != n2.Disabled {
			return !n1.Disabled
		}

		h1 := healthMap[n1.RawURI]
		h2 := healthMap[n2.RawURI]

		val1 := math.MaxFloat64
		if h1 != nil {
			if h1.ConsecutiveFailures > 0 {
				val1 = 1e6 + float64(h1.ConsecutiveFailures)*1000
			} else if h1.LastTestMs > 0 {
				val1 = h1.LastTestMs
			}
		}

		val2 := math.MaxFloat64
		if h2 != nil {
			if h2.ConsecutiveFailures > 0 {
				val2 = 1e6 + float64(h2.ConsecutiveFailures)*1000
			} else if h2.LastTestMs > 0 {
				val2 = h2.LastTestMs
			}
		}

		// 延迟一致的按名字自然排序
		if val1 == val2 {
			return n1.Name < n2.Name
		}
		return val1 < val2
	})

	saveNodesUnsafe()
	mu.Unlock()
}

func GetNodeName(uri string) string {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	for _, n := range nodeList {
		if n.RawURI == uri {
			return n.Name
		}
	}
	return "Unknown"
}

func EnableNode(uri string) bool {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	found := false
	for i, n := range nodeList {
		if n.RawURI == uri {
			nodeList[i].Disabled = false
			if h, exists := healthMap[uri]; exists {
				h.CooldownUntil = 0
			}
			found = true
			break
		}
	}
	if found {
		saveNodesUnsafe()
		saveHealthUnsafe()
	}
	return found
}

func padB64(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "-", "+"), "_", "/")
	if pad := len(s) % 4; pad != 0 {
		s += strings.Repeat("=", 4-pad)
	}
	return s
}

func parseNodeIdentity(rawURI string) (scheme, userinfo, host string, port int, ok bool) {
	if strings.HasPrefix(rawURI, "vmess://") {
		b64Str := rawURI[8:]
		if idx := strings.Index(b64Str, "?"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		if idx := strings.Index(b64Str, "#"); idx != -1 {
			b64Str = b64Str[:idx]
		}
		b64Str = padB64(b64Str)
		if b, err := base64.StdEncoding.DecodeString(b64Str); err == nil {
			var d map[string]any
			if err := json.Unmarshal(b, &d); err == nil {
				id, _ := d["id"].(string)
				add, _ := d["add"].(string)
				portStr := fmt.Sprintf("%v", d["port"])
				p, _ := strconv.Atoi(portStr)
				return "vmess", id, add, p, true
			}
		}
		return "", "", "", 0, false
	}
	if strings.HasPrefix(rawURI, "ss://") {
		body := rawURI[5:]
		if idx := strings.Index(body, "#"); idx != -1 {
			body = body[:idx]
		}
		if idx := strings.Index(body, "@"); idx != -1 {
			b, err := base64.StdEncoding.DecodeString(padB64(body[:idx]))
			if err == nil {
				parts := strings.SplitN(string(b), ":", 2)
				if len(parts) >= 2 {
					hp := strings.Split(body[idx+1:], ":")
					if len(hp) >= 2 {
						p, _ := strconv.Atoi(hp[1])
						return "ss", parts[0] + ":" + parts[1], hp[0], p, true
					}
				}
			}
		}
		return "", "", "", 0, false
	}
	u, err := url.Parse(rawURI)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", "", "", 0, false
	}
	scheme = u.Scheme
	userinfo = ""
	if u.User != nil {
		userinfo = u.User.Username()
	}
	host = u.Hostname()
	port, _ = strconv.Atoi(u.Port())
	if port == 0 {
		port = 443
	}
	return scheme, userinfo, host, port, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func RecordTest(uri string, ok bool, ms float64, errStr string) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	h, exists := healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		healthMap[uri] = h
	}
	h.LastTestMs = ms
	h.LastTestError = errStr
	if ok {
		h.SuccessCount++
		h.ConsecutiveFailures = 0
		h.LastSuccessAt = time.Now().Unix()
		h.CooldownUntil = 0
	} else {
		h.FailCount++
		h.ConsecutiveFailures++
		h.LastFailAt = time.Now().Unix()
		failures := maxInt(1, h.ConsecutiveFailures)
		cooldown := minInt(1800, 30*(1<<minInt(failures-1, 6)))
		h.CooldownUntil = time.Now().Unix() + int64(cooldown)
	}
	saveNodesUnsafe()
	saveHealthUnsafe()
}

func UpdateNodeTestResult(uri string, ok bool, ms float64, errStr string) {
	RecordTest(uri, ok, ms, errStr)
}

// RecordRateLimit 业务流 429 专属软降温函数：只执行短冷却，保留原评分分数不破坏
func RecordRateLimit(uri string, cooldownSec int) {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	h, exists := healthMap[uri]
	if !exists {
		h = &NodeHealth{} //nolint:exhaustruct
		healthMap[uri] = h
	}
	h.CooldownUntil = time.Now().Unix() + int64(cooldownSec)
	h.LastTestError = "429 Rate Limit"
	h.LastFailAt = time.Now().Unix()
	saveNodesUnsafe()
	saveHealthUnsafe()
}

type scoredNode struct {
	node  Node
	score float64
}

func SelectForParallel(k int) []Node {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	now := time.Now().Unix()

	// 历史衰减机制：对积压了过多历史分数的节点执行定期折半衰减，使评分更易受近期波动响应
	decayed := false
	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil {
			if h.SuccessCount > 1000 || h.FailCount > 200 {
				h.SuccessCount /= 2
				h.FailCount /= 2
				decayed = true
			}
		}
	}
	if decayed {
		saveHealthUnsafe()
	}

	var scored []scoredNode
	var cooldownNodes []scoredNode
	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil && h.CooldownUntil > now {
			cooldownNodes = append(cooldownNodes, scoredNode{n, float64(h.CooldownUntil)})
			continue
		}
		score := 100.0
		if h != nil {
			score += math.Min(float64(h.SuccessCount), 100) * 3
			score -= math.Min(float64(h.FailCount), 100) * 4
			score -= float64(h.ConsecutiveFailures) * 25
			if h.LastTestMs > 0 {
				score -= math.Min(h.LastTestMs/1000.0, 30.0)
			}
			lastSeen := maxInt64(h.LastSuccessAt, h.LastFailAt)
			if lastSeen == 0 {
				score += 20
			} else if now-lastSeen > 3600 {
				score += 10
			}
		} else {
			score += 20
		}
		scored = append(scored, scoredNode{n, math.Max(1.0, score)})
	}
	sort.Slice(scored, func(i, j int) bool { return scored[i].score > scored[j].score })
	if len(scored) < k && len(cooldownNodes) > 0 {
		sort.Slice(cooldownNodes, func(i, j int) bool { return cooldownNodes[i].score < cooldownNodes[j].score })
		needed := k - len(scored)
		if needed > len(cooldownNodes) {
			needed = len(cooldownNodes)
		}
		scored = append(scored, cooldownNodes[:needed]...)
	}
	topK := config.Load().ParallelNodeTopK
	if topK <= 0 {
		topK = 80
	}
	if len(scored) > topK {
		scored = scored[:topK]
	}
	weights := make([]float64, len(scored))
	totalWeight := 0.0
	const tau = 40.0 // Boltzmann temperature parameter
	for i, s := range scored {
		w := math.Exp(s.score / tau)
		if math.IsInf(w, 0) || math.IsNaN(w) {
			w = 1.0
		}
		weights[i] = w
		totalWeight += w
	}
	var selected []Node
	for i := 0; i < k && len(scored) > 0; i++ {
		r := rand.Float64() * totalWeight
		idx := len(weights) - 1
		for j, w := range weights {
			r -= w
			if r <= 0 {
				idx = j
				break
			}
		}
		selected = append(selected, scored[idx].node)
		totalWeight -= weights[idx]
		weights = append(weights[:idx], weights[idx+1:]...)
		scored = append(scored[:idx], scored[idx+1:]...)
	}

	if config.Load().DebugMode {
		log.Printf("[Nodes] 选择并行节点 (需求: %d, 实际: %d)", k, len(selected))
	}
	return selected
}

func GetAverageLatency() float64 {
	mu.Lock()
	defer mu.Unlock()
	ensureLoaded()
	var sum float64
	var count int
	for _, n := range nodeList {
		if n.Disabled {
			continue
		}
		h := healthMap[n.RawURI]
		if h != nil && h.LastTestMs > 0 && h.CooldownUntil <= time.Now().Unix() {
			sum += h.LastTestMs
			count++
		}
	}
	if count == 0 {
		return 500.0
	}
	return sum / float64(count)
}
