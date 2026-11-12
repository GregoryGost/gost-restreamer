package gpu

import (
	"sync"
	"sync/atomic"
)

// nativeOwner сохраняет единственного процессного владельца NVML до подтверждённого освобождения.
var nativeOwner struct {
	// Mutex защищает проверку, закрепление и снятие процессного владельца NVML.
	sync.Mutex
	// provider указывает точного владельца NVML; ошибка Shutdown сохраняет его вместо допуска новой
	// загрузки.
	provider *nativeProvider
}

// nativeProvider сохраняет право процессного владения при неопределённой ошибке Shutdown.
// Привязка NVML изолирована файлом Linux, протокол владения проверяется без GPU на любой ОС.
type nativeProvider struct {
	// mu последовательно допускает Init, Poll и Shutdown к нативной реализации и состоянию её
	// жизненного цикла.
	mu sync.Mutex
	// admission защищает однократное закрепление источника за Collector до Init.
	admission sync.Mutex
	// backend выполняет платформенные операции NVML под последовательным протоколом обёртки.
	backend Provider
	// claimed отмечает закреплённое процессное владение, снимаемое только после подтверждённого
	// Shutdown.
	claimed bool
	// initialized разрешает Poll только после успешного Init нативной реализации.
	initialized bool
	// loadFailed отмечает отсутствие библиотеки, при котором нативный Shutdown не вызывается.
	loadFailed bool
	// started атомарно запрещает повторный Init и получение Collector после начала нативного
	// запуска.
	started atomic.Bool
	// closed запоминает завершённую попытку Shutdown и запрещает дальнейшие Poll.
	closed bool
	// collector отмечает уже назначенный Collector, исключая повторное использование того же
	// Provider.
	collector bool
	// shutdownReason сохраняет результат единственного нативного Shutdown для повторных вызовов.
	shutdownReason Reason
}

// newNativeProvider оборачивает реализацию NVML отдельным протоколом однократной загрузки и
// процессного владения.
func newNativeProvider(backend Provider) *nativeProvider {
	return &nativeProvider{backend: backend, shutdownReason: Available}
}

// claimCollector однократно закрепляет Provider за Collector до запуска Init и отклоняет повторное
// использование.
func (p *nativeProvider) claimCollector() bool {
	p.admission.Lock()
	defer p.admission.Unlock()
	if p.collector || p.started.Load() {
		return false
	}
	p.collector = true
	return true
}

// Init сериализует загрузку реализации NVML и получает единственное процессное владение. Повторный
// запуск или живой другой владелец возвращают Busy.
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
	// result содержит нормализованный результат Init для допуска Poll и различения отсутствующей
	// библиотеки.
	result := normalized(p.backend.Init())
	p.initialized = result == Available
	p.loadFailed = result == LibraryUnavailable
	return result
}

// Poll сериализует нативное чтение и передаёт Limits только успешно инициализированной незакрытой
// реализации NVML.
func (p *nativeProvider) Poll(limits Limits) Sample {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.initialized || p.closed {
		return Sample{Reason: ProviderError}
	}
	return p.backend.Poll(limits)
}

// Shutdown однократно освобождает реализацию NVML и снимает процессное владение только при
// подтверждённом успехе. Ошибка освобождения сохраняется; отсутствие библиотеки не вызывает нативный
// Shutdown.
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
