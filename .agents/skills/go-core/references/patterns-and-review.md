# Go expert review and Good/Bad decision protocol

This is the **entry checklist** for code review and refactoring, not a second copy of every language rule. Load the focused reference for the relevant domain and inspect **complete** code, callers, tests and configuration before raising a finding.

## Severity and evidence

| Category | Example | Required review evidence |
| --- | --- | --- |
| CORRECTNESS | Copying `sync.Mutex`, unsynchronized writes, broken error mapping | Concrete path and operation, invariant, reproduction or language/tool proof. |
| SECURITY | Unvalidated authorization, unbounded attacker-controlled I/O, secret leakage | Trust boundary, data flow and realistic abuse route; no speculation. |
| COMPATIBILITY | Go 1.27-only syntax in a Go 1.23 module; changed exported interface | Actual `go.mod` / build matrix, caller breakage or compiler evidence. |
| DESIGN | Premature interface, package cycles, unclear dependency ownership | Existing callers, tradeoffs and simpler alternative; explain why it matters. |
| STYLE | Naming, receiver spelling, optional formatting conventions | Current project convention or official guideline; do not label as a defect. |

A guideline from the Google Go Style Guide is useful evidence for idiomatic style, but it is **not** part of the Go language specification and does not automatically override project-local conventions. Do not invent test results, API versions or measured performance.

## Quick anti-pattern index

1. Java/C# class inheritance transposed into embedding without considering promoted API: `oop-and-interfaces.md`.
2. Interface per struct or unused abstraction for mocking: `oop-and-interfaces.md` and `idiomatic-design.md`.
3. Value receiver copying `sync.Mutex`: `oop-and-interfaces.md` (Go vet `copylocks`).
4. Comparison of error text instead of `errors.Is`/`errors.As`: `errors-and-lifecycle.md`.
5. `context.Background()` inside request I/O, forgotten response bodies or unmanaged work: `errors-and-lifecycle.md`.
6. Using channels for simple protected state, data races or never-stopping workers: `concurrency-and-memory.md`.
7. Returning aliased slices from API intended to promise immutability: `concurrency-and-memory.md`.
8. Catch-all packages, unintended export and speculative multi-layer architecture: `packages-and-api-design.md`.
9. New language/API syntax without checking `go.mod` and CI: `modern-go.md`.
10. Using new generics/iterators mechanically rather than to reduce real complexity: `modern-go.md` and `idiomatic-design.md`.
11. Unbounded caches/queues, dangling goroutines, unreleased connections, large slice views retaining backing arrays: `memory-and-resource-leaks.md`.

## Review workflow

1. **Context:** read `CODEX_PROJECT.md`, `go.mod`/`go.work`, relevant packages, tests, generated-code ownership, lint/test commands, Go minimum and CGO/build matrix.
2. **Contracts:** reconstruct public APIs and actual consumers, data ownership, error identities, context propagation, goroutine lifecycle and module boundaries.
3. **Alternatives:** seek the smallest correct design; compare a concrete type against an interface, value receiver against pointer, channels against mutex, direct code against a generic abstraction.
4. **Refutation:** try to disprove each potential finding via callers, compatibility tests and real configuration. Distinguish proven defects from hypotheses and preferences.
5. **Evidence:** include file/line, concrete failure mode, supported version, official source or relevant test, and a bounded fix.
6. **Validation:** run only approved project commands; report what executed versus what remains unverified.

## Good / Bad snippets must be trustworthy

- Every sample identifies the **minimum Go version** through a file-level default or an explicit block label, with higher requirements and dependencies called out. Mark a snippet as standalone or abbreviated.
- Prefer two self-contained examples that each compile; a deliberately uncompilable sample must be marked **compile error** and say why.
- Separate BAD labels for **correctness** from a **style/design tradeoff** valid in other contexts; do not manufacture strawmen.
- Include explanation of behavior, when the good pattern is preferable, counterexamples and a way to validate (compiler, `go vet`, `-race`, tests or benchmark).
- Avoid external dependencies in language-only examples; keep Huma/GORM/Goose advice in their active specialized skills.
- Do not add runnable sample source files to the template repository without an explicit reason; references with fenced snippets are source documentation.

## Acceptance checklist

- [ ] Effective Go version, module/workspace, platform and dependency policy checked.
- [ ] Public API, invariants, ownership, cancellation and errors reviewed.
- [ ] Interfaces and composition are proportionate to real callers.
- [ ] Concurrent mutation synchronized; goroutine lifecycle bounded; resources cleaned.
- [ ] For affected long-running/resource paths, retained heap, background work and native/CGO allocations are evaluated separately; leak findings supported by comparable evidence.
- [ ] Suggestions labeled correctness/security/compatibility/design/style with proof.
- [ ] New syntax and tools version-gated; no unjustified dependency/tool installation.
- [ ] Appropriate active testing/framework/DB overlays consulted without duplication.
- [ ] Tests/commands executed or the limitation stated; no invented performance claims.

