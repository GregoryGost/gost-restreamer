# Huma v2 OpenAPI contract and testing

## Design contract

- Treat operation registration, typed request/response structs, validation tags and Huma configuration as the source of generated OpenAPI/JSON Schema.
- Validate operation ID uniqueness, method/path pairing, required and nullable fields, body vs query/header semantics, declared status responses and auth definitions.
- Confirm schema reflects the publicly supported API, not internal GORM models or driver-specific types.
- If HTTP returns error statuses not documented by operation config, update metadata or implementation intentionally and test both.

## Runtime verification

- Exercise the same router adapter and middleware as production via `humatest` or `httptest`.
- Validate wire-level status codes, content types and RFC 9457 Problem Details error fields when applicable.
- Test field validation and unknown input separately: additional JSON body properties and unknown query parameters have distinct configuration policies.
- Verify authenticated and unauthenticated requests, resource-level authorization and that OpenAPI descriptions match actual enforcement.
- Check the generated `/openapi.json` output or `api.OpenAPI()` with deterministic structural assertions; use snapshots only when project policy approves them.
- Confirm public exposure/absence of docs/spec/schema endpoints follows configuration and security policy.
- Avoid assuming Huma has HTTP/DB transaction management or automatically enforces security merely from OpenAPI metadata.

## Failure-focused review

- An optional zero value accidentally represented as required?
- A database field leaked into response model?
- An error classified as 500 although it is a known 404/409?
- A documented auth scheme not enforced by middleware?
- A spec generated successfully but service behavior differs?

