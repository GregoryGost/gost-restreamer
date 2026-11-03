# Official observability sources

- Standard-library structured logs: https://pkg.go.dev/log/slog
- slog handler options and `ReplaceAttr`: https://pkg.go.dev/log/slog#HandlerOptions
- slog `LogValuer`: https://pkg.go.dev/log/slog#LogValuer
- OpenTelemetry Go: https://opentelemetry.io/docs/languages/go/
- OpenTelemetry Go instrumentation: https://opentelemetry.io/docs/languages/go/instrumentation/
- OpenTelemetry Go repositories and release metadata: https://github.com/open-telemetry/opentelemetry-go
- Go diagnostics: https://go.dev/doc/diagnostics
- Runtime metrics: https://pkg.go.dev/runtime/metrics

Prefer documentation for the project's pinned Go and OTel releases; optional contrib instrumentation/exporters may have separate versions. Treat signal status as version- and repository-dependent rather than assuming every signal has identical stability.

