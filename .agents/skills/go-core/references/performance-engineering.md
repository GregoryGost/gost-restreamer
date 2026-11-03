# Go performance engineering and evidence-based optimization

Load this reference for measured latency, CPU, allocations, contention, throughput or resource regressions; it is **not** a mandate to optimize every code change.

## Measurement sequence

1. Define the performance claim and representative operation: input sizes, concurrency, data distribution, cache warm-up, environmental limits and which metric matters.
2. Establish the baseline toolchain, `go.mod`, platform, CGO, flags, runtime limits, code revision and workload. Preserve comparable conditions across candidate revisions.
3. For CPU/heap problems use authorized `go test -bench`, `runtime/pprof` and `go tool trace` as appropriate. Profiles may include sensitive symbols/request details; capture and store only in approved locations.
4. Attribute time/allocations to actual call paths and bottlenecks, then implement the smallest improvement preserving semantics, cancellation and error contracts. A sampled flame graph alone is not definitive proof of root cause.
5. Compare repeated statistically meaningful results and normal functionality tests; do not claim improvement from a single noisy timing run.

## Choose the right tool

| Question | Evidence |
| --- | --- |
| Local deterministic operation speed | `go test -bench` with representative data, `-benchmem` and repeat counts |
| Allocation growth vs retention | `alloc_space` vs `inuse_space`, `runtime/metrics`, repeated live-heap observations |
| CPU hot spots | CPU `pprof` collected with same workload; call graph and relevant source |
| Scheduler delays or goroutine contention | Go execution trace plus blocking/mutex profiles when enabled |
| Relative benchmark change | Approved `benchstat` CLI if available, or conservative manual comparison with sample counts |
| Production build tuning | Optional PGO profile from representative workload, with normal regression testing |

## PGO (opt-in)

Go has supported profile-guided optimization since Go 1.20, with automatic `default.pgo` behavior starting in Go 1.21. The compiler expects a CPU profile in the Go `pprof` format. Select the **exact** Go version and check `go help build` for actual support; `-pgo=off` disables PGO when a clean baseline is needed.

Do **not** automatically commit a CPU profile: it can expose process internals or workload details, while Git-based profile pinning improves reproducibility. The target project decides source control, profile provenance, regeneration, retention and approved artifact location.

**BAD:** enable PGO globally with a microbenchmark-generated profile and claim an app-wide speedup without a comparable workload.

**GOOD:** choose a representative owner-approved workload, collect comparable CPU profiles, build normal and PGO variants under controlled flags, test behavior and compare performance on actual target conditions. If the project has no meaningful representative profile, keep PGO disabled.

## Review checklist

- Is the objective and dataset explicit?
- Were correctness tests, race/lifecycle invariants and resource ownership preserved?
- Are the environment, CPU governor, runtime/GC, repeat samples and build settings comparable?
- Are labels, profiler output, benchmark data and PGO files within the authorized output policy?
- Could a simpler implementation or differing workload refute the claimed win?

Official sources: https://go.dev/doc/pgo ; https://go.dev/doc/diagnostics ; https://pkg.go.dev/runtime/pprof ; https://pkg.go.dev/golang.org/x/perf/cmd/benchstat

