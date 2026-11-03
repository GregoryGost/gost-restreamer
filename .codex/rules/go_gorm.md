# Go GORM rules

Activate only via the exact `go_gorm.md` entry in Active Rules, `go-gorm` in Active Stack Profiles, or a complete Go GORM profile naming `go_gorm.md + go-gorm`. This is backend-independent GORM behavior. A task mention alone cannot activate it.

## Required skills

- `go-core`
- `go-gorm`

## Source of truth

Read `CODEX_PROJECT.md`, `go.mod`, ORM models, repository code, transaction services, migrations, driver configuration, existing tests and deployment assumptions. Verify GORM version, API style and actual database before importing version-sensitive examples.

## Constraints

- Repository/data-access modules own GORM queries; services own multi-operation transaction boundaries and domain invariants; Huma handlers own HTTP translation.
- Choose GORM Traditional or Generics API based on the pinned version; `gorm.G[T]` requires GORM >= v1.30.0.
- Propagate context through I/O. Use request-scoped sessions/contexts; do not share mutable chained query state across goroutines.
- Handle `result.Error` in Traditional API or returned errors in Generics API; classify not-found and constraint errors explicitly at the repository boundary.
- Prefer bound parameters to query string concatenation; whitelist dynamic column identifiers or order fields.
- Avoid unscoped writes, accidental soft deletes, hidden N+1 queries, unbounded listings and unexpected bulk updates.
- Make transaction ownership explicit and keep write transactions short; do not call external APIs while holding DB locks.
- Do not expose GORM models as HTTP DTOs by default.
- Migration ownership is project-specific. Do not silently call `AutoMigrate` at runtime when Goose owns the schema.
- Apply DB-specific and test overlays only when active and relevant.

