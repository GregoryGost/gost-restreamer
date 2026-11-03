# Go testing rules

Activate through an exact `go_testing.md` Active Rules entry, `go-testing` Active Stack Profiles entry, or a complete Go Testing profile naming `go_testing.md + go-testing`. A Go-related request alone is not activation.

## Required skills

- `go-core`
- `go-testing`

## Source of truth

Read `CODEX_PROJECT.md`, `go.mod`, `*_test.go`, relevant production code, build/test flags and CI before changing tests. Resolve the declared unit/integration/E2E boundaries, race coverage and artifact policy.

## Constraints

- Default to standard-library `testing`, `testing/quick` where suitable and `net/http/httptest` for HTTP; use external assertions, mocks, property-testing frameworks or container libraries only when already declared/approved.
- Test observable behavior: success, expected failures, boundary values, cancellation, authorization, consistency and failure recovery as relevant.
- Use table-driven tests and `t.Run` where clarity improves; group related behavior without producing one tiny test file per case.
- Use `t.Helper`, `t.Cleanup`, `t.TempDir`, `t.Setenv` appropriately. `t.Setenv` must not run in parallel tests or with parallel ancestors.
- Only call `t.Parallel` after verifying independent state, config, test files, DBs, ports and mocks.
- Use isolated real database integration tests for query, transaction, constraint, migration and locking semantics. Do not mock away the persistence engine under test.
- Use real router/handler boundaries in API integration tests, and reserve actual network/process E2E tests for flows that need them.
- Use `go test -race` when supported/declared and meaningful; no claim of race freedom from a single passing run. The race detector is not a goroutine, heap or resource leak detector.
- For tasks changing background worker, pool, stream, cache or resource ownership, test cancellation, join, cleanup and repeated lifecycle as appropriate. Use `go-testing/references/leak-regression.md`; never require brittle global goroutine-count equality or unapproved leak-test dependencies.
- Coverage is a diagnostic signal, not a substitute for behavior testing. Write coverage files only in approved output paths.
- Fuzz security- or correctness-critical parsers/encoders with bounded time, not indefinitely by default.
- Do not run destructive tests against production or shared user-owned systems.

