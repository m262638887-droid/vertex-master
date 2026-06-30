package vertex

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsfdsagfadg/vertex/internal/cli"
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/nodes"
)

type stickyModeKey struct{}

func safeResetTimer(t *time.Timer, d time.Duration) {
	if !t.Stop() {
		select {
		case <-t.C:
		default:
		}
	}
	t.Reset(d)
}

func RunParallel[T any](ctx context.Context, cfg config.AppConfig, run func(context.Context, string) (T, error)) (T, error) {
	stickyPool := nodes.GetStickyPool()

	if cfg.StickyPoolEnabled && stickyPool.AvailableCount() > 0 {
		log.Printf("[Vertex] [RunParallel] 尝试粘性节点池 (%d 个可用)", stickyPool.AvailableCount())
		for {
			uri, ok := stickyPool.Acquire()
			if !ok {
				break
			}
			log.Printf("[Vertex] [RunParallel] 使用粘性节点: %s", nodes.GetNodeName(uri))
			ctxSticky := context.WithValue(ctx, stickyModeKey{}, true)
			val, err := run(ctxSticky, uri)
			if err == nil {
				stickyPool.Release(uri)
				return val, nil
			}
			log.Printf("[Vertex] [RunParallel] 粘性节点 %s 失败，逐出: %s", nodes.GetNodeName(uri), err.Error())
			stickyPool.Evict(uri)
		}
		log.Printf("[Vertex] [RunParallel] 粘性节点池耗尽，降级并行竞速")
	}

	cands := nodes.SelectForParallel(cfg.ParallelPoolSize)
	if cfg.StickyPoolEnabled {
		var filtered []nodes.Node
		for _, c := range cands {
			if !stickyPool.IsSticky(c.RawURI) {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}

	if !cfg.StickyPoolEnabled {
		stickyURIs := stickyPool.List()
		if len(stickyURIs) > 0 {
			stickySet := make(map[string]bool, len(stickyURIs))
			for _, u := range stickyURIs {
				stickySet[u] = true
			}
			var filtered []nodes.Node
			for _, c := range cands {
				if !stickySet[c.RawURI] {
					filtered = append(filtered, c)
				}
			}
			stickyNodes := make([]nodes.Node, 0, len(stickyURIs))
			for _, u := range stickyURIs {
				stickyNodes = append(stickyNodes, nodes.Node{RawURI: u, Name: nodes.GetNodeName(u)})
			}
			cands = append(stickyNodes, filtered...)
		}
	}

	if !cfg.ParallelPoolEnabled || len(cands) == 0 {
		proxy := cfg.ActiveNodeURI
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		log.Printf("[Vertex] [RunParallel] 降级为单节点运行: %s", nodes.GetNodeName(proxy))
		return run(ctx, proxy)
	}

	if cfg.DebugMode {
		log.Printf("[Vertex] [RunParallel] 开启对冲延迟竞速, %d 个节点参与", len(cands))
		for _, c := range cands {
			log.Printf("[Vertex] [RunParallel] 参与节点: %s", c.Name)
		}
	}

	cli.UpdateReqState(RequestIDFromContext(ctx), "⚡ 并发竞速", "\033[33m", fmt.Sprintf("并行节点: %d", len(cands)))

	ctxRace, cancel := context.WithCancel(ctx)

	type result struct { //nolint:govet
		uri string
		val T
		err error
	}

	resCh := make(chan result, len(cands)+20)
	var active int32
	activeKeys := make(map[string]bool)
	var mu sync.Mutex

	launchNode := func(uri string) {
		mu.Lock()
		if activeKeys[uri] {
			mu.Unlock()
			return
		}
		activeKeys[uri] = true
		mu.Unlock()

		atomic.AddInt32(&active, 1)
		go func(u string) {
			v, err := run(ctxRace, u)
			select {
			case resCh <- result{u, v, err}:
			case <-ctxRace.Done():
			}
		}(uri)
	}

	launchNode(cands[0].RawURI)

	delay := time.Duration(cfg.ParallelPoolDelayMs) * time.Millisecond
	if cfg.ParallelPoolDelayDynamic {
		delay = time.Duration(nodes.GetAverageLatency()) * time.Millisecond
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	nextIdx := 1
	var zero T

	for {
		select {
		case <-ctx.Done():
			cancel()
			return zero, ctx.Err() //nolint:wrapcheck

		case <-timer.C:
			if nextIdx < len(cands) {
				if cfg.DebugMode {
					log.Printf("[Racing] 对冲延迟唤醒，启动备份节点: %s", cands[nextIdx].Name)
				}
				launchNode(cands[nextIdx].RawURI)
				nextIdx++
				timer.Reset(delay)
			}

		case res := <-resCh:
			atomic.AddInt32(&active, -1)
			name := nodes.GetNodeName(res.uri)

			if res.err == nil {
				log.Printf("[Racing] 竞速胜出节点: %s", name)
				cli.UpdateReqWinner(RequestIDFromContext(ctx), name)
				cli.UpdateReqState(RequestIDFromContext(ctx), "🟢 数据传输", "\033[32m", "已建立连接")
				nodes.RecordTest(res.uri, true, 50, "")
				stickyPool.Add(res.uri)
				if cfg.DebugMode {
					log.Printf("[Racing] 胜出节点 %s 加入粘性池", name)
				}

				go func() {
					collectCtx, collectCancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer collectCancel()
					for {
						select {
						case bgRes := <-resCh:
							atomic.AddInt32(&active, -1)
							if bgRes.err == nil {
								log.Printf("[Racing] 后台收集: 节点 %s 加入粘性池", nodes.GetNodeName(bgRes.uri))
								stickyPool.Add(bgRes.uri)
							}
						case <-collectCtx.Done():
							cancel()
							return
						}
					}
				}()

				return res.val, nil
			}

			if res.err != context.Canceled && !errors.Is(res.err, context.Canceled) {
				if cfg.DebugMode {
					log.Printf("[Racing] 节点 %s 失败: %s", name, res.err.Error())
				}

				ve := asVertexError(res.err)
				if ve != nil && ve.Kind == "ratelimit" {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 触发 429 API 限制，进入 30 秒短时歇息", name)
					}
					nodes.RecordRateLimit(res.uri, 30)
				} else {
					nodes.RecordTest(res.uri, false, 0, res.err.Error())
				}

				if stickyPool.IsSticky(res.uri) {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 从粘性池逐出", name)
					}
					stickyPool.Evict(res.uri)
				}

				if ve != nil && !ve.IsRetryable() {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 触发不可重试的硬性错误，终止竞速", name)
					}
					cancel()
					return zero, res.err
				}

				if nextIdx < len(cands) {
					if cfg.DebugMode {
						log.Printf("[Racing] 竞速失败触发极速对冲接力...")
					}
					launchNode(cands[nextIdx].RawURI)
					nextIdx++
					safeResetTimer(timer, delay)
				}
			} else {
				if cfg.DebugMode {
					log.Printf("[Racing] 节点 %s 拨号取消", name)
				}
			}

			if atomic.LoadInt32(&active) == 0 && nextIdx >= len(cands) {
				cancel()
				if res.err != nil {
					return zero, res.err
				}
				return zero, fmt.Errorf("all nodes failed")
			}
		}
	}
}

func StreamParallel(ctx context.Context, cfg config.AppConfig, op func(ctx context.Context, proxyURI string) <-chan StreamChunk, yield func(StreamChunk) bool) {
	stickyPool := nodes.GetStickyPool()

	if cfg.StickyPoolEnabled {
		for {
			uri, ok := stickyPool.Acquire()
			if !ok {
				break
			}
			log.Printf("[Vertex] [StreamParallel] 使用粘性节点: %s", nodes.GetNodeName(uri))
			ctxSticky := context.WithValue(ctx, stickyModeKey{}, true)

			ch := op(ctxSticky, uri)
			first, ok := <-ch
			if !ok {
				log.Printf("[Vertex] [StreamParallel] 粘性节点 %s 流立即关闭，逐出", nodes.GetNodeName(uri))
				stickyPool.Evict(uri)
				continue
			}
			if first.Err != nil {
				log.Printf("[Vertex] [StreamParallel] 粘性节点 %s 重试耗尽，逐出: %s", nodes.GetNodeName(uri), first.Err.Message)
				stickyPool.Evict(uri)
				continue
			}

			stickyPool.Release(uri)
			if !yield(first) {
				return
			}
			for chunk := range ch {
				if !yield(chunk) {
					return
				}
			}
			return
		}
		log.Printf("[Vertex] [StreamParallel] 粘性节点池耗尽，降级并行竞速")
	}

	cands := nodes.SelectForParallel(cfg.ParallelPoolSize)
	if cfg.StickyPoolEnabled {
		var filtered []nodes.Node
		for _, c := range cands {
			if !stickyPool.IsSticky(c.RawURI) {
				filtered = append(filtered, c)
			}
		}
		cands = filtered
	}

	if !cfg.StickyPoolEnabled {
		stickyURIs := stickyPool.List()
		if len(stickyURIs) > 0 {
			stickySet := make(map[string]bool, len(stickyURIs))
			for _, u := range stickyURIs {
				stickySet[u] = true
			}
			var filtered []nodes.Node
			for _, c := range cands {
				if !stickySet[c.RawURI] {
					filtered = append(filtered, c)
				}
			}
			stickyNodes := make([]nodes.Node, 0, len(stickyURIs))
			for _, u := range stickyURIs {
				stickyNodes = append(stickyNodes, nodes.Node{RawURI: u, Name: nodes.GetNodeName(u)})
			}
			cands = append(stickyNodes, filtered...)
		}
	}

	if !cfg.ParallelPoolEnabled || len(cands) == 0 {
		proxy := cfg.ActiveNodeURI
		if proxy == "" {
			proxy = cfg.ProxyURL
		}
		log.Printf("[Vertex] [StreamParallel] 降级为单节点运行: %s", nodes.GetNodeName(proxy))
		for chunk := range op(ctx, proxy) {
			if !yield(chunk) {
				return
			}
		}
		return
	}

	if cfg.DebugMode {
		log.Printf("[Vertex] [StreamParallel] 开启对冲延迟流式竞速, %d 个节点参与", len(cands))
		for _, c := range cands {
			log.Printf("[Vertex] [StreamParallel] 参与节点: %s", c.Name)
		}
	}

	cli.UpdateReqState(RequestIDFromContext(ctx), "⚡ 并发竞速", "\033[33m", fmt.Sprintf("并行节点: %d", len(cands)))

	ctxRace, cancel := context.WithCancel(ctx)
	defer cancel()

	type res struct { //nolint:govet
		uri   string
		ch    <-chan StreamChunk
		first StreamChunk
		err   error
	}

	resCh := make(chan res, len(cands)+20)
	var active int32
	activeKeys := make(map[string]bool)
	var mu sync.Mutex

	launchNode := func(uri string) {
		mu.Lock()
		if activeKeys[uri] {
			mu.Unlock()
			return
		}
		activeKeys[uri] = true
		mu.Unlock()

		atomic.AddInt32(&active, 1)
		go func(u string) {
			ch := op(ctxRace, u)
			select {
			case first, ok := <-ch:
				if !ok {
					select {
					case resCh <- res{u, nil, StreamChunk{}, fmt.Errorf("stream closed")}:
					case <-ctxRace.Done():
					}
				} else if first.Err != nil {
					select {
					case resCh <- res{u, nil, StreamChunk{}, first.Err}:
					case <-ctxRace.Done():
					}
				} else {
					select {
					case resCh <- res{u, ch, first, nil}:
					case <-ctxRace.Done():
					}
				}
			case <-ctxRace.Done():
			}
		}(uri)
	}

	launchNode(cands[0].RawURI)

	delay := time.Duration(cfg.ParallelPoolDelayMs) * time.Millisecond
	if cfg.ParallelPoolDelayDynamic {
		delay = time.Duration(nodes.GetAverageLatency()) * time.Millisecond
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	candIdx := 1
	var winner *res

loop:
	for {
		select {
		case r := <-resCh:
			atomic.AddInt32(&active, -1)
			name := nodes.GetNodeName(r.uri)

			if r.err == nil {
				winner = &r
				log.Printf("[Racing] 竞速胜出节点: %s", name)
				cli.UpdateReqWinner(RequestIDFromContext(ctx), name)
				cli.UpdateReqState(RequestIDFromContext(ctx), "🟢 数据传输", "\033[32m", "已建立连接")
				nodes.RecordTest(r.uri, true, 50, "")
				stickyPool.Add(r.uri)
				if cfg.DebugMode {
					log.Printf("[Vertex] [StreamParallel] 胜出节点 %s 加入粘性池", name)
				}
				break loop
			} else if ctx.Err() == nil && r.err != context.Canceled && !errors.Is(r.err, context.Canceled) {
				if cfg.DebugMode {
					log.Printf("[Racing] 节点 %s 失败: %s", name, r.err.Error())
				}

				ve := asVertexError(r.err)
				if ve != nil && ve.Kind == "ratelimit" {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 触发 429 API 限制，进入 30 秒短时歇息", name)
					}
					nodes.RecordRateLimit(r.uri, 30)
				} else {
					nodes.RecordTest(r.uri, false, 0, r.err.Error())
				}

				if stickyPool.IsSticky(r.uri) {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 从粘性池逐出", name)
					}
					stickyPool.Evict(r.uri)
				}

				if ve != nil && !ve.IsRetryable() {
					if cfg.DebugMode {
						log.Printf("[Racing] 节点 %s 触发不可重试的硬性错误，终止竞速", name)
					}
					cancel()
					yield(StreamChunk{Err: ve})
					return
				}

				if candIdx < len(cands) {
					if cfg.DebugMode {
						log.Printf("[Racing] 竞速失败触发极速对冲接力...")
					}
					launchNode(cands[candIdx].RawURI)
					candIdx++
					safeResetTimer(timer, delay)
				}
			}

			if atomic.LoadInt32(&active) == 0 && candIdx >= len(cands) {
				break loop
			}

		case <-timer.C:
			if candIdx < len(cands) {
				if cfg.DebugMode {
					log.Printf("[Racing] 对冲延迟唤醒，启动备份节点: %s", cands[candIdx].Name)
				}
				launchNode(cands[candIdx].RawURI)
				candIdx++
				timer.Reset(delay)
			}

		case <-ctx.Done():
			log.Printf("[Racing] 客户端断开，停止并行竞争")
			return
		}
	}

	if winner != nil {
		if !yield(winner.first) {
			return
		}

		collectTimeout := time.NewTimer(30 * time.Second)
		defer collectTimeout.Stop()
		for {
			select {
			case chunk, ok := <-winner.ch:
				if !ok {
					return
				}
				if !yield(chunk) {
					return
				}
			case bgRes := <-resCh:
				atomic.AddInt32(&active, -1)
				bgName := nodes.GetNodeName(bgRes.uri)
				if bgRes.err == nil {
					log.Printf("[Vertex] [StreamParallel] 后台收集: 节点 %s 加入粘性池", bgName)
					stickyPool.Add(bgRes.uri)
				} else if bgRes.err != context.Canceled && !errors.Is(bgRes.err, context.Canceled) {
					ve := asVertexError(bgRes.err)
					if ve != nil && ve.Kind == "ratelimit" {
						nodes.RecordRateLimit(bgRes.uri, 30)
					} else {
						nodes.RecordTest(bgRes.uri, false, 0, bgRes.err.Error())
					}
				}
			case <-collectTimeout.C:
				log.Printf("[Vertex] [StreamParallel] 后台收集超时，退化至仅读取胜出流")
				for chunk := range winner.ch {
					if !yield(chunk) {
						return
					}
				}
				return
			case <-ctx.Done():
				return
			}
		}
	} else {
		yield(StreamChunk{Err: NewInternalError("all nodes failed to stream")})
	}
}
