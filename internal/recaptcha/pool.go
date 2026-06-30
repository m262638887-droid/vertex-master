// Copyright (c) 2026 BaiMeow. All rights reserved.
// Use of this source code is governed by the PolyForm Noncommercial License 1.0.0
// that can be found in the LICENSE file.

package recaptcha

import (
	"github.com/bsfdsagfadg/vertex/internal/config"
	"github.com/bsfdsagfadg/vertex/internal/transport"
)

type TokenPool struct {
	fetch func(proxyURI string) (string, error)
}

func NewTokenPoolSize(net *transport.NetworkClient, poolSize int) *TokenPool {
	return &TokenPool{
		fetch: func(proxyURI string) (string, error) { return FetchRecaptchaToken(net, proxyURI) },
	}
}

// Start 为生命周期钩子，当前实现为纯懒加载：token 仅在首次 GetToken 调用时按需获取。
// 不启动后台预取 goroutine，避免用户在未发送 chat 请求时产生无意义的 TLS 拨号开销。
// 历史版本（commit eac2847）曾使用后台 ticker 维持热缓存以消除首条请求同步获取延迟，
// 但对于不发送 chat 请求的用户完全浪费，且首条请求可能赶上缓存过期仍需同步获取，弊大于利。
// 若在极低延迟场景下需要预取，可在此处按需追加后台填充逻辑。
func (p *TokenPool) Start() {}

func (p *TokenPool) Stop() {}

func (p *TokenPool) Stats() (size, fill int) {
	return 0, 0
}

func (p *TokenPool) GetToken() (string, error) {
	return p.fetch(config.Load().ProxyURL)
}

func (p *TokenPool) GetTokenWithProxy(proxyURI string) (string, error) {
	if proxyURI == "" {
		return p.GetToken()
	}
	return p.fetch(proxyURI)
}
