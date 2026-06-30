// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

// Package metrics 定义进程级可观测采集器的类型与接口。
//
// 采集器以空操作形态提供——计数方法直接返回、Snapshot 返回零值。热路径上的调用方
// （IncUpstream*/StartRequest/EndRequest/RecordRequest 等）无需任何分支判断即可直接
// 调用，零额外开销；需要详细运行指标的部署可在此基础上接入具体采集后端。
package metrics

// Collector 是采集器。当前无字段——所有方法均为空操作。
type Collector struct{}

// RequestRecord 描述单条请求记录的字段结构。
type RequestRecord struct {
	Time    string  `json:"time"`
	Path    string  `json:"path"`
	Success bool    `json:"success"`
	Latency float64 `json:"latency"`
}

// Snapshot 是一次指标快照的字段结构。
type Snapshot struct {
	StartUnix      int64   `json:"start_unix"`
	Total          int64   `json:"total"`
	Success        int64   `json:"success"`
	Fail           int64   `json:"fail"`
	Active         int64   `json:"active"`
	SuccessRate    float64 `json:"success_rate"`
	Upstream429    int64   `json:"upstream_429"`
	UpstreamEmpty  int64   `json:"upstream_empty"`
	UpstreamAuth   int64   `json:"upstream_auth_fail"`
	LatencyP50     float64 `json:"latency_p50_sec"`
	LatencyP95     float64 `json:"latency_p95_sec"`
	LatencyP99     float64 `json:"latency_p99_sec"`
	LatencySamples int     `json:"latency_samples"`
}

// Default 是进程级全局采集器。
var Default = New(0) //nolint:gochecknoglobals

// New 构造采集器；maxLatency 为延迟采样窗口大小（当前实现未消费）。
func New(maxLatency int) *Collector { return &Collector{} }

// 以下方法为空操作 / 零值返回。

func (c *Collector) SetStart(unix int64)                                             {}
func (c *Collector) StartRequest()                                                   {}
func (c *Collector) EndRequest(success bool, latencySec float64)                     {}
func (c *Collector) IncUpstream429()                                                 {}
func (c *Collector) IncUpstreamEmpty()                                               {}
func (c *Collector) IncUpstreamAuth()                                                {}
func (c *Collector) Snapshot() Snapshot                                              { return Snapshot{} } //nolint:exhaustruct
func (c *Collector) Reset()                                                          {}
func (c *Collector) RecordRequest(path string, success bool, lat float64, at string) {}
func (c *Collector) RecentRequests() []RequestRecord                                 { return nil }
