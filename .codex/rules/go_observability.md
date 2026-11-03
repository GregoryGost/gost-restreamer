# Go observability rules

Activate only by exact `go_observability.md` in Active Rules, `go-observability` in Active Stack Profiles, or the enabled Go Observability profile naming the `go_observability.md + go-observability` pair. Mentioning logs or metrics in a task does not activate the overlay.

## Required skills

- `go-core`
- `go-observability`

## Project policy and source of truth

- Read the target's `CODEX_PROJECT.md` Go Application Context And Data Disclosure Policy and Go Observability Profile as configured when the skill was installed. These are project decisions, not per-task risk assessments. If absent, ambiguous or contradictory for the affected operation, report the missing configuration and request a **one-time profile decision** rather than inventing defaults.
- Application log disclosure is governed by the single shared policy, **not** an independent privacy default in this rule. `full` explicitly permits complete values, including tokens, in application logs for the categories selected by the owner. Do not silently replace them with hashes or masks. `masked` and `selective` obey their exact declared categories. Full logging is an explicit accepted exposure trade-off, not permission to disclose credentials in agent messages, review comments or unrelated external systems.
- Consult actual Go version and declared `slog`/OpenTelemetry exporter versions, existing logging configuration, telemetry destinations and retention. Never inject new collectors, dependencies, exporters, endpoint listeners or CI upload without authorization.

## Design and verification

- Separate events/logs, metrics and traces; do not infer that logging implies tracing or an exporter. Standard-library `log/slog` is the version-appropriate baseline (Go 1.21+), while OpenTelemetry and exporters are optional project choices.
- Preserve event meaning, stable correlation IDs, span/context propagation and correct parent-child relationships. Avoid double instrumentation, globally unbounded labels, misleading status/latency measurements and storing mutable request state in long-lived loggers.
- Respect declared per-field capture, retention and export boundaries; protect debug/pprof routes separately under their existing authorization rules.
- Test structured fields, error attribution, context propagation, shutdown/flush on owned telemetry providers and absence of accidental third-party exports. Do not claim observability from metrics/logs that were not actually collected.
- Use `.agents/skills/go-observability/references/` for concrete patterns and source URLs. Coordinate with `go-security` only when it is independently active; both use the **same** shared project policy.

