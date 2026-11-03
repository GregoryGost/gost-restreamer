# Errors, contexts, resources and lifecycle

Go version gates: `errors.Is`/`errors.As` and `%w` wrapping: Go 1.13+; `errors.Join` and `context.WithCancelCause`: Go 1.20+; `context.WithoutCancel`: Go 1.21+. For older targets, select compatible alternatives rather than raising the minimum version implicitly. Go snippets below default to Go 1.13+ unless their local heading specifies another minimum.

## 1. Error identity must survive wrapping

**BAD — comparing localized or contextual error messages** (CORRECTNESS for a contract requiring error identity):

```go
package store

import "errors"

var ErrMissing = errors.New("missing")

func IsMissing(err error) bool {
    return err != nil && err.Error() == "missing"
}
```

**GOOD — preserve causality and classify errors** (Go 1.13+):

```go
package store

import (
    "errors"
    "fmt"
)

var ErrMissing = errors.New("missing")

func Lookup(id string) (string, error) {
    if id == "" {
        return "", fmt.Errorf("lookup %q: %w", id, ErrMissing)
    }
    return "found", nil
}

func IsMissing(err error) bool {
    return errors.Is(err, ErrMissing)
}
```

Use a sentinel when callers need stable equality/`errors.Is` semantics. Typed errors are suitable when callers need structured details, inspected with `errors.As` (or `errors.AsType` only in Go 1.26+). Avoid exposing driver-specific error values from the domain boundary.

## 2. Translate errors once at the correct boundary

- Repository: database/driver errors to stable repository errors; avoid leaking SQL/driver details to services/transport.
- Service: distinguish domain failures and maintain transactional invariants; add context without destroying identity.
- HTTP/CLI: translate to user-facing status/message and log relevant operational context without exposing credentials or full queries.
- Do not log and return the same error repeatedly at each layer; avoid indiscriminate `fmt.Errorf("%v", err)` when callers rely on unwrapping.
- `errors.Join` (Go 1.20+) can preserve multiple cleanup failures; do not discard the primary error.

## 3. Propagate context to I/O; don't store a request context

**BAD — detaching an outbound HTTP call from the caller** (CORRECTNESS for cancellation contracts):

```go
package client

import (
    "context"
    "net/http"
)

func Fetch(_ context.Context, client *http.Client, url string) (*http.Response, error) {
    req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, url, nil)
    if err != nil { return nil, err }
    return client.Do(req)
}
```

**GOOD — honor the caller's cancellation** (Go 1.13+ for `NewRequestWithContext`):

```go
package client

import (
    "context"
    "net/http"
)

func Fetch(ctx context.Context, client *http.Client, url string) (*http.Response, error) {
    req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
    if err != nil { return nil, err }
    return client.Do(req)
}
```

The caller **owns the returned response body** and must close it. An HTTP client must still have project-appropriate timeout, redirect and resource limits. Never retain an HTTP request context in a long-lived service field. If work must outlive a request, define separate ownership, timeout, idempotency, retries, observability and durability; `context.WithoutCancel` is not automatically the correct solution.

## 4. Own resources and check result errors

**BAD — ignoring a response body prevents predictable reuse/cleanup**:

```go
package client

import (
    "net/http"
)

func Ping(client *http.Client, url string) error {
    _, err := client.Get(url)
    return err
}
```

**GOOD — close every successful response; avoid unbounded draining** (Go 1.0+):

```go
package client

import (
    "fmt"
    "net/http"
)

func Ping(client *http.Client, url string) error {
    resp, err := client.Get(url)
    if err != nil { return err }
    defer resp.Body.Close()
    if resp.StatusCode != http.StatusOK {
        return fmt.Errorf("unexpected HTTP status: %d", resp.StatusCode)
    }
    return nil
}
```

Closing the response body is mandatory even when the status is unexpected. Reusing a connection may benefit from consuming a response body, but unlimited draining of an untrusted body is not a safe default; define byte limits and timeouts instead. For files/transactions, check deferred flush/close/commit errors when the result depends on them, and preserve the primary error if cleanup also fails.

## 5. Explicit shutdown boundaries

- `http.Client` and its transport are generally reusable; do not create one for every request by default. When owning a custom transport, close idle connections during shutdown when appropriate.
- `sql.DB` is a concurrency-safe connection pool; close it at application lifetime end, not after each query.
- A goroutine started in application startup needs a stop signal, bounded waiting and error handling. HTTP request cancellation does not clean up independently owned background work.
- Define whether shutdown drains, rejects or abandons queued work; do not promise durability for an in-memory buffer without a replay mechanism.
- Avoid holding database transactions during remote calls, sleeps or expensive processing.

## Leak and ownership review

On early returns, verify that owned `http.Response.Body`, `*sql.Rows`, `*sql.Conn`, transactions, files and background tasks are released or stopped. Application-owned pools are not closed per request; the caller owns response bodies. For retention, heap-versus-RSS analysis, CGO memory and goroutine leak profiling, use `memory-and-resource-leaks.md`. Do not label an unreferenced timer as a universal leak: Go 1.23+ changed collection semantics, subject to module/runtime version.

## Review prompts

- Is the error identity preserved and translated at the correct boundary?
- Is caller cancellation observed without accidentally canceling durable work?
- Who owns each opened resource and its cleanup errors?
- Can HTTP reads/streams be unbounded or leak secret data?
- Is service shutdown observable, bounded, and safe for partial startup?

Official references: https://pkg.go.dev/errors ; https://pkg.go.dev/context ; https://pkg.go.dev/net/http ; https://pkg.go.dev/database/sql ; https://go.dev/blog/go1.13-errors

