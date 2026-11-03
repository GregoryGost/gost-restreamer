---
name: go-testing
description: "Use for Go unit, integration, HTTP service tests, fuzz tests, benchmarks, race checks, fixtures, coverage and deterministic isolation."
---

# Go Testing

Use Go standard-library `testing` and project-declared tools. Tests prove behavior and invariants, not just percentages.

## Required Dependencies

- `go-core`

Use `.codex/rules/go_testing.md` only when it is independently active; its required skill pair is validated under `material_dependencies.md`.

## Workflow

1. Inspect `CODEX_PROJECT.md`, `go.mod`, existing `*_test.go`, code under test, package boundaries, build tags, CI commands and available test dependencies.
2. Categorize the change as pure unit, HTTP/handler integration, real-database integration, external-system integration, process/service E2E, fuzzing or performance.
3. Select the narrowest test boundary that catches the bug. Write relevant tests before implementation when practical.
4. Cover happy path, failures, edge cases, cancellation and data invariants proportional to the change.
5. Run the declared checks; distinguish executed, unavailable and intentionally skipped test scopes.

## Unit tests

- Use table-driven cases and named `t.Run` subtests when they make failures easier to identify.
- Assert outputs and meaningful side effects rather than internal call sequence or private field layout.
- Prefer simple fakes or narrowly scoped test doubles; keep fake behavior aligned with real contracts.
- Use `errors.Is` / `errors.As` for stable error assertions instead of matching arbitrary text.
- Do not replace a complicated real component with an inaccurate mock and call the result integration coverage.

## Fixtures and determinism

- Use `t.TempDir` and `t.Cleanup`; close response bodies, servers, DB pools and background workers.
- For ownership-sensitive changes, test cancellation/stop plus completion and cleanup on success, errors and repeated lifecycle; use `references/leak-regression.md`. A stable Go runtime goroutine count is not a general-purpose pass/fail oracle.
- Use `t.Setenv` only in serial tests; it is unsafe with `t.Parallel` in the same test or ancestor.
- Avoid real sleeps, fixed ports and shared mutable test globals; use bounded readiness checks and deterministic clocks where appropriate.
- For Go 1.25+ modules, consider stable `testing/synctest.Test` and `synctest.Wait` to test scheduler/time-dependent behavior in an isolated bubble. Inspect API restrictions and avoid it where external real I/O or goroutine lifetimes do not fit that model; do not silently raise the module's minimum Go version.
- `t.Parallel` requires process/global/DB/port isolation. Confirm it before enabling `-shuffle=on` or parallel CI.
- Never rely on map iteration, test order, local timezone or installed external services without explicit fixture policy.

## Integration / E2E tests

- HTTP API: use `httptest.ResponseRecorder` or `httptest.Server` with the real registered router and middleware; verify status, headers, wire JSON and side effects.
- Database: migrate a temporary file-backed SQLite database when checking multi-connection semantics, foreign keys, WAL, locks or driver behavior.
- Service E2E: run real process/boundary and migrations with disposable dependencies when project configuration requires it; do not substitute handler tests for black-box tests.
- Contain external APIs behind project-approved fakes, local test servers or disposable infrastructure. Never point destructive tests at production.
- Capture enough diagnostics to explain failures while preserving secret redaction.

## Race, coverage, fuzz and benchmark

- Run the project-selected `go test ./...` and optionally `go test -race ./...` where the compiler/platform/CGO allow it. `-race` does **not** detect retained Go heap, leaked goroutines or open OS/SQL resources.
- Use `-count=1` to avoid test cache for reproducibility investigations and `-shuffle=on` to expose test-order coupling when fixtures are independent.
- Use `go test -coverprofile=<declared output> ./...` when coverage is requested/configured; avoid dumping extra artifacts in the repository root.
- Use bounded `go test -fuzz=FuzzName -fuzztime=<budget>` for appropriate pure functions, retaining valid regression corpus cases.
- Benchmarks (`go test -bench=... -benchmem`) require representative input and should not be used to assert functional correctness.

## Optional overlays

Apply `go-huma-v2` for independently active Huma contract tests. Apply `go-gorm` and `go-gorm-sqlite-goose` for independently active DB and migrations tests. When selected, use `go-local-cache` for Otter v2 cache-hit/expiry/eviction/loading tests, or `go-nats-kv-cache` for disposable JetStream KV/CAS/TTL/watch integration tests. Do not treat a mock as proof of NATS server behavior.

Load `references/test-boundaries-and-patterns.md` for general examples, `references/leak-regression.md` for lifecycle/memory regression testing when relevant, and `references/official-sources.md` for official tools.

