# Go leak and resource-lifecycle regression testing

Use when a change starts/backgrounds workers, holds buffers, opens connections/streams, changes caches/queues or claims to fix a leak. Do **not** require heap snapshots and memory-threshold assertions on every small unit test.

## Test the ownership contract first

1. Reconstruct the smallest owned lifecycle: create/start, produce work, finish or cancel, stop/drain, close and verify externally visible completion.
2. Exercise success and early failure/cancellation paths; include repeated start/stop where realistic. Use clean fixture state and test-owned resources.
3. Prefer a completion channel, WaitGroup, closed `Rows`/`Response.Body`, explicit handler state or pool stats to opaque goroutine-count tests.
4. Make timeouts **guards** around an event-driven wait; do not use `time.Sleep` to "let goroutines settle". `t.Cleanup` must restore environment and stop owned workers even when an assertion fails.
5. Reproduce unbounded growth with bounded repeated workloads and compare *like-for-like* profiles or resource stats when the bug actually concerns memory/resource retention. Avoid arbitrary fixed heap deltas, elapsed-time promises and global GC manipulation in production.
6. Keep integration tests disposable: temporary files/DBs, isolated HTTP servers and approved local dependencies; no access to user/production processes or live pprof endpoints without explicit authorization.

## GOOD — cancellation + deterministic completion (Go 1.14+; context itself requires Go 1.7+)

The test belongs to the same `jobs` package as `Start` in `go-core/references/memory-and-resource-leaks.md`. It is **not** standalone without that implementation.

```go
package jobs

import (
	"context"
	"testing"
	"time"
)

func TestStartStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	events := make(chan int)
	done := Start(ctx, events)
	t.Cleanup(func() { cancel() })

	cancel()
	select {
	case <-done:
		// Worker shutdown acknowledged.
	case <-time.After(2 * time.Second):
		t.Fatal("worker did not terminate after cancellation")
	}
}
```

This is a bounded CI guard, **not** a fixed wait. A production worker with a shutdown deadline and error result should be tested through that public contract instead. For Go 1.25+ projects, `testing/synctest` is an option for virtual time when its isolation and blocking constraints fit.

## Optional external helper: goleak

- Only when already declared by the project or explicitly approved: `go.uber.org/goleak` (`VerifyNone` in serial scoped tests or `VerifyTestMain` for package-level checks).
- `VerifyNone` is incompatible with `t.Parallel` due to goroutines of other tests being visible. Use `VerifyTestMain` or isolate tests as appropriate; do not silence genuine leaks with wide ignore lists.
- Do not add the dependency, amend `go.mod` or install a profiler without approval. Standard-library lifecycle tests are the default.

## Go 1.27 goroutineleak profile

- When available and permitted, an approved `runtime/pprof` `goroutineleak` profile can supplement lifecycle assertions for permanently blocked goroutines. It detects a subset and **cannot** prove absence of I/O-stalled or other reachable-condition leaks.
- Go 1.26 had an experiment requiring a separate build feature; do not use its flags as a portable test default.
- Do not export `/debug/pprof` publicly or capture real user data in CI artifacts.

## Resource and memory regressions

- HTTP: close `resp.Body` on success and error-status paths; test owned clients/servers shut down where lifecycle matters.
- Database: close `*sql.Rows` (including early errors), `*sql.Conn` and transactions; inspect `db.Stats()` and compare `InUse` after repeatable operations without relying on exact global timing. The `sql.DB` pool may **intentionally** keep idle connections open.
- File/native: close files and handles and assert behavior after repeated operations; on CGO drivers, Go heap profiles alone cannot capture SQLite/native allocator usage.
- Heap retention: compare baseline and after-load `inuse_space` in controlled integration/benchmark diagnostics, accounting for warm-up, GC sampling and legitimate cached data. Do not assert zero allocations or exact `runtime.MemStats` values as universal thresholds.
- A passing `-race` test is independent of, and insufficient for, leak detection.

## Acceptance checklist

- [ ] Relevant owned tasks and resources terminate or close on every exit path.
- [ ] Repeated lifecycle and cancellation/error paths are covered proportionally.
- [ ] No global goroutine baseline treated as a precise oracle; any helper dependency is approved.
- [ ] Timeouts only bound checks, not substitute for completion events.
- [ ] Go/CGO memory measurement limitations recorded.
- [ ] Test artifacts isolated, sanitized and cleaned according to project profile.

Sources: https://pkg.go.dev/testing ; https://pkg.go.dev/database/sql ; https://pkg.go.dev/runtime/pprof ; https://pkg.go.dev/go.uber.org/goleak ; https://go.dev/doc/diagnostics