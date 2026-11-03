---
name: go-local-cache
description: "Use for single-process Go caching with Otter v2: size/weight eviction, TTL, loaders, refresh, invalidation, memory, lifecycle, and tests."
---

# Go Local Cache — Otter v2

Use this skill when the target `CODEX_PROJECT.md` independently selects `go-local-cache` with Otter v2. This is an **in-process** cache: it is not shared between replicas, does not implement distributed locks and does not require NATS, Redis or another cache library. It can be discarded and rebuilt.

## Required Dependencies

- `go-core`

Only material dependencies are listed above. Apply `.codex/rules/go_local_cache.md` when independently active, and use `material_dependencies.md` for active dependency validation. Do not activate `go-nats-kv-cache` implicitly.

## Workflow

1. Inspect `CODEX_PROJECT.md`, `go.mod`, `go.sum`, CI Go toolchain, pinned `github.com/maypok86/otter/v2`, call sites, cache owners, writes/commits and test commands.
2. Identify the cached query/contract: key scope, owner, value DTO, visibility/authorization inputs, acceptable staleness, and expected invalidation.
3. Select finite `MaximumSize` **or** `MaximumWeight` with `Weigher`. Choose `ExpiryWriting`, `ExpiryAccessing` or a project-declared policy. Distinguish logical expiry from physically freed heap.
4. Decide whether explicit `GetIfPresent` + service fetch or Otter `Get` with a loader is warranted; avoid duplicate cache-aside boilerplate if the verified loader already solves it.
5. Default to no automatic background refresh. If approved, establish staleness policy, handling of refresh errors, cancelability and application lifecycle ownership.
6. Implement only the approved source changes, keeping caching decisions in service/read paths and persistence in repositories; do not expose internal cache objects through Huma contracts.
7. Test hit/miss, expiration, eviction, concurrent loading, invalidation after commit and start/stop when relevant. Run or report project-declared checks; do not assert a memory leak solely from RSS.

## Technical guardrails

- Otter v2 is the **selected backend**, not a license to add new dependencies to a target whose project policy prohibits them. Upstream v2 currently declares Go 1.24+, but check the version pinned by the target.
- `Set` returns `(V, bool)`, with `bool` reflecting whether this was a new association, **not** proof of durable write. `GetIfPresent` returns `(V, bool)`. Use documented `Invalidate(key)` for key invalidation.
- `MaximumSize` bounds entry count, not bytes; `MaximumWeight`/`Weigher` are static weight estimates, not strict process-memory limits. No promise of exact eviction instant.
- Cache keys must include tenant/user/role/feature flags/locale/filters/pagination/version as applicable; do not coalesce different authorization contexts under one key.
- Avoid storing pointers to mutable results or oversized backing arrays across requests. Use immutable/read-model values and document copy/ownership semantics.
- Caching may bypass the source-of-truth read; avoid it for authorization-critical or strongly consistent flows unless the actual consistency model is proven.
- `Get` loaders can coalesce concurrent calls per key, so a key must represent the same scope, data and permission contract for all callers.
- Otter `Refresh` / refresh calculators may return stale data while refreshing and may detach loader work from caller cancellation. Treat as opt-in, version-verified behavior.
- Use `StopAllGoroutines()` for owned cache background activities when retiring a cache. Do not assume a generic `Close()` method exists on the Otter cache.
- Reuse the project's existing instrumentation; do not expose profiling endpoints or introduce runtime switches without authorization.

## Reference routing

- `references/otter-v2-patterns.md` — actual Otter API, Good/Bad examples, expiry/eviction/refresh.
- `references/testing-and-review.md` — focused unit/integration tests and acceptance checklist.
- `references/official-sources.md` — versioned authoritative sources.
- `.agents/skills/go-core/references/cache-design.md` — shared design requirements only when cache correctness, keys, staleness and invalidation are in scope.
- `.agents/skills/go-core/references/memory-and-resource-leaks.md` — only when leak/lifetime behavior is affected.

## Optional coordination

Apply independently active `go-testing` for code/test changes, `go-huma-v2` for HTTP exposure, and `go-gorm` or `go-gorm-sqlite-goose` for database read and post-commit invalidation. None is an unconditional dependency of this skill.

