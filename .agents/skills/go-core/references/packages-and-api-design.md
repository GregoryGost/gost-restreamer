# Go packages, modules and API boundaries

Unless noted otherwise, Go code examples require Go 1.0+; directory trees are illustrative project layout, not Go source code.

## Resolve the project boundary first

Inspect `go.mod`, `go.work`, module replacement directives, code generation, tags, exported symbols, imports, tests and CI. The `go` directive determines permitted language semantics; the optional `toolchain` directive is not a universal deployment guarantee. Do not change dependency graph or run `go mod tidy` during documentation/review work.

## 1. Packages should expose coherent capabilities

**BAD — catch-all package obscures ownership** (DESIGN; examples of names rather than runnable code):

```text
internal/utils/database.go
internal/utils/http.go
internal/utils/users.go
internal/helpers/service.go
```

**GOOD — group by meaningful responsibility**, respecting the current repository:

```text
internal/accounts/service.go
internal/accounts/repository.go
internal/httpapi/routes.go
cmd/service/main.go
```

These are *illustrative*, not mandatory directory names. `internal/` is a Go-enforced import boundary, whereas `pkg/` is an optional naming convention. A small service may need fewer packages. Avoid package import cycles and dependency direction that makes domain code import HTTP/ORM implementations.

## 2. Keep the public surface deliberate

**BAD — exported fields can be mutated without invariant checks** (DESIGN):

```go
package users

type User struct {
    DisplayName string
}
```

This is fine for pure transfer records. It is inappropriate if the display name must be non-empty for a domain entity.

**GOOD — use an explicit operation when the invariant matters**:

```go
package users

import "errors"

// User represents an account with an identifier and a required display name.
type User struct {
    // id is the identifier assigned when the user is created.
    id string
    // displayName is the non-empty name shown to other users.
    displayName string
}

// NewUser creates an account; neither identifier nor name may be empty.
// id identifies the new account; displayName supplies the visible name.
// Invalid inputs return an error instead of creating a User.
func NewUser(id, displayName string) (User, error) {
    if id == "" || displayName == "" {
        return User{}, errors.New("id and display name required")
    }
    return User{id: id, displayName: displayName}, nil
}

// ID returns the stored identifier without changing the user.
func (u User) ID() string { return u.id }

// DisplayName returns the user's current visible name.
func (u User) DisplayName() string { return u.displayName }

// Rename changes the visible name to a non-empty value; it leaves the
// previous name unchanged and returns an error if name is empty.
func (u *User) Rename(name string) error {
    if name == "" {
        return errors.New("display name required")
    }
    u.displayName = name
    return nil
}
```

This example protects a simple domain invariant; actual transport validation and authorization remain separate responsibilities.

## 3. Make architecture proportional

- Start with cohesive concrete packages. Introduce service/repository interfaces when a real boundary or independent implementations demand them.
- Dependency injection can be an ordinary constructor; container frameworks are optional third-party choices.
- Separate transport DTOs, domain decisions and persistence representations only where they differ in lifecycle or trust; avoid redundant type copies that add no contract value.
- Avoid a mandatory Java-style layer for every operation; a thin HTTP adapter may call a focused service directly.
- Do not rename or move packages solely to match an idealized directory tree. Preserve package import paths, external API and generated artifacts unless the task authorizes migration.

## 4. Public API compatibility and versioning

- Changes to exported names, method sets, struct fields, JSON tags, error identity and interfaces can break consumers even if the package still compiles locally.
- Adding a method to an exported interface can break downstream implementations; returning concrete types often preserves more evolution space.
- Check `go doc`, call sites, the `go.mod` major-version path (`/v2`, etc.) and the applicable Go module compatibility contract.
- Keep interfaces minimal and avoid changing public signatures for test convenience.
- Consider cross-platform build tags, CGO dependencies and generated-code sources before changing public package contracts.

## Review prompts

- Are packages coherent and dependency direction understood?
- Is `internal` used as an actual import boundary rather than a style ritual?
- Are exported types and method sets intentionally compatible?
- Does every new interface/dependency have a real use?
- Did the change touch code generation or dependency metadata with permission?

Official references: https://go.dev/ref/mod ; https://go.dev/doc/modules/layout ; https://go.dev/doc/modules/version-numbers ; https://go.dev/wiki/CodeReviewComments#package-names

