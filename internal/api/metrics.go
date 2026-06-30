// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package api

import (
	"runtime"

	"github.com/bsfdsagfadg/vertex/internal/spool"
)

// metricsBody 返回服务的实时状态，供 /metrics 与管理后台 stats 做存活探测。
func (s *Server) metricsBody() map[string]any {
	snap := s.metrics.Snapshot()

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	return map[string]any{
		"status": "ok",
		"memory": map[string]any{
			"alloc_mb":   bToMb(m.Alloc),
			"heap_inuse": bToMb(m.HeapInuse),
			"num_gc":     m.NumGC,
			"goroutines": runtime.NumGoroutine(),
			"spilled_mb": bToMb(uint64(spool.SpilledBytes())),
		},
		"requests": map[string]any{
			"total":          snap.Total,
			"success":        snap.Success,
			"fail":           snap.Fail,
			"active":         snap.Active,
			"success_rate":   snap.SuccessRate,
			"upstream_429":   snap.Upstream429,
			"upstream_empty": snap.UpstreamEmpty,
			"upstream_auth":  snap.UpstreamAuth,
		},
	}
}

func bToMb(b uint64) float64 {
	return float64(b) / 1024 / 1024
}
