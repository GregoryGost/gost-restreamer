---
name: go-core
description: "Use for idiomatic Go architecture, object modeling, methods, interfaces, errors, context, concurrency, version-aware APIs, performance and critical code review."
---

# Go Core — Idiomatic Go Engineering

This is the **language-level expert skill** for Go. It covers correct, maintainable and idiomatic Go implementation and review, including modeling without class inheritance. It is not a separate `go-expert` role: deep guidance lives in focused `references/` and is activated through this existing skill.

HTTP/Huma, GORM/SQLite/Goose and testing framework specifics belong to separately active skills; this base skill must not duplicate or implicitly activate them.

## Activation and coordination

- Inspect `CODEX_PROJECT.md`, `go.mod`, `go.work` if present, source, build tags, CI and the declared active stack. Select only active rules and skills using `AGENTS.md` and `.codex/rules/material_dependencies.md`.
- Use `.codex/rules/go_core.md` when it is independently active. This skill does not activate that rule or any optional overlay by itself.
- When a target project selects only `go-core` without the rule, respect that active selection and do not silently create an undeclared dependency.
- Obey project language, code-comment policy, existing conventions, dependency policy, mutation scope and approved validation commands.

## Required investigation before an expert judgment

1. Identify the actual task: behavior change, refactor, API evolution, concurrency bug, performance question, compatibility review or idiomatic style review.
2. Read the **complete relevant** packages, public contracts, consumers, existing tests and configuration. Use search only to locate evidence.
3. Resolve `go.mod` language version, possible `toolchain` directive, installed compiler, platform/CGO/build matrix and module versions. Do not equate latest online documentation with available syntax.
4. Reconstruct invariants, state ownership, error mapping, cancellation, goroutine/resource lifecycle, package boundaries and observable behavior.
5. Compare at least one simpler design where material: concrete type versus interface; value versus pointer receiver; composition versus embedding; mutex versus channel; direct function versus generics.
6. Try to disprove tentative findings against actual consumers, tests and official versioned sources; mark what is verified and what remains unknown.
7. Implement only authorized changes; use repository-defined formatter/linter/build/test commands and report what ran.

## Reference routing — load only relevant material

| Scope | Read before design/review |
| --- | --- |
| Structs, OOP concepts, receivers, interfaces, typed nil, embedding | `references/oop-and-interfaces.md` |
| Zero-value, constructors, options, dependency injection, slices, generics tradeoffs | `references/idiomatic-design.md` |
| Packages, import direction, public APIs, compatibility, module layout | `references/packages-and-api-design.md` |
| Module graph, go.work, toolchain selection, cross-builds, CGO, build tags and generated code | `references/modules-and-build.md` |
| Error types, wrapping, context propagation, resource ownership, shutdown | `references/errors-and-lifecycle.md` |
| Application startup, signal-driven cancellation, graceful shutdown and readiness | `references/service-lifecycle.md` |
| Data races, synchronization, goroutine ownership, channels, performance | `references/concurrency-and-memory.md` |
| Evidence-led CPU/heap/trace profiling, benchmarks, PGO and performance regressions | `references/performance-engineering.md` |
| Memory retention, goroutine/resource leaks, heap/RSS/CGO diagnostics, pprof | `references/memory-and-resource-leaks.md` |
| Terminal debugging of failing tests or local Go processes with Delve (`dlv`) | `references/runtime-debugging.md` |
| Cache eligibility, keys, consistency, invalidation and failure boundaries | `references/cache-design.md`, only for cache-related tasks |
| New language/library features, deprecations and module-level behavior | `references/modern-go.md` |
| General code review, refactor audit or multi-category request | `references/patterns-and-review.md` plus the matching focused references |
| Any version-sensitive or disputed claim | `references/official-sources.md`; then check the actual official source |

Do not load every reference for a one-line change; do not skip the relevant reference merely because a general summary was already loaded.

## Engineering defaults, not absolutes

- Model data with named types, focused methods and explicit invariants. Prefer composition and explicit ownership; Go embedding is method/field promotion, not subclassing.
- Prefer concrete types and narrow consumer-owned interfaces where they bring meaningful substitution. Existing framework interfaces and library boundary contracts are legitimate exceptions.
- Use pointer receivers for mutable/non-copyable state; value receivers can be appropriate for small immutable value-like types. Verify method sets and interface satisfaction.
- Make zero values useful where sensible; constructors are justified for validation, configuration, dependencies and resource ownership.
- Avoid interfaces, generics, reflection, functional options, DI frameworks or extra layers with no current value, but do not ban them categorically.
- Propagate `context.Context` into cancelable I/O; wrap and classify errors at deliberate boundaries; do not leak driver errors to transport or suppress failures.
- Bound goroutine lifetimes and queue capacity, synchronize shared mutable state and separate correctness from performance guesses.
- For long-running work and changes that affect resource ownership, check cancellation/join, cleanup on all exit paths, bounded retention and appropriate memory diagnostics. Distinguish live Go heap, native/CGO memory and process RSS; do not require profiling for unrelated small changes. See `references/memory-and-resource-leaks.md`.
- For reproducible failures needing runtime state, optionally use terminal-only Delve via `references/runtime-debugging.md`, but only after verifying the toolchain, debugger availability, interactive input and allowed side effects. Debugging is not a required step for routine tasks.
- Apply a target's established application role, operational boundary and disclosure policy from `CODEX_PROJECT.md` without inventing new assumptions on each task. Choose `references/service-lifecycle.md`, `references/performance-engineering.md` and `references/modules-and-build.md` for relevant work; no mandatory profiler, new process, dependency upgrade or PGO.
- Keep package imports acyclic and exported contracts intentional. Do not introduce new dependencies, tools, language-version upgrades or source rewrites without the active project policy/user authorization.
- `gofmt` is the standard baseline; apply additional local linting/testing tools only when already declared and permitted.

## Review output contract

Classify each actionable finding as **CORRECTNESS**, **SECURITY**, **COMPATIBILITY**, **DESIGN** or **STYLE**. State:

- exact source path/line or API surface;
- observable failure/invariant/trust boundary;
- Go version/module/build constraints where applicable;
- evidence from complete code, test, vet, race analysis or a matching official source;
- smallest safe remedy and any tradeoff or plausible counterexample;
- verification actually performed, with unavailable checks called out.

Do not present a preference (e.g. "always use interfaces"/"always use pointer receivers"/"always use channels") as universal correctness.

## Optional coordination

- `go-testing` for independently active Go tests and test engineering.
- `go-huma-v2` for typed Huma HTTP contracts when that profile is active.
- `go-gorm` for active GORM persistence, with `go-gorm-sqlite-goose` only when the exact SQLite/Goose stack is selected.
- `go-local-cache` for the independently active Otter v2 in-process cache; `go-nats-kv-cache` for the independently active direct NATS JetStream KV cache. Neither activates the other and no shared abstraction/layer is implied.
- `go-observability` for declared application logs, metrics and tracing; `go-security` for selected threat/control policy; `go-http-client` for outgoing `net/http` contracts. Each is independent and references the same project disclosure policy where relevant.

This skill remains framework-neutral and does not select DB, router, migration strategy or runtime toolchain for a target project.

