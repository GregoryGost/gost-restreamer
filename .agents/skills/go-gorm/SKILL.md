---
name: go-gorm
description: "Use for Go GORM models, repositories, CRUD, queries, associations, transaction ownership, error translation, performance and database tests."
---

# Go GORM

Backend-independent GORM expertise. Apply a matching active backend overlay when available for database-specific behavior. Otherwise evaluate driver, migrations, locking and SQL using the actual server version, official documentation and authorized integration tests. Techniques from another database's material may be candidates, provided their target-engine support, semantics and trade-offs are independently verified; the project profile still controls skill activation.

## Required Dependencies

- `go-core`

## Discovery

Read `CODEX_PROJECT.md`, `go.mod`, existing GORM models and methods, DB backend/driver, migrations, service contracts, tests, logging/privacy conventions and project test commands.

GORM Traditional API and Generics API differ: `gorm.G[T]` is available in GORM 1.30.0+; do not mechanically refactor API style without a project decision. Read the exact installed GORM API documentation before new queries.

## Data access boundaries

- Keep database schema, SQL, GORM clauses, joins, preloads, order clauses and persistence exceptions inside repositories/data-access code.
- Keep services responsible for business validation, authorization policy, composition and multi-repository transaction decisions.
- Expose stable domain/DTO values or fully materialized read models across boundaries; avoid live lazy-loading ORM objects in HTTP responses.
- Use unique and foreign key/check constraints to enforce integrity independently of pre-checks. A read-before-write check cannot replace a constraint under concurrency.
- Make timestamps, nullability, soft-delete semantics, zero-values and serialization explicit. GORM defaults and model tags are not universal business rules.

## Query and transaction safety

- Parameterize values with placeholders/expressions; validate/whitelist dynamic identifiers. Do not concatenate request-supplied SQL fragments.
- Check errors from every finishing method. Handle not-found deliberately (e.g. `errors.Is(err, gorm.ErrRecordNotFound)`).
- Translate known persistence errors into stable repository/domain errors; GORM `TranslateError` is optional and dialect/version dependent.
- For multiple writes use `db.Transaction(func(tx *gorm.DB) error {...})` and exclusively use `tx` inside its callback. Return error for rollback and nil for commit.
- Keep transactions short, never perform HTTP requests or long computations inside them, and avoid surprising commits in individual repository methods.
- Treat GORM transactions as local to the actual database transaction. For cross-service consistency, escalate verified workflow/recovery needs to the architecture decision. Compare feasible local transactions, existing coordination, Saga, TCC, supported distributed commit, stored routines and message-reliability mechanisms when the verified requirements make them relevant; multiple tables, messages or ORM limits alone are insufficient evidence for a particular choice. For approved database routines, keep execution in data-access code and verify actual driver/engine invocation, transaction ownership, errors/results, privileges and versioned migrations through the matching active overlay or version-matched official sources when no overlay exists.
- Reusing unscoped query state can leak conditions; use fresh sessions for distinct operations and do not assume chained `*gorm.DB` is an immutable singleton.
- Avoid N+1 and accidental eager loading. Use `Preload` / `Joins` intentionally and confirm SQL/latency with actual query evidence.
- Require deliberate filters/constraints for `Update` / `Delete`; verify row counts and optimistic concurrency where contract demands it.
- Use pagination with stable ordering; do not run unrestricted list queries where a bounded response is required.

## Migration and test coordination

- Follow the active migration owner. In a Goose-managed project, do not run GORM `AutoMigrate`/`Migrator` as a competing schema authority in runtime or production startup.
- Use real database integration tests with the correct backend to cover constraints, rolled-back transactions, query correctness and error mapping.
- Test schema alignment with migrations rather than accepting a model that only happens to work after `AutoMigrate`.
- Use `go-testing` and the backend-specific skill only when independently activated.

Load `references/patterns-and-review.md` for anti-patterns, `references/official-sources.md` for official GORM API.

