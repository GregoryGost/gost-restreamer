# Go core rules

This is a framework-neutral language rule. Activate only through the exact `go_core.md` entry in Active Rules, `go-core` in Active Stack Profiles, or an enabled Go profile that names the `go_core.md + go-core` pair. A task mentioning Go alone does not override target-project activation. Template maintenance may edit this source rule directly.

## Required skills

- `go-core`

## Source of truth

1. Read `CODEX_PROJECT.md`, `go.mod`, `go.work` when present, `go.sum`, toolchain configuration, relevant source and test files.
2. Resolve the effective Go language and toolchain versions independently; do not infer runtime support from current online examples.
3. Inspect real package boundaries, exported APIs, build tags, platform and CGO constraints, dependency policy and project-declared commands. Resolve the already selected `gopls` connection/mode, analyzer configuration, diagnostics policy and check scope from `CODEX_PROJECT.md` when enabled; never invent project-wide defaults.
4. Add framework/database/testing materials only when independently active and relevant.

## Expert review workflow

- Read the matching `go-core/references/` material when the task touches OOP/interfaces, package/API design, errors/lifecycle, startup/shutdown, performance/PGO, module/build maintenance, memory/resource retention, terminal runtime debugging, language-server diagnostics/semantic code navigation or new language features. For broad code review, start with `references/patterns-and-review.md` and load focused references as needed.
- Treat compiler/runtime correctness, security, compatibility, design tradeoffs and style as distinct categories; evidence is required for every asserted defect.
- Compare concrete alternatives and attempt to disprove findings against full relevant code and callers; never classify a valid context-dependent idiom as a universal anti-pattern.
- Prefer the versioned Go specification/release notes over older articles on language changes; validate syntax/library availability against the module language level and installed build toolchain.

## Constraints

- Keep package responsibilities cohesive; avoid cyclic imports and generic catch-all packages.
- Follow `gofmt`, `go vet` and project-selected linters. Do not introduce extra lint/build dependencies by default.
- When the project enables `gopls` diagnostics, follow `go-core/references/gopls-language-server.md`. Verify installed version and available interface; prefer the **actually connected** official MCP for semantic context and diagnostics, otherwise the declared LSP or CLI fallback. For MCP `go_diagnostics`, request workspace diagnostics with the affected Go file paths where the advertised schema supports them; a workspace-only call need not include the deeper file-level analyzers. Never assume the assistant can see VS Code Problems or unsaved editor buffers.
- On source changes, inspect new diagnostic findings under the **project-selected** analyzer/severity policy and repeat required checks on the final source snapshot. Treat pre-existing issues and nonblocking hints separately; no blanket `fixAll`, analyzer suppression, new dependency or unrequested refactoring. If a required `gopls` check cannot run or its scope/version is uncertain, report it as **NOT VERIFIED** to the primary before Validation Gate; do not claim a clean IDE/workspace based only on `go vet` or passing tests.
- Wrap meaningful error context with `%w`; use `errors.Is` / `errors.As` for contractual errors. Do not suppress errors or panic for routine failures.
- Pass `context.Context` explicitly for request-scoped I/O and cancellation; do not store request contexts in long-lived structs.
- Make ownership of connections, files, goroutines, channels and shutdown explicit.
- For long-lived services or code that acquires resources, check early-return cleanup, worker cancellation/join, bounded queues/caches and accidental retention. Use `go-core/references/memory-and-resource-leaks.md` for diagnosis. A growing RSS or allocation counter alone does not prove a leak; Go heap profiles do not account for all CGO/native allocations.
- Do not start unbounded/background goroutines without cancellation, error observation and drain/stop behavior.
- Protect shared mutable data by ownership, synchronization or message passing; do not infer race freedom from test success.
- Prefer concrete types and small interfaces when substitution is real; use composition rather than simulating class inheritance. Choose value or pointer receivers by mutation, copyability and method-set requirements. Avoid **unnecessary** interfaces, generic abstractions, reflection and dependency injection frameworks, but allow justified uses.
- For service lifecycle changes, consult `go-core/references/service-lifecycle.md`: use owned startup and independent shutdown contexts, bounded resource drain and project-declared readiness policy. Do not add health endpoints by default.
- For performance work, consult `go-core/references/performance-engineering.md`: require comparable measurements, not optimization claims from one benchmark. PGO and extra profilers are opt-in.
- For module/build changes, consult `go-core/references/modules-and-build.md`: distinguish read-oriented inspection from mutating commands (`go mod tidy`, `go get`, `go generate`). Do not change dependencies or toolchain without authorization.
- Do not mutate generated files without changing their authoritative generator input.
- When authorized runtime investigation needs breakpoints or goroutine/variable inspection, prefer the existing terminal Delve (`dlv`) following `go-core/references/runtime-debugging.md`. Verify compatible Go/Delve versions, persistent terminal input and process/build side effects; never install, attach to running services or expose remote debugger listeners by default. Debugger observations are not passing tests.
- Validate changes with project-declared checks; report unexecuted or unavailable commands.

