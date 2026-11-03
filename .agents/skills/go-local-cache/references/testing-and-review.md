# Otter v2 — tests and review

## Test matrix (select relevant cases)

- Cache hit, miss and cache-aside source call count with deterministic fixtures.
- TTL expiry for configured writing/access policy; use controlled timing when the active toolkit permits it and avoid flaky millisecond sleeps.
- Capacity/weight eviction with representative key/value sizes. Do not require exact victim order unless the library guarantees it.
- Concurrent same-key load: deduplicated work and consistent domain result; different tenant/user scopes never share the same loader key.
- Loader not-found, cancellation, timeout and source errors remain distinct; never turn backend outage into empty data.
- Post-commit invalidation: rollback retains prior cache policy; successful commit invalidates as required; concurrent write/read race tested where correctness needs it.
- Optional refresh: stale response, retry/error behavior, cancellation/lifecycle and no unwanted resurrection of invalidated entries.
- Stop/shutdown: Otter-owned background work terminates when application retires the cache; ensure no in-flight service request still uses it.
- Memory: finite capacity, absence of retained oversized buffers, no unbounded log/metric labels. Avoid exact RSS/heap equality assertions.

## Good / Bad: testing the actual return contract

GOOD: after `Set(k, v)`, verify `GetIfPresent` and key invalidation against an independently constructed cache. Avoid declaring failure because `Set` returned false on update. Tests should reflect eviction policy rather than assuming no eviction at finite capacity.

BAD: rely on a precise `time.Sleep` and global goroutine-count comparison as the only proof of no leak; run an unapproved network Redis/NATS instance from a local-cache test; add `goleak` without project permission.

## Review checklist

- [ ] Exact Otter v2 module version, Go minimum, runtime and dependency change authorization checked.
- [ ] Capacity (`MaximumSize` vs `MaximumWeight` + `Weigher`), TTL and key policy justified.
- [ ] Key includes all privacy/authorization/result-affecting dimensions.
- [ ] No ORM/client/request/context object cached as business data.
- [ ] Invalidation happens after commit; effects on concurrent reads understood.
- [ ] Loader/refresh behavior tested when enabled, and correctly documented.
- [ ] No presumed `Close()` or incorrect `Set` success semantics.
- [ ] No accidental dependency on NATS, GoCache, Redis or a two-level cache.
- [ ] Tests run or exact missing checks reported.

