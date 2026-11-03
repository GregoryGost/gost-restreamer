---
name: go-huma-v2
description: "Use for Go Huma v2 typed HTTP operations, router adapters, OpenAPI/JSON Schema generation, validation, middleware, errors and API tests."
---

# Go Huma v2

Own the Huma v2 HTTP/API layer. Do not replace backend logic or data models with transport structs.

## Required Dependencies

- `go-core`

Use `.codex/rules/go_huma_v2.md` only when independently active; its required skills are validated under `material_dependencies.md`.

## Verify before implementation

- Inspect project profile, `go.mod` (Huma v2 module + versions), effective Go toolchain, router/adapter, middleware order, Huma config, operation registrations, generated API and tests.
- Current public Huma installation docs specify Go 1.26 or newer; verify the exact selected release's compatibility and project toolchain.
- Select the existing router adapter: `humago` for standard `http.ServeMux` when appropriate, `humachi` for chi or another declared supported adapter. Huma is not a mandate to add a separate router.
- Keep OpenAPI 3.1 output and optional 3.0.3 downgrade support version/config aware.

## Typed API boundaries

- Define `huma.Register` operations with deliberate method, path, operation ID, summary, tags, responses and security metadata.
- Model path/query/header parameters and body inputs with documented struct tags; response body and headers must match real bytes/status.
- Use separate public API DTOs and GORM persistence models. Do not expose secrets, private database fields or GORM associations by serializing raw database entities.
- Consider required vs optional/nullable fields, zero values, defaulting, validation tags, field format and numerical precision.
- Huma defaults can reject additional JSON body properties while unknown query fields may be ignored unless configured; verify actual configuration rather than assuming all unknown input is rejected.
- Use built-in validation and documented custom validators/resolvers when applicable; don't duplicate framework validation in every handler.

## Service composition and errors

- Compose router, Huma API, middleware, services, repositories, clients and lifecycle once at the application entrypoint/factory.
- Operation handlers do HTTP translation only; services own domain invariants, repositories own GORM/SQL.
- Carry `context.Context` through I/O. Enforce request timeouts, payload limits and cancellation at correct boundaries.
- Convert domain errors to Huma errors with expected status codes; default Huma errors use Problem Details (`application/problem+json`) when unmodified. Avoid transport leakage from repository errors.
- Enforce authorization in middleware/service logic independently of OpenAPI `security` declarations.
- Decide exposure of `/openapi.json`, `/docs`, schemas and middleware explicitly for deployment.

## Testing and review

- Use the real application registration with `humatest` or standard `httptest` as appropriate to the active testing policy; never mock the Huma layer under integration test.
- Test valid and invalid inputs, unknown fields/params per config, status/headers/media types, error schemas, authn/authz, content negotiation and cancellation where meaningful.
- Compare generated spec against intended public contract using structural assertions or approved snapshot/diff tooling; a generated spec is not proof of correctness.
- Include regression tests for server behavior whenever changing JSON names, operation names, statuses or schemas.
- Load `references/openapi-contract-testing.md` for contract details and `references/official-sources.md` for authoritative Huma APIs.

## Optional coordination

- `go-testing` for active tests; `go-gorm` plus `go-gorm-sqlite-goose` for active persistence. Neither is a universal prerequisite for an API without a database.

