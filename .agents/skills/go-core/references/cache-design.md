# Shared Go caching design: key scope, freshness, correctness

This is a **backend-neutral** reference for independently activated Go cache skills. It is not a general-purpose cache interface, a new package, or a reason to enable caching by default. Storage APIs, TTL capabilities, lifecycle and integration tests are defined by the selected backend-specific skill.

## Cache eligibility

- First establish the source of truth and the behavior of a cache miss/outage. A cache must be disposable and reconstructible from that source unless a separately reviewed durability policy exists.
- Only cache read results for which some bounded staleness and transient loss are acceptable. Do not silently cache authentication/authorization decisions or strongly consistent invariants.
- Determine whether the data is per-process or shared across processes and which application instances own it. A local cache is not coherent across replicas.
- Set explicit capacity/size, expiry, expected cardinality, eviction, retry/fallback and observability budgets. TTL is not a correctness or memory capacity guarantee by itself.

## Key contract

- Include the data-schema version and every result-affecting input: tenant, user, role/permission scope, filters, ordering, pagination, locale, feature flags and source version where relevant.
- Use stable deterministic namespaced encoding; avoid raw credentials, secrets, unbounded arbitrary user input and privacy-sensitive data in metric labels/logs.
- The backend's key grammar is controlling: a string valid in Otter is not necessarily valid as a NATS KV key. NATS KV keys must be encoded to its supported character set.
- Define negative caching and error behavior: absent data, invalid data, authorization denial and backend failure are not equivalent. Do not cache transient errors or a permission-sensitive negative result under a public key.

## Mutation and invalidation

- Cache-aside read: check cache, fetch from authoritative storage on miss, then cache valid DTO. A cache miss can race with a concurrent write; choose an acceptable staleness contract.
- Write path: commit the authoritative transaction first, then invalidate/version the affected cache key(s). Never invalidate on rollback or claim two systems share a transaction.
- A commit followed by failed invalidation can leave stale data. For stringent requirements consider versioned keys, outbox/retry or a deliberate bypass; TTL alone is not immediate consistency.
- Local `Invalidate` and remote `Delete` are independent operations and do not automatically synchronize. Do not implement an L1+L2 stack merely because both skills are active.
- Avoid caching live ORM entities/sessions, open connections, request contexts, mutable cross-request pointers and unbounded buffers.

## Reliability / observability

- Distinguish cache hit, genuine miss, timeout, cancellation and backend outage. Implement bounded fallback/retry policy and avoid stampedes.
- Monitor effective hit ratio, miss load, latency, eviction, estimated size and retry/timeout rates using project-approved metrics without high-cardinality key labels.
- Profile retained Go memory and goroutine lifecycle when the implementation changes background work or buffer ownership; do not infer leaks from one RSS sample.
- Choose backend-specific failure and read-after-write semantics: NATS KV may serve stale reads in some topologies, while a single-process cache has no interprocess visibility.

## Coordination

Use `go-local-cache` for Otter v2 local caching and `go-nats-kv-cache` for direct `nats.go/jetstream`. These are **independent** activated skills and neither is a mandatory dependency of the other.

Load this reference only for caching design, keys, invalidation or consistency decisions. Do not copy Otter or NATS API instructions into this shared material.

