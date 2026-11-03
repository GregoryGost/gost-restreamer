# Go Huma v2 rules

Activate only via exact `go_huma_v2.md` Active Rules, `go-huma-v2` Active Stack Profiles, or an enabled Go Huma v2 profile naming `go_huma_v2.md + go-huma-v2`. Huma v1 and a mere task mention are insufficient.

## Required skills

- `go-core`
- `go-huma-v2`

## Version and framework boundary

Read `CODEX_PROJECT.md`, `go.mod`, effective Go toolchain, selected `github.com/danielgtaylor/huma/v2` version, router/adapter, actual operation registration and tests. Current upstream Huma docs require Go 1.26+; validate the pinned module's actual minimum instead of imposing an unverified value on another revision.

## HTTP contract constraints

- Define stable, typed request and response models and operation metadata. Registration must reflect actual method, path, status, content type and error conditions.
- Keep operation handlers thin: input validation and dependency resolution at HTTP boundary, domain logic in services, persistence in repositories.
- Use the project's actual Huma adapter (e.g. `humago` or `humachi`); do not introduce/replace a router without approval.
- Distinguish missing/nullable fields and Huma's validation behavior; inspect config before assuming strictness, unknown-query behavior or exact 400/422 mappings.
- Keep documented authentication schemes and runtime authorization enforcement aligned. OpenAPI security metadata alone does not enforce access control.
- Map domain failures to intentional HTTP status and RFC 9457-style error responses where configured; never leak internal data.
- Verify generated OpenAPI and real HTTP responses, including docs/schema endpoint exposure policies. Huma generates schemas, but semantic correctness is still application-owned.
- Use `go-testing` only when independently active and testing is in scope.

