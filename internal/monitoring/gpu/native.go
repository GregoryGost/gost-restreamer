package gpu

import (
	"sync"
	"sync/atomic"
)

var nativeOwner struct {
	sync.Mutex
	provider *nativeProvider
}

// nativeProvider сохраняет право процессного владения при неопределённой ошибке Shutdown.
// Привязка NVML изолирована файлом Linux, протокол владения проверяется без GPU на любой ОС.
type nativeProvider struct {
	mu             sync.Mutex
	admission      sync.Mutex
	backend        Provider
	claimed        bool
	initialized    bool
	loadFailed     bool
	started        atomic.Bool
	closed         bool
	collector      bool
	shutdownReason Reason
}

func newNativeProvider(backend Provider) *nativeProvider {
	return &nativeProvider{backend: backend, shutdownReason: Available}
}

func (p *nativeProvider) claimCollector() bool {
	p.admission.Lock()
	defer p.admission.Unlock()
	if p.collector || p.started.Load() {
		return false
	}
	p.collector = true
	return true
}

func (p *nativeProvider) Init() Reason {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.Swap(true) || p.closed {
		return Busy
	}
	nativeOwner.Lock()
	if nativeOwner.provider != nil {
		nativeOwner.Unlock()
		return Busy
	}
	nativeOwner.provider = p
	p.claimed = true
	nativeOwner.Unlock()
	result := normalized(p.backend.Init())
	p.initialized = result == Available
	p.loadFailed = result == LibraryUnavailable
	return result
}

func (p *nativeProvider) Poll(limits Limits) Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized || p.closed {
		return Sample{Reason: ProviderError}
	}
	return p.backend.Poll(limits)
}

func (p *nativeProvider) Shutdown() Reason {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || !p.claimed {
		return p.shutdownReason
	}
	p.closed = true
	if !p.loadFailed {
		p.shutdownReason = normalized(p.backend.Shutdown())
	}
	if p.shutdownReason == Available {
		nativeOwner.Lock()
		if nativeOwner.provider == p {
			nativeOwner.provider = nil
		}
		nativeOwner.Unlock()
		p.claimed = false
	}
	return p.shutdownReason
}
