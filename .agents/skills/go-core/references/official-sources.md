# Authoritative Go sources and precedence

## Evidence hierarchy

1. **Language and semantic correctness:** version-matched Go specification and release notes; `go.mod` language level, actual toolchain and target build constraints.
2. **Standard-library behavior:** version-matched `pkg.go.dev` docs and package implementation/contract. Newer examples may not compile against the target.
3. **Go module/toolchain mechanics:** module reference and current Go command docs.
4. **Idiomatic design:** Google Go Style Guide, Go Code Review Comments and Effective Go; treat stylistic guidance as conditional, below project-specific conventions and correctness requirements.
5. **Blogs/tutorials:** explanatory context; validate version-sensitive claims against (1)–(3) before adopting.

## Official links

- Language specification: https://go.dev/ref/spec
- Memory model: https://go.dev/ref/mem
- Release notes index: https://go.dev/doc/devel/release
- Go 1.27 release notes (generic methods, json/v2): https://go.dev/doc/go1.27
- Go 1.26 release notes: https://go.dev/doc/go1.26
- Go 1.25 release notes (synctest, WaitGroup.Go): https://go.dev/doc/go1.25
- Go 1.23 release notes (iterators): https://go.dev/doc/go1.23
- Go 1.22 release notes (loop variables): https://go.dev/doc/go1.22
- Generic methods Go 1.27: https://go.dev/blog/generic-methods
- Go modules: https://go.dev/ref/mod
- Module layout: https://go.dev/doc/modules/layout
- Go command: https://pkg.go.dev/cmd/go
- Standard library API index: https://pkg.go.dev/std
- Errors: https://pkg.go.dev/errors
- Context: https://pkg.go.dev/context
- Sync: https://pkg.go.dev/sync
- Go race detector: https://go.dev/doc/articles/race_detector
- Diagnostics and profiling: https://go.dev/doc/diagnostics
- Official gopls language server diagnostics and CLI: https://go.dev/gopls/features/diagnostics
- Official gopls experimental built-in MCP setup, modes and side effects: https://go.dev/gopls/features/mcp
- gopls configuration, build flags and analyzers: https://go.dev/gopls/settings
- PGO: https://go.dev/doc/pgo
- Module and workspace semantics: https://go.dev/ref/mod
- Go toolchain selection: https://go.dev/doc/toolchain
- OS signal cancellation: https://pkg.go.dev/os/signal
- HTTP server shutdown semantics: https://pkg.go.dev/net/http#Server.Shutdown
- Benchmark comparison (optional benchstat): https://pkg.go.dev/golang.org/x/perf/cmd/benchstat
- Delve terminal debugger and getting started: https://github.com/go-delve/delve/blob/master/Documentation/cli/getting_started.md
- Delve CLI help and commands: https://github.com/go-delve/delve/blob/master/Documentation/cli/README.md
- Delve `dlv debug`: https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_debug.md
- Delve `dlv test`: https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_test.md
- Delve `dlv exec`: https://github.com/go-delve/delve/blob/master/Documentation/usage/dlv_exec.md
- GC and process memory accounting: https://go.dev/doc/gc-guide
- Runtime memory and goroutine metrics: https://pkg.go.dev/runtime/metrics
- Runtime heap / goroutine / goroutineleak profiles: https://pkg.go.dev/runtime/pprof
- HTTP profiling endpoint security: https://pkg.go.dev/net/http/pprof
- Goroutine leak profiler limits (Go 1.27): https://go.dev/blog/goroutine-leak-profiles
- Go 1.23 timer behavior: https://go.dev/doc/go1.23
- SQL resource ownership: https://pkg.go.dev/database/sql
- Go code review comments: https://go.dev/wiki/CodeReviewComments
- Effective Go (older, not a complete modern Go style guide): https://go.dev/doc/effective_go
- Google Go Style Guide: https://google.github.io/styleguide/go/guide
- Google Go Style Decisions: https://google.github.io/styleguide/go/decisions
- Google Go Best Practices: https://google.github.io/styleguide/go/best-practices

**Documentation drift:** at the time these materials were prepared, parts of the Go FAQ still asserted that methods with their own type parameters are unsupported. This conflicts with Go 1.27 release notes. For a Go 1.27 module, prioritize the release notes and current specification; do not propagate older FAQ assertions.

Official guidance is not necessarily a universal linter-enforced rule. The target project's version, API stability and local conventions govern implementation decisions.

