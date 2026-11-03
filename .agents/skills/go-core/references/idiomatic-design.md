# Idiomatic Go decisions and Good/Bad examples

These defaults are **conditional engineering heuristics**. Repository contracts, clarity, measured evidence and real callers take priority over importing conventions from another language. Unless marked otherwise, snippets use Go 1.0+; `slices.Clone` needs Go 1.21+ and the generic interface example needs Go 1.18+.

## 1. Constructors versus useful zero values

**BAD — constructor ceremony for a usable zero value** (DESIGN; Go 1.0+):

```go
package tally

type Counter struct{ count int }

func NewCounter() *Counter { return &Counter{} }
```

A constructor is not *wrong*, but unnecessary if it does nothing, enforces no invariant and ownership policy does not require it.

**GOOD — useful zero value** (DESIGN; Go 1.0+):

```go
package tally

type Counter struct{ count int }

func (c *Counter) Add(n int) { c.count += n }
func (c *Counter) Value() int { return c.count }
```

Use a constructor when validation, configuration, dependencies, private invariants or resource creation require it. A zero value cannot be promised for every type (for example, an open DB connection).

## 2. Explicit dependencies instead of global mutable state

**BAD — hidden mutable global dependency** (DESIGN/TESTABILITY):

```go
package worker

import "log"

var Logger = log.Default()

func Process() { Logger.Print("processed") }
```

**GOOD — inject an explicit dependency with a coherent lifecycle**:

```go
package worker

import "log"

type Worker struct {
    logger *log.Logger
}

func New(logger *log.Logger) *Worker { return &Worker{logger: logger} }

func (w *Worker) Process() {
    w.logger.Print("processed")
}
```

Validate nil if it is possible at the public boundary. Do not introduce a DI framework or an interface just to inject one concrete logger.

## 3. Functional options only when they pay for themselves

**BAD — compulsory generic option machinery for one setting** (DESIGN; valid Go):

```go
package server

type Config struct{ Port int }
type Option func(*Config)

func WithPort(port int) Option {
    return func(c *Config) { c.Port = port }
}
```

**GOOD — a typed configuration when callers benefit from explicit values** (Go 1.0+; independently compilable):

```go
package server

import "errors"

var ErrInvalidPort = errors.New("invalid port")

type Config struct{ Port int }
type Server struct{ port int }

func New(config Config) (*Server, error) {
    if config.Port < 1 || config.Port > 65535 {
        return nil, ErrInvalidPort
    }
    return &Server{port: config.Port}, nil
}
```

Options are appropriate with many optional independent settings, stable defaults or backward-compatible extension needs. Avoid a rule that forces either style universally.

## 4. Slices, maps and ownership

**BAD — assuming `append` cannot modify an input's backing array** (CORRECTNESS):

```go
package example

func AppendTag(tags []string) []string {
    return append(tags, "new")
}
```

This function is fine if mutation of the caller's backing storage is allowed, but is unsafe when the contract promises isolation.

**GOOD — detach when ownership requires it** (Go 1.21+ for `slices.Clone`):

```go
package example

import "slices"

func AppendTag(tags []string) []string {
    copyOfTags := slices.Clone(tags)
    return append(copyOfTags, "new")
}
```

For earlier Go versions, use `append([]string(nil), tags...)` or `make` + `copy`. A nil slice and empty non-nil slice can serialize differently (for example `null` versus `[]` under `encoding/json`); model the public contract explicitly.

## 5. Generics and abstractions

**BAD — abstraction for a single concrete use without a need** (DESIGN; Go 1.18+):

```go
package ids

type Identifiable[T any] interface { ID() T }

func IDs[T any, E Identifiable[T]](values []E) []T {
    out := make([]T, 0, len(values))
    for _, value := range values { out = append(out, value.ID()) }
    return out
}
```

This may be legitimate reusable library code; when there is one known `User` type, a direct loop is easier to understand. Do not blanket-ban `any`, generics, reflection or functional options: require a concrete benefit and compatible Go version.

## Review prompts

- Does a constructor enforce a meaningful invariant or own resources?
- Are dependencies explicit, initialized and torn down correctly?
- Are abstractions justified by real callers rather than hypothetical futures?
- Does the API define mutation/ownership of maps, slices, bytes and pointers?
- Do exported JSON/time/money/nullability contracts remain stable?
- Is the recommendation evidence-backed or merely a preference?

Further reading: https://google.github.io/styleguide/go/best-practices.html ; https://go.dev/doc/effective_go ; https://go.dev/wiki/CodeReviewComments

