# Go benchmark design, comparisons and performance regressions

Keep functional test oracles separate from performance measurements. This reference supplements `go-core/references/performance-engineering.md` only when both skills are active.

## Representative benchmark design

- Pick operations whose inputs and workload represent the claim. Include realistic sizes, success/error mix, concurrency and warm-up where relevant.
- Keep timed regions focused. Use `b.ResetTimer()` after one-time setup; put assertion/cleanup outside measured work where reasonable without hiding real costs. Use `b.ReportAllocs()` or `-benchmem` to observe allocation churn.
- Avoid benchmarking mocked persistence as evidence about real SQLite locks or remote HTTP/NATS latency. Benchmark the true boundary in isolated infrastructure when needed and permitted.
- When `b.RunParallel` is relevant, verify shared state, consistent contention, isolation and cleanup; do not assume results are independent under real global caches.

## Commands (replace package/target/output and check execution permission)

```sh
go test -run '^$' -bench '^BenchmarkEncode$' -benchmem -count=10 ./internal/codec
# If a project-approved benchstat executable already exists:
benchstat baseline.txt candidate.txt
```

Benchmark tests execute code and may create artifacts. Direct any output redirection to an **approved** temporary or artifact directory; examples do not authorize installing `benchstat`. The selected Go/testing versions determine supported flags.

## Interpreting results

- Preserve raw per-iteration results, Go/toolchain versions, architecture, OS, CPU settings, code revisions and test/environment conditions.
- Run repeated samples (prefer interleaved baseline/candidate measurements where practical) and use `benchstat` only when already available and authorized. Its significance labels reflect sampling assumptions, not a proof that production improves.
- Keep speed, allocations, memory-retention and correctness claims distinct. A faster benchmark can still introduce a data race, leak or API incompatibility.
- PGO requires representative CPU profiles; a local microbenchmark's runtime profile should not be assumed representative of a whole production service.

## Failure examples

**BAD:** one run reports 8% lower ns/op and the reviewer announces "8% faster in production".

**GOOD:** benchmark under comparable versions and workload across multiple runs, show effect size/noise and confirm affected correctness/race tests before a bounded performance claim.

Official sources: https://pkg.go.dev/testing#hdr-Benchmarks ; https://pkg.go.dev/golang.org/x/perf/cmd/benchstat ; https://go.dev/doc/pgo

