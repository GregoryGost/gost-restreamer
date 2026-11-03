# Otter v2 — API patterns, limits and anti-patterns

## Minimum version and source

The examples below use `github.com/maypok86/otter/v2` and require the Go language/toolchain version declared by the installed Otter module (upstream v2 currently Go 1.24+). Select the exact dependency version from the target `go.mod`. Do not run upstream `go get -u` examples as a routine validation step.

## GOOD — bounded local cache with time-based expiry

Standalone example (assuming the Otter v2 module is already an approved dependency):

```go
package main

import (
    "fmt"
    "time"

    "github.com/maypok86/otter/v2"
)

func main() {
    users, err := otter.New(&otter.Options[string, string]{
        MaximumSize:      10_000,
        ExpiryCalculator: otter.ExpiryWriting[string, string](5 * time.Minute),
    })
    if err != nil {
        panic(err)
    }
    defer users.StopAllGoroutines()

    users.Set("tenant-a:user-42", "public-data")
    value, found := users.GetIfPresent("tenant-a:user-42")
    fmt.Println(value, found)
    users.Invalidate("tenant-a:user-42")
}
```

The example is illustrative, **not a default 10,000-entry configuration** for any project. In production, size/expiry and key format must be justified by measured workload and access boundaries. Do not use `StopAllGoroutines` while other requests still use the cache.

## BAD — confusing admission with durable success

```go
package cachecheck

import "github.com/maypok86/otter/v2"

// BAD: false also means a key was already present.
func UpdateBad(users *otter.Cache[string, string], key, value string) {
    _, newlyAssociated := users.Set(key, value)
    if !newlyAssociated {
        panic("cache write failed")
    }
}
```

**GOOD — update the value without misinterpreting the result:**

```go
package cachecheck

import "github.com/maypok86/otter/v2"

func Update(users *otter.Cache[string, string], key, value string) {
    users.Set(key, value)
}
```

For Otter, the boolean indicates whether the key was newly associated rather than whether the cache successfully persisted an entry. An update to an existing key can return false. It is not a substitute for a transaction or distributed persistence guarantee.

## BAD — unlimited cardinality under one TTL

```text
Cache each random request identifier for one hour.
No MaximumSize or MaximumWeight is configured.
Assume TTL alone limits the peak number of live entries.
```

A high-cardinality workload may retain many simultaneously live entries before expiry. Set a capacity budget and decide whether a value-weight policy is necessary. `MaximumSize` and `MaximumWeight` are alternative policies, not simultaneous settings; `Weigher` is required for weight limits.

## Loading, refresh and invalidation

- Prefer plain `GetIfPresent` for a simple cache-aside path with explicit service ownership. Use `Get(ctx,key,loader)` if duplicate-load coalescing and loader semantics fit the business contract.
- For a loader, return `otter.ErrNotFound` only for a verified absent entity; do not convert database/network outages to "not found" or cache error payloads as success.
- Loader coalescing is **process-local**. Different caller permissions or other inputs must never share one cache key.
- `ExpiryWriting` expires since last write; `ExpiryAccessing` extends on access (with documented timer granularity behavior). Specify which semantics business data needs.
- Refreshing is **asynchronous** and may return the existing stale value. Background refresh errors generally preserve the old value (except not-found behavior); do not use it for correctness-critical reads without explicit acceptance.
- Invalidate after the database transaction actually commits. A racing load may still require versioned cache keys or other coordination when strong freshness is needed; TTL is not a transaction boundary.
- A local cache in one process says nothing about copies in another process. Do not introduce a multi-layer cache as a hidden default.

## Resource and privacy boundaries

- Do not retain live GORM entities, DB sessions, HTTP requests, `context.Context`, goroutine-owned state or unbounded slices in cache values.
- Define logging/metrics without leaking cache keys containing personal identifiers or raw payloads. Prefer structured stable key scopes and hashes where applicable.
- Test `StopAllGoroutines` under approved lifecycle conditions; the call stops Otter-managed goroutines, it is **not** cache invalidation.
- Use `go-core/references/memory-and-resource-leaks.md` for actual resource-retention diagnosis rather than forcing GC or assuming `MaximumWeight` is a process-RSS limit.

