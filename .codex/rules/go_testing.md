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
- For an unresolved reproducible Go test failure, optionally use terminal `dlv test` according to `go-core/references/runtime-debugging.md` only if Delve is installed and compatible, interactive terminal input exists, and the scoped test's side effects are authorized. Debugger output is diagnostic evidence; rerun normal tests after any fix. Do not mandate or install a debugger.
- For performance questions, use `go-testing/references/benchmark-regression.md`: representative benchmarks, repeated comparable measurements and ordinary correctness checks; don't install `benchstat` or enable PGO without target approval. A performance result does not replace regression tests.
- The Go Testing Profile in `CODEX_PROJECT.md` is the authority for coverage activation, scoped packages, outputs and minimum. When integrating `go-testing` into a **new** project, configure statement coverage with a default floor of **85%**; a higher target is valid, but any lower target or disabled gate needs an explicit project-owner exception. Do not silently change the profile of an existing project.
- When coverage is enabled, run the authorized **complete** coverage suite, require successful test execution and a usable current-run profile, calculate **aggregate statement coverage weighted by statement count after Go-equivalent duplicate-block normalization**, and compare to the configured threshold without raw-row double counting or rounded display ambiguity. Use an approved fresh per-run profile and validate the test exit status. Mark the gate failed below threshold; missing/invalid profile, zero instrumented statements or unrun checks are **not** passes. Do not silently narrow package scope, skip tests or exclude code to improve the number.
- Report measured numerator/denominator or the supported exact percentage, configured threshold, package scope, test status, excluded packages, known missing coverage, approved artifact path and baseline change when comparable evidence exists. If no automated threshold command is configured/executed, say the gate is **not enforced**, even if a displayed percentage looks high.
- See `go-testing/references/coverage-policy.md` for official Go commands and an evidence-based threshold check. Coverage is a quality signal, not proof of correct behavior; write output only in approved directories.
- Fuzz security- or correctness-critical parsers/encoders with bounded time, not indefinitely by default.
- Do not run destructive tests against production or shared user-owned systems.

