package api

import (
	"context"
	"os/exec"
	"time"
)

type CapabilityDTO struct {
	Status    string     `json:"status"`
	Reason    string     `json:"reason"`
	CheckedAt *time.Time `json:"checked_at"`
}
type CapabilitiesDTO struct {
	FFmpeg  CapabilityDTO `json:"ffmpeg"`
	FFprobe CapabilityDTO `json:"ffprobe"`
	Helper  CapabilityDTO `json:"helper"`
	NVIDIA  CapabilityDTO `json:"nvidia"`
}
type CapabilitiesProvider interface {
	Snapshot(context.Context) (CapabilitiesDTO, error)
}

// ExecutableCapabilities проверяет только явно заданные исполняемые файлы без запуска процессов.
type ExecutableCapabilities struct{ FFmpeg, FFprobe, Helper string }

func (p ExecutableCapabilities) Snapshot(ctx context.Context) (CapabilitiesDTO, error) {
	if err := ctx.Err(); err != nil {
		return CapabilitiesDTO{}, err
	}
	now := time.Now().UTC()
	check := func(name string) CapabilityDTO {
		if name == "" {
			return CapabilityDTO{"unavailable", "not_configured", &now}
		}
		if _, e := exec.LookPath(name); e != nil {
			return CapabilityDTO{"unavailable", "executable_unavailable", &now}
		}
		return CapabilityDTO{"available", "executable_resolved", &now}
	}
	return CapabilitiesDTO{check(p.FFmpeg), check(p.FFprobe), check(p.Helper), CapabilityDTO{"unverified", "hardware_not_verified", nil}}, nil
}

type HealthDTO struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
}

func (a *API) registerSystem() {
	register(a, operation("GET", "/system/capabilities", "get-capabilities", "Проверенные возможности настроенных компонентов."), func(ctx context.Context, _ *struct{}) (*Result[CapabilitiesDTO], error) {
		if a.config.Capabilities == nil {
			return nil, unavailable()
		}
		v, e := a.config.Capabilities.Snapshot(ctx)
		return &Result[CapabilitiesDTO]{v}, e
	})
	register(a, operation("GET", "/health/live", "get-liveness", "Работоспособность HTTP управления."), func(context.Context, *struct{}) (*Result[HealthDTO], error) {
		return &Result[HealthDTO]{HealthDTO{"live", ""}}, nil
	})
	register(a, operation("GET", "/health/ready", "get-readiness", "Готовность управления независимо от медиавозможностей."), func(ctx context.Context, _ *struct{}) (*Result[HealthDTO], error) {
		_, e := a.config.Core.ListStreams(ctx, pageOne())
		if e != nil {
			return nil, unavailable()
		}
		return &Result[HealthDTO]{HealthDTO{"ready", ""}}, nil
	})
}
