# Concurrency, memory ownership and performance

Go does not prevent data races automatically. Channels, mutexes and atomics are different tools; choose by explicit ownership and synchronization requirements. A race-free program has stronger memory-model guarantees, but race-detector success from one execution is not a proof. Unless stated otherwise, Go snippets here require Go 1.0+; `context`-based worker code requires Go 1.7+ and `slices.Clone` requires Go 1.21+.

## 1. Synchronize shared state

**BAD — unsynchronized read-modify-write** (CORRECTNESS; test with `go test -race` where supported):

```go
package counter

type Counter struct{ n int }

func (c *Counter) Inc() { c.n++ }
func (c *Counter) Value() int { return c.n }
```

Using this type concurrently without external synchronization is a data race.

**GOOD — small shared state protected by a mutex** (Go 1.0+):

```go
package counter

import "sync"

type Counter struct {
    mu sync.Mutex
    n  int
}

func (c *Counter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.n++
}

func (c *Counter) Value() int {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.n
}
```

If the owner already serializes all calls, a lock may be unnecessary. For a simple independent counter, `sync/atomic` may fit; for transferring work/ownership, channels may fit. Do not use a goroutine-and-channel actor merely to increment one integer without a real actor/lifecycle need.

## 2. Cancel *and* join owned goroutines

**BAD — unmanaged background worker** (DESIGN/CORRECTNESS depending on caller lifecycle):

```go
package poller

import "time"

func Start(f func()) {
    go func() {
        for {
            f()
            time.Sleep(time.Second)
        }
    }()
}
```

There is no stop, join, error observation or policy for blocking `f`.

**GOOD — explicit lifecycle, cancelable loop, join** (Go 1.7+):

```go
package poller

import (
    "context"
    "time"
)

func Start(parent context.Context, f func()) func() {
    ctx, cancel := context.WithCancel(parent)
    done := make(chan struct{})

    go func() {
        defer close(done)
        ticker := time.NewTicker(time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-ctx.Done():
                return
            case <-ticker.C:
                f()
            }
        }
    }()

    return func() {
        cancel()
        <-done
    }
}
```

The code is demonstrative: `f` is synchronous and must return in bounded time for `stop` to complete. For blocking operations, pass `ctx` to `f`, propagate errors and choose a defined shutdown deadline/policy. Multiple callers can invoke the returned stop function because closing `done` is performed once by its owner; only this function calls `cancel`. For service-grade workers, use a named lifecycle type if it improves ownership and state management.

`sync.WaitGroup.Go` is available in Go 1.25+ for counting goroutines; the function supplied to it must not panic. It does not replace cancellation or error propagation. For older modules use `Add` / `Done` correctly.

## 3. Channels are not a substitute for a protocol

- Specify who sends, who receives and who closes each channel; only an owner with a complete protocol should close a channel.
- For worker pools, bound goroutine count and queue capacity; describe overload/backpressure and shutdown/drain behavior.
- Do not send on a channel after it may be closed; cancellation and producer shutdown must happen in an established order.
- A canceled context does not wake goroutines blocked on arbitrary channels unless the goroutine selects on `ctx.Done()`.
- Avoid calling an external API while holding a mutex or database write transaction.

## 4. Slice and map aliasing

**BAD — leaking mutable backing storage through a getter** (CORRECTNESS when a copy is promised):

```go
package catalog

type Catalog struct{ ids []string }
func (c *Catalog) IDs() []string { return c.ids }
```

**GOOD — define immutable-view behavior by copying** (Go 1.21+):

```go
package catalog

import "slices"

type Catalog struct{ ids []string }
func (c *Catalog) IDs() []string { return slices.Clone(c.ids) }
```

A `map` is also a reference-like data structure: unprotected concurrent access involving mutation is unsafe. Copy only where ownership requires isolation; repeated copying of large collections can be expensive, so verify it matches the contract and measure if critical.

## 5. Memory and resource leaks

- A data race, a permanently blocked goroutine and a retained heap object are different defects. A successful `-race` test cannot establish leak freedom.
- Check who owns cancellation and joins, whether buffers/maps/queues have bounds, and whether a short slice or closure accidentally retains a large object.
- When the task concerns memory growth, load `memory-and-resource-leaks.md` for heap/RSS/CGO distinctions, `pprof`, `runtime/metrics` and the Go 1.27 `goroutineleak` profile rather than relying on guessed thresholds.

## 6. Performance guidance

- Measure with benchmarks, `pprof`, `go tool trace` and allocation profiles before making performance claims.
- Use `go test -bench=. -benchmem` with representative data and compare under comparable environments; no benchmark is inherently a correctness test.
- Escape analysis (`go build -gcflags="-m"`) is diagnostic, not a mandate to eliminate all heap allocations.
- Investigate contention, unnecessary conversions, repeated allocations, oversized buffers and unbounded goroutines before optimizing micro-operations.
- Do not introduce `unsafe`, object pools or custom memory management without real evidence and a clearly tested correctness boundary.

## Review prompts

- Can two goroutines touch shared mutable memory without synchronization?
- Who creates/closes channels, workers and tasks, and how is cancellation joined?
- Are work/queue capacities and timeout policies bounded?
- Does code accidentally leak slice/map backing storage?
- Are performance conclusions based on measurements rather than folklore?

Official references: https://go.dev/ref/mem ; https://go.dev/doc/articles/race_detector ; https://pkg.go.dev/sync ; https://go.dev/blog/pprof

