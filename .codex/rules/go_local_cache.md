# Go local cache rules — Otter v2

Activate only through the exact `go_local_cache.md` entry in Active Rules, `go-local-cache` in Active Stack Profiles, or a completed Go Local Cache profile naming the exact `go_local_cache.md + go-local-cache` pair. Mentioning caching does not itself activate this rule in a target project.

## Required skills

- `go-core`
- `go-local-cache`

## Scope and source of truth

- This rule covers **process-local** data caching with `github.com/maypok86/otter/v2`. It does not activate or manage NATS KV, Redis, GoCache, multi-level caching, durable queues or distributed locking.
- Read `CODEX_PROJECT.md`, `go.mod`/`go.sum`, exact Otter version, runtime Go version, existing service read/write paths and tests. Verify the module's supported Go version; current upstream v2 declares Go 1.24+, but that is not permission to raise the target toolchain automatically.
- Resolve ownership and selected configuration for TTL, capacity (`MaximumSize` or `MaximumWeight`), key scope, value type, optional loader, refresh policy, invalidation, observability and shutdown from project evidence.

## Constraints

- Cache only values whose staleness and loss on process restart are acceptable. The cache is not the source of truth and must not hold live ORM models, DB sessions, HTTP requests, connections or mutable shared runtime handles.
- Always apply a finite size or weight budget unless an explicit workload-based exception is documented. TTL is not a memory-capacity substitute; a weight function is an estimate, not a process RSS limit.
- Include tenant, user/permission scope and all result-affecting inputs in cache keys where applicable. Do not permit coalesced loaders to cross authorization boundaries.
- Invalidate/update only after successful source-of-truth commit. Cache-aside and versioned keys have different consistency limits; do not claim transactional atomicity.
- Use the actual Otter v2 APIs. `Set` returns the previous/present value and a boolean, not an error/success acknowledgment. `GetIfPresent` differs from `Get` with a loader.
- Background refresh is optional; validate its error/cancellation/staleness semantics before enabling. On app shutdown stop Otter-owned goroutines with the verified API `StopAllGoroutines` when needed; do not invent `Close`.
- For changes to cache lifetime, use active `go-testing` and `go-local-cache` references for TTL, eviction, concurrent loading, invalidation and leak regression tests.
- Do not install new libraries, change module versions or enable diagnostics/infrastructure merely because this rule is active.

