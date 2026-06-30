package nodes

import "sync"

type StickyNodePool struct { //nolint:govet
	mu    sync.Mutex
	pool  map[string]bool
	inUse map[string]bool
}

var globalStickyPool = NewStickyNodePool() //nolint:gochecknoglobals

func GetStickyPool() *StickyNodePool {
	return globalStickyPool
}

func NewStickyNodePool() *StickyNodePool {
	return &StickyNodePool{ //nolint:exhaustruct
		pool:  make(map[string]bool),
		inUse: make(map[string]bool),
	}
}

func (p *StickyNodePool) Acquire() (string, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for uri := range p.pool {
		if p.pool[uri] && !p.inUse[uri] {
			p.inUse[uri] = true
			return uri, true
		}
	}
	return "", false
}

func (p *StickyNodePool) Release(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.inUse, uri)
	p.pool[uri] = true
}

func (p *StickyNodePool) Evict(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.pool, uri)
	delete(p.inUse, uri)
}

func (p *StickyNodePool) Add(uri string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pool[uri] = true
}

func (p *StickyNodePool) IsSticky(uri string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, exists := p.pool[uri]
	return exists
}

func (p *StickyNodePool) AvailableCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	count := 0
	for uri := range p.pool {
		if !p.inUse[uri] {
			count++
		}
	}
	return count
}

func (p *StickyNodePool) StaleCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inUse)
}

func (p *StickyNodePool) List() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	uris := make([]string, 0, len(p.pool))
	for uri := range p.pool {
		uris = append(uris, uri)
	}
	return uris
}
