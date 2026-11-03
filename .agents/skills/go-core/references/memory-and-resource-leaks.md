# Go memory retention, goroutine leaks and resource lifecycle

Use this reference for long-running Go applications, suspected leaks, unbounded growth, resource lifetime changes, cache/queue changes or memory diagnostics. This is a **diagnosis and prevention workflow**, not a mandate to profile every small code edit. The source guidance is framework-neutral; GORM/SQLite-specific details stay in their owning overlay.

## 1. Classify the observation before assigning blame

| Observation | Distinction and first evidence |
| --- | --- |
| Retained Go objects | Data remains reachable after a business operation completes. Compare heap `inuse_space` profiles and live-object metrics at comparable GC/workload phases. |
| High allocation churn | Large `alloc_space` or allocation counts can increase GC/CPU cost without increasing live heap. Not itself a leak. |
| Goroutine leak | A goroutine fails to terminate after its owner exits; it may retain stacks, requests, channel values or buffers. Inspect lifecycle and goroutine profiles. |
| Resource leak | Unclosed `http.Response.Body`, `*sql.Rows`, `*sql.Conn`, transactions, files, sockets or native handles can exhaust resources without proportionate heap growth. |
| Native / CGO memory | SQLite and other native libraries may allocate outside Go runtime accounting. Compare OS process metrics with Go runtime metrics and known driver/native allocations. |
| High RSS / reserved memory | RSS includes more than live heap; allocator retention, mapped files, goroutine stacks and OS behavior matter. RSS growth alone is **not proof** of a leak. |

**Never** equate a data race with a memory leak, a rising `alloc_space` profile with permanently retained data, or a large process RSS with a Go heap leak. An intentionally retained cache or connection pool is not automatically defective; compare it with the declared bounds, workload and lifecycle policy.

## 2. Evidence-based diagnostic sequence

1. **Define the lifecycle and hypothesis.** Identify what should be released (per-request buffers, workers, temporary DB rows, cache entries, native handles), its owner, release trigger and acceptable steady-state growth. Reconstruct how work could remain reachable.
2. **Make the workload reproducible.** Capture Go language/runtime version, build tags, CGO, OS/architecture, process topology, dependency versions, traffic mix, background jobs and cache warm-up. Place diagnostic artifacts outside the repository in an approved location; protect potentially sensitive profile contents.
3. **Collect a baseline after warm-up**, repeat the *same bounded workload*, let jobs complete and compare later observations at comparable load and GC phases. Use multiple observations; do not infer a leak from one heap or goroutine count.
4. **Correlate dimensions:** Go `/gc/heap/live:bytes`, `/memory/classes/heap/objects:bytes`, `/sched/goroutines:goroutines`, and relevant pool stats; separately observe process RSS, file descriptors/handles and native memory if available. Verify metric names against the running toolchain. No single counter is definitive.
5. **Inspect cause:** capture heap (`inuse_space` for currently retained sampled heap objects, `alloc_space` for sampled allocation history) and goroutine profiles. Inspect retaining allocation sites, growing maps/queues, blocked goroutines and owners. Profiles are sampled; absence of a sample is not proof of absence.
6. **Change the smallest actual ownership/lifecycle bug**, then replay the same workload. Verify resource closure and stabilization by comparable evidence rather than forcing `runtime.GC()` in production or changing `GOGC` to hide symptoms.
7. **Report limits:** distinguish VERIFIED, INFERRED and UNVERIFIED; state sample count, reproduction conditions, Go/driver version, profile cost, source path and residual risk. If CGO/driver allocations cannot be measured, say so.

Useful commands (diagnostic examples; substitute **approved local profile paths** and protect access):

```sh
go tool pprof -inuse_space ./profiles/heap-after.pb.gz
go tool pprof -alloc_space ./profiles/heap-after.pb.gz
go tool pprof -base ./profiles/heap-before.pb.gz ./profiles/heap-after.pb.gz
```

Profile subtraction is an investigative aid, not proof by itself: align workloads/GC phases and check that samples/metadata are comparable. Use project-approved profile collection, not an assumed exposed HTTP endpoint. `net/http/pprof` handlers may reveal stack traces, request details and service internals; never publish them on the public Huma/API listener without explicit security-approved isolation. Prefer protected local/internal access, scoped authorization and bounded collection time. Production profiling has overhead and needs operational approval.

## 3. BAD / GOOD: ownerless goroutine (Go 1.7+ for context)

BAD — returns without a contract to stop a reader if the input channel remains open. This code is valid and only leaks when the channel is never closed and the worker remains unwanted:

```go
package jobs

func StartBad(events <-chan int) {
	go func() {
		for range events {
			// Process events.
		}
	}()
}
```

GOOD — explicit cancellation plus a completion signal. The caller is responsible for `cancel()` and waiting on `done`; cancellation ends a goroutine even if the input channel never closes:

```go
package jobs

import "context"

func Start(ctx context.Context, events <-chan int) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-events:
				if !ok {
					return
				}
			}
		}
	}()
	return done
}
```

Both snippets are standalone package examples with no external dependencies. The real consumer should process the received event and specify its processing/error behavior. A signal only is sufficient here; long-running or failing work requires an explicit error/exit policy. Never assume cancellation automatically joins a goroutine blocked on unrelated I/O; pass the context through cancellable operations.

## 4. BAD / GOOD: retaining a large backing array (Go 1.0+)

BAD — a tiny returned prefix may keep the entire original backing array reachable if the caller retains it:

```go
package payload

func Prefix(input []byte) []byte {
	if len(input) > 16 {
		input = input[:16]
	}
	return input
}
```

GOOD — copy only when the contract requires an independent small result:

```go
package payload

func Prefix(input []byte) []byte {
	if len(input) > 16 {
		input = input[:16]
	}
	return append([]byte(nil), input...)
}
```

The GOOD version deliberately returns a nil slice for an empty input; if wire semantics distinguish nil from empty, preserve that contract. Copying increases allocations: if both sides share lifetime/ownership and the original buffer can be released normally, the BAD snippet may be the correct zero-copy design. Make the tradeoff explicit rather than always cloning slices.

Other retention risks: unbounded maps/queues, caches without eviction, large values retained by closures, forgotten subscriber registrations, long-lived timers/tickers held by active owners, and retry buffers without size limits. Long-lived collections should have a documented bound, expiry/eviction, or a justified durability policy based on actual requirements.

## 5. BAD / GOOD: rows and connection ownership (Go 1.8+ for QueryContext)

BAD — early return leaves `Rows` open and can hold a pooled connection:

```go
package repository

import (
	"context"
	"database/sql"
)

func FirstBad(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT 1 UNION ALL SELECT 2")
	if err != nil {
		return 0, err
	}
	if rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
		return n, nil
	}
	return 0, rows.Err()
}
```

GOOD — close on every return path and check iteration errors:

```go
package repository

import (
	"context"
	"database/sql"
)

func First(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, "SELECT 1 UNION ALL SELECT 2")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, err
		}
		return 0, sql.ErrNoRows
	}
	var n int
	if err := rows.Scan(&n); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	return n, nil
}
```

The query is illustrative and assumes a driver supporting its SQL syntax; it is not a complete repository strategy. `*sql.DB` is a shared pool owned by the application, not closed per request. Explicitly close owned `*sql.Conn`, statement handles, files and client HTTP response bodies; use `Commit`/`Rollback` as required for transactions. A `Rows` that reaches EOF may auto-close, but early returns make explicit `Close` necessary.

## 6. Version-specific tools and false positives

- **Go 1.27+**: `runtime/pprof` provides a stable `goroutineleak` profile; `net/http/pprof` can serve `/debug/pprof/goroutineleak` on an **approved, protected** listener. It detects some goroutines permanently blocked on unreachable channels/synchronization primitives. It **does not** prove freedom from leaks: network I/O stalls and waits on reachable/global primitives can be missed. In Go 1.26 this profile was experimental and required opt-in; do not assume it exists in earlier targets or add old `GOEXPERIMENT` flags to Go 1.27.
- **Go 1.23+**: unreferenced timers/tickers can become collectible under the modern timer semantics (subject to the main module's language level and runtime configuration; Go 1.27 removes the old `asynctimerchan` override). Calling `Stop` may still be required by the **behavioral** ownership contract to prevent callbacks, future ticks or stale work. Do not flag `time.After` as a universal leak.
- **`go test -race`**: finds executed data races, **not** heap, goroutine or resource leaks. `runtime.NumGoroutine()` is a noisy indicator, not a deterministic pass/fail oracle for global tests. `go.uber.org/goleak` is an optional approved third-party test helper, not mandatory.
- **`GOMEMLIMIT`**: a **soft Go runtime memory limit**, not a guaranteed process RSS ceiling, and it excludes native allocations such as CGO memory. Do not treat it, `GOGC`, extra `runtime.GC()` calls or periodic forced heap trims as a fix for retained references.
- **`sync.Pool`**: the runtime can discard pooled items at any time; do not rely on it as a durable bounded cache or a way to prevent leaks.

## 7. Review checklist

- [ ] Ownership/lifetime specified for goroutines, channels, caches, queues, callbacks, streams and native handles changed by the task.
- [ ] Finite completion/cancellation, bounded queues and explicit shutdown/drain semantics verified for relevant long-running paths.
- [ ] Response bodies, SQL Rows/transactions/connections, files and subscriptions released on every error/early-return path.
- [ ] Large arrays/maps/buffers are not unintentionally retained by small views, closures or global references.
- [ ] Heap in-use vs allocation churn, process RSS, runtime metrics and CGO/native memory are not conflated.
- [ ] Claimed leaks backed by a reproducible lifecycle, retaining path or comparable diagnostics, not guesswork.
- [ ] Toolchain-specific timer and `goroutineleak` features gated to the correct Go version.
- [ ] Debug endpoints, profiles and diagnostic artifacts protected and removed under approved artifact policy.
- [ ] Relevant tests cover repeated lifecycle, cancellation, cleanup and failure paths where practical.

## Official sources

- Go diagnostics: https://go.dev/doc/diagnostics
- GC and memory accounting: https://go.dev/doc/gc-guide
- Go runtime metrics and native memory exclusions: https://pkg.go.dev/runtime/metrics
- Heap and goroutine profiles: https://pkg.go.dev/runtime/pprof
- Protected pprof HTTP endpoints: https://pkg.go.dev/net/http/pprof
- Go 1.27 stable goroutine leak profile: https://go.dev/doc/go1.27
- Goroutine leak profiler limits/examples: https://go.dev/blog/goroutine-leak-profiles
- Go 1.23 timer semantics: https://go.dev/doc/go1.23
- SQL rows and connection ownership: https://pkg.go.dev/database/sql
- HTTP response-body ownership: https://pkg.go.dev/net/http
- Go runtime memory soft limit: https://pkg.go.dev/runtime