# Version-aware modern Go

**Do not equate latest Go with the target module's language level.** Before using a feature, inspect `go.mod`'s `go` line, optional `toolchain` line, actual `go version`, platform/build tags and CI/runtime builds. A modern library API may exist only in a newer standard library, even if the syntax is otherwise valid.

## Verified feature gates

| Feature | Minimum Go | Notes |
| --- | --- | --- |
| Type parameters for functions/types | 1.18 | Use for genuine type-safe reuse, not every helper. |
| `errors.Join` / `context.WithCancelCause` | 1.20 | Preserve meaningful multiple errors or cancellation causes. |
| `slices`, `maps` standard packages | 1.21 | Verify the particular function introduced in that version or later. |
| `for range` over integers | 1.22 | `for i := range n` produces 0 through n-1. |
| Per-iteration loop variables | language level 1.22 | Controlled by module/file language version, not merely installed toolchain. |
| `iter.Seq`, range-over-function | 1.23 | Do not rewrite a simple slice loop merely for novelty. |
| `testing/synctest.Test` and `sync.WaitGroup.Go` | 1.25 | Stable synctest API; `WaitGroup.Go` callback must not panic. |
| `errors.AsType` and `new(expression)` | 1.26 | Use only when target source/compiler supports them. |
| Generic concrete methods | 1.27 | Method may declare its own type parameters; interface methods still cannot and generic methods cannot implement interface methods. |
| `encoding/json/v2` | 1.27 | Strict new API/semantics; review JSON compatibility, tags and external dependencies before adopting. |
| `runtime/pprof` `goroutineleak` profile | 1.27 | Stable profiler for some permanently blocked goroutines; not all I/O stalls. Go 1.26 used an experimental build feature. |

This table lists the **first Go version** for the stated capability, not a recommendation to upgrade or a complete security-support policy. The generic-function example below requires Go 1.18+; the generic-method example is intentionally Go 1.27+ only and cannot be checked with an older compiler.

## 1. Don't change behavior by assuming a new loop variable rule

**BAD — asserting that a newer compiler automatically changes all old modules** (CORRECTNESS/MAINTENANCE):

```text
Compiler is Go 1.27 => closures in every imported module use Go 1.22 loop semantics.
```

**GOOD — verify the module/file language level**:

```text
Read go.mod's go directive for the package's module, then consider per-file build constraints.
Check each affected module in a go.work workspace separately.
```

For Go 1.22+ language semantics the loop variables declared by `for` are per iteration. Legacy modules can retain older semantics, and each module can have a different language level. Do not add `v := v` mechanically without confirming the actual module version and capture use.

## 2. Generic methods are not general interface-method generics (Go 1.27+)

A separate generic function remains available on earlier supported Go versions (Go 1.18+):

```go
package boxes

type Box[T any] struct{ Value T }

func Map[T, U any](b Box[T], f func(T) U) Box[U] {
    return Box[U]{Value: f(b.Value)}
}
```

On Go 1.27+, a method on a concrete type may declare its own type parameter (verify the pinned compiler before using this syntax):

```go
package boxes

type Box[T any] struct{ Value T }

func (b Box[T]) Map[U any](f func(T) U) Box[U] {
    return Box[U]{Value: f(b.Value)}
}
```

This syntax does **not** allow generic methods inside an interface, nor generic method implementations of interface methods. The method example cannot be compiled by a pre-1.27 toolchain. Don't introduce it into projects that target older Go versions.

## 3. Modernization is a choice, not a blanket rewrite

- `goroutineleak` is an optional Go 1.27 diagnostic, not a mandatory endpoint or proof that all goroutine leaks are absent. Restrict `net/http/pprof` to explicitly protected diagnostic access; see `memory-and-resource-leaks.md`.
- Go 1.23 introduced collection of unreferenced timers/tickers when the main module and runtime select the new semantics. Go 1.27 removes the old `asynctimerchan` override. `Stop` can still matter for behavioral cancellation; do not flag every timer without `Stop` as a leak.

- Prefer modern standard-library helpers when they make the contract clearer and fit the minimum Go version. Avoid rewriting clear loops to `iter` pipelines without justification.
- `go fix` in modern versions may rewrite files; inspect the tool's version and proposed changes, and run it only when the current request authorizes source changes.
- `encoding/json/v2` (Go 1.27+) has different validation and semantics from `encoding/json` v1. Do not change serialized contracts or migrate automatically.
- Check behavior of reused abstractions, especially `slices.Clone` nil handling, `maps.Keys` iterator semantics from Go 1.23, and each API's actual package documentation.
- Generic-method availability in Go 1.27 supersedes older FAQ text claiming that Go will never have generic methods. Prefer the versioned 1.27 release notes and language specification for this question.

## Verification

- For standard-library calls, use `pkg.go.dev/<package>?tab=doc` or matching installed source for the target toolchain.
- Test syntax with the chosen `go` version; document when the local validation toolchain is too old for a feature.
- Run `go vet` and relevant behavioral tests where code is actually changed, respecting project-declared commands.
- Don't claim portability across Windows/Linux/ARM/CGO based only on a successful local Linux build.

Official sources: https://go.dev/doc/go1.27 ; https://go.dev/blog/generic-methods ; https://go.dev/doc/go1.26 ; https://go.dev/doc/go1.25 ; https://go.dev/doc/go1.23 ; https://go.dev/doc/go1.22 ; https://go.dev/ref/mod

