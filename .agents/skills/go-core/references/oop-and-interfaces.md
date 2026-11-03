# Go object modeling, methods and interfaces

## Applicability and evidence

Go models behavior with named types, methods, interfaces and composition, not class inheritance. Embedding promotes fields/methods but is **not** subtype inheritance. Check the effective `go` directive before using new syntax. Unless a block specifies a higher version, Go snippets here require Go 1.0+. Examples involving generics or newer packages must state their own minimum.

Decision categories: **CORRECTNESS** (compiler/runtime/data invariant), **DESIGN** (maintainability/ownership tradeoff), **STYLE** (local preference). Do not turn a style guideline into a correctness finding.

## 1. Encapsulate invariants without a class hierarchy

**BAD — public mutable fields allow invalid state** (Go 1.0+; compiles but violates the example's invariant):

```go
package account

type Account struct {
    BalanceCents int64
}

func (a *Account) Withdraw(cents int64) {
    a.BalanceCents -= cents
}
```

**GOOD — private state and explicit valid transitions** (Go 1.13+; `errors.New` is older):

```go
package account

import "errors"

var ErrInsufficientFunds = errors.New("insufficient funds")

type Account struct {
    balanceCents int64
}

func NewAccount(initial int64) (*Account, error) {
    if initial < 0 {
        return nil, errors.New("negative initial balance")
    }
    return &Account{balanceCents: initial}, nil
}

func (a *Account) Withdraw(cents int64) error {
    if a == nil || cents <= 0 || cents > a.balanceCents {
        return ErrInsufficientFunds
    }
    a.balanceCents -= cents
    return nil
}

func (a *Account) BalanceCents() int64 {
    return a.balanceCents
}
```

The example assumes no concurrent access and uses one error for invalid withdrawals for brevity. For a public API, distinguish invalid input from insufficient balance if callers require that contract. It intentionally does **not** add a mutex: a concurrency policy must be selected first.

## 2. Receiver methods, method sets and copying locks

**BAD — copying a structure containing `sync.Mutex`** (Go 1.0+; compiles, `go vet -copylocks` reports a defect):

```go
package counter

import "sync"

type Counter struct {
    mu sync.Mutex
    n  int
}

func (c Counter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.n++
}
```

**GOOD — pointer receiver updates the protected original** (Go 1.0+):

```go
package counter

import "sync"

type Counter struct {
    mu sync.Mutex
    n  int
}

func (c *Counter) Inc() {
    c.mu.Lock()
    defer c.mu.Unlock()
    c.n++
}

func (c *Counter) Value() int {
    c.mu.Lock()
    defer c.mu.Unlock()
    return c.n
}
```

Reasons: a value receiver copies the receiver (and therefore copies the lock); the original counter is not updated. Pointer receivers are required when mutation or non-copyable synchronization state is involved. For small, immutable, naturally value-like types a value receiver can be correct. Keep receiver choices consistent unless method-set requirements justify mixing.

Method sets: for a defined non-interface type `T`, `T` has methods declared with receiver `T`; `*T` also has methods declared with receiver `*T`. Automatic pointer-taking in a method call on an addressable value does not mean `T` satisfies an interface requiring a pointer-receiver method. Verify the actual type and interface assignment.

## 3. Interfaces describe behavior, not ancestry

**BAD — implementation-side interface created without consumers** (DESIGN; valid Go):

```go
package storage

type Store interface {
    Save(string) error
}

type SQLStore struct{}

func (SQLStore) Save(string) error { return nil }

func NewStore() Store { return SQLStore{} }
```

**GOOD — concrete implementation, narrow consumer-owned interface** (DESIGN; each block is a separate package):

```go
package storage

type SQLStore struct{}

func (SQLStore) Save(string) error { return nil }

func NewSQLStore() *SQLStore { return &SQLStore{} }
```

```go
package notifier

type Saver interface {
    Save(string) error
}

type Notifier struct {
    store Saver
}

func New(store Saver) *Notifier { return &Notifier{store: store} }

func (n *Notifier) Notify(message string) error {
    return n.store.Save(message)
}
```

Consumer-owned interfaces are a useful default when polymorphism/test seams have real value, not a universal ban on interfaces defined by framework contracts or libraries. Return concrete types when exposing more capabilities later matters; do not add an interface solely to permit mocking.

## 4. Embedding is promotion, not inheritance

**BAD — unintentional public promotion** (DESIGN):

```go
package api

import "net/http"

type Client struct {
    *http.Client
}
```

This exposes methods like `Do` and a potentially nil embedded field as part of `Client`'s API. It may be intentional for an adapter, but do not mistake it for an is-a relationship or a safe shortcut for composition.

**GOOD — explicit composition controls exposed methods** (DESIGN):

```go
package api

import (
    "net/http"
)

type Client struct {
    httpClient *http.Client
}

func New(httpClient *http.Client) *Client {
    return &Client{httpClient: httpClient}
}

func (c *Client) Do(req *http.Request) (*http.Response, error) {
    return c.httpClient.Do(req)
}
```

The example requires a non-nil injected client; production constructors should validate or supply a documented default. An embedded field is justified only when promoted API behavior is actually desired.

## 5. Typed nil in an interface

**BAD — non-nil interface containing a nil pointer** (CORRECTNESS):

```go
package example

type Worker interface{ Run() }

type job struct{}
func (*job) Run() {}

func BuildWorker() Worker {
    var j *job
    return j
}
```

`BuildWorker() != nil` is true because the interface holds a dynamic type (`*job`) even though the dynamic value is nil. An interface cannot be treated as nil based solely on a typed pointer payload.

**GOOD — return a nil interface on absence** (CORRECTNESS):

```go
package example

type Worker interface{ Run() }

type job struct{}
func (*job) Run() {}

func BuildWorker(found bool) Worker {
    if !found {
        return nil
    }
    return &job{}
}
```

## Review prompts

- Is a state invariant protected by API design and appropriate synchronization?
- Are receiver choices correct for copying, mutation and interface method sets?
- Is each interface needed by a real consumer? Could a concrete type suffice?
- Is embedding deliberate API promotion rather than inheritance imitation?
- Can a typed-nil interface, zero value or nil dependency escape to callers?
- Is a recommendation a correctness constraint, a design tradeoff or a style preference?

Official references: https://go.dev/ref/spec ; https://go.dev/wiki/CodeReviewComments#interfaces ; https://google.github.io/styleguide/go/decisions.html

