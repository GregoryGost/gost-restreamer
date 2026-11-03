---
name: go-observability
description: "Use for Go structured logging, metrics, OpenTelemetry traces, correlation, telemetry lifecycle and verification under declared project disclosure policy."
---

# Go Observability — logs, metrics and traces

Own the application's telemetry behavior, not generic Go language rules, Huma routing or deployment infrastructure. Standard-library `log/slog` is preferable for basic logging when supported; an OpenTelemetry collector is **not** a prerequisite.

## Required Dependencies

- `go-core`

Use `.codex/rules/go_observability.md` only when independently active. The rule and skill pair is selected explicitly in `CODEX_PROJECT.md`; this skill does not activate other overlays.

## Fixed project policy

1. Read the **already selected** Go Application Context And Data Disclosure Policy and Go Observability Profile from `CODEX_PROJECT.md`. These are configured once when the skill is integrated into the target project; do not recompute the application's exposure classification on each task.
2. If the selected profile is incomplete, materially contradictory to observed deployment or names an unavailable runtime/tool, report the exact mismatch; do not silently revise it, change redaction mode or invent an exporter.
3. Obey the owner's category-level `full`, `masked` or `selective` application-log capture. Complete tokens, headers or payloads are permitted when explicitly selected for that category. Their visibility in application-owned logs does **not** expand authorization to echo them into agent chat, PRs, telemetry exporters or external tools.
4. Read `go.mod`, Go language/toolchain, current logging implementation, request/message boundaries, output destinations, operational permissions and project validation commands.

## Implement only the telemetry needed

- Prefer structured events with stable names, levels, timestamp semantics and field names; use `slog` only if available in the effective Go version. Keep deterministic correlation fields and domain-specific event meaning distinct from noisy developer traces.
- For traces and metrics, use active project-approved OTel API/SDK/exporter versions and instrumentation boundaries. Verify actual selected release status and compatibility; APIs, instrumentation packages and exporters evolve independently.
- Propagate `context.Context` where trace state belongs to an operation; avoid creating disconnected spans, unbounded-cardinality metric labels and accidental duplicate middleware instrumentation.
- Decide whether an observed event is a request attempt, completed operation, retry or business action; distinguish failures from cancellations. Preserve enough identifiers to diagnose the user's stated case under its declared logging policy.
- Own shutdown/flush of logging/export pipelines at composition root, with bounded time and explicit loss/partial-flush reporting. No service installation or live endpoint changes are implied.

## Reference routing

- `references/structured-logging-and-disclosure.md` — `slog`, exact disclosure modes, event schema and Good/Bad.
- `references/otel-metrics-traces.md` — telemetry ownership, propagation, exporter opt-in, cardinality and lifecycle.
- `references/testing-and-review.md` — verification matrix, anti-patterns and review questions.
- `references/official-sources.md` — official, version-sensitive sources.

## Verification

Use the actual configured handlers, traces and metrics. Verify field presence and values according to the **chosen** project policy; a test must not enforce masking if the project's declared policy is `full`. Test cancellation, error events, span context and graceful flush when relevant, without transmitting live credentials to third parties.

## Optional coordination

`go-http-client` and `go-huma-v2` may define HTTP span boundaries only when independently active. `go-security` may identify a real disclosure problem, but cannot override an explicit project logging decision by silently changing telemetry. `go-testing` remains independent.

