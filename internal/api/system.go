package api

import (
	"context"
	"os/exec"
	"time"
)

// CapabilityDTO возвращает отдельно проверенную доступность компонента, причину и время проверки.
type CapabilityDTO struct {
	// Status содержит available, unavailable или unverified согласно фактической проверке.
	Status string `json:"status"`
	// Reason содержит ограниченную причину результата проверки возможности.
	Reason string `json:"reason"`
	// CheckedAt содержит UTC-время выполненной проверки либо null для непроверенного оборудования.
	CheckedAt *time.Time `json:"checked_at"`
}

// CapabilitiesDTO разделяет проверки исполняемых файлов и аппаратной возможности NVIDIA.
type CapabilitiesDTO struct {
	// FFmpeg сообщает только разрешение настроенного исполняемого FFmpeg.
	FFmpeg CapabilityDTO `json:"ffmpeg"`
	// FFprobe сообщает только разрешение настроенного исполняемого ffprobe.
	FFprobe CapabilityDTO `json:"ffprobe"`
	// Helper сообщает разрешение настроенного gost-process-helper.
	Helper CapabilityDTO `json:"helper"`
	// NVIDIA сообщает отдельный статус аппаратной возможности без вывода из наличия FFmpeg.
	NVIDIA CapabilityDTO `json:"nvidia"`
}

// CapabilitiesProvider предоставляет текущий снимок фактически выполненных проверок возможностей.
type CapabilitiesProvider interface {

	// Snapshot возвращает фактически проверенные возможности с собственными статусами и временем;
	// разрешение исполняемого файла не подтверждает работу аппаратного кодировщика.
	Snapshot(context.Context) (CapabilitiesDTO, error)
}

// ExecutableCapabilities проверяет только явно заданные исполняемые файлы без запуска процессов.
type ExecutableCapabilities struct { /* FFmpeg задаёт проверяемый исполняемый FFmpeg; FFprobe задаёт проверяемый ffprobe; Helper задаёт проверяемый gost-process-helper. */
	FFmpeg, FFprobe, Helper string
}

// Snapshot проверяет разрешение явно настроенных исполняемых файлов через PATH без запуска процессов.
// Аппаратная возможность NVIDIA остаётся unverified, а ошибка контекста прерывает проверку.
func (p ExecutableCapabilities) Snapshot(ctx context.Context) (CapabilitiesDTO, error) {
	// err содержит причину отмены запроса возможностей до проверки исполняемых файлов.
	if err := ctx.Err(); err != nil {
		return CapabilitiesDTO{}, err
	}
	// now задаёт единое UTC-время локальных проверок исполняемых файлов текущего снимка.
	now := time.Now().UTC()
	// check различает отсутствие настройки, недоступность исполняемого файла и успешное разрешение
	// его пути без запуска.
	check := func(name string) CapabilityDTO {
		// Различает отсутствие настройки, невозможность найти исполняемый файл и успешное разрешение его пути
		// без запуска процесса.

		if name == "" {
			return CapabilityDTO{"unavailable", "not_configured", &now}
		}
		// e содержит отказ поиска настроенного исполняемого файла через PATH или по указанному
		// пути.
		if _, e := exec.LookPath(name); e != nil {
			return CapabilityDTO{"unavailable", "executable_unavailable", &now}
		}
		return CapabilityDTO{"available", "executable_resolved", &now}
	}
	return CapabilitiesDTO{check(p.FFmpeg), check(p.FFprobe), check(p.Helper), CapabilityDTO{"unverified", "hardware_not_verified", nil}}, nil
}

// HealthDTO возвращает состояние работоспособности или готовности управляющей части.
type HealthDTO struct {
	// Status содержит live или ready при успешной соответствующей проверке.
	Status string `json:"status"`
	// Reason содержит публичную причину состояния, пустую для успешных live и ready.
	Reason string `json:"reason"`
}

// registerSystem публикует возможности, работоспособность HTTP и готовность управления. Готовность
// проверяется ограниченным чтением Store и не зависит от FFmpeg, NVIDIA или GPU-статистики.
func (a *API) registerSystem() {
	register(a, operation("GET", "/system/capabilities", "get-capabilities", "Проверенные возможности настроенных компонентов."), func(ctx context.Context, _ *struct{}) (*Result[CapabilitiesDTO], error) {
		// Возвращает снимок настроенного поставщика возможностей либо ошибку временной недоступности.

		if a.config.Capabilities == nil {
			return nil, unavailable()
		}
		// v содержит снимок поставщика возможностей; e содержит отказ его получения.
		v, e := a.config.Capabilities.Snapshot(ctx)
		return &Result[CapabilitiesDTO]{v}, e
	})
	register(a, operation("GET", "/health/live", "get-liveness", "Работоспособность HTTP управления."), func(context.Context, *struct{}) (*Result[HealthDTO], error) {
		// Подтверждает доступность HTTP управления без проверки медиакомпонентов.

		return &Result[HealthDTO]{HealthDTO{"live", ""}}, nil
	})
	register(a, operation("GET", "/health/ready", "get-readiness", "Готовность управления независимо от медиавозможностей."), func(ctx context.Context, _ *struct{}) (*Result[HealthDTO], error) {
		// Проверяет чтение хранилища через список потоков перед подтверждением готовности управления.

		// e содержит отказ ограниченного чтения Stream из хранилища для проверки readiness.
		_, e := a.config.Core.ListStreams(ctx, pageOne())
		if e != nil {
			return nil, unavailable()
		}
		return &Result[HealthDTO]{HealthDTO{"ready", ""}}, nil
	})
}
