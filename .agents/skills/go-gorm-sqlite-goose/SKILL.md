---
name: go-gorm-sqlite-goose
description: "Use when Go persistence is explicitly GORM + SQLite with Goose v3 SQL/Go migrations; cover drivers, CGO, PRAGMA, WAL, schema evolution and DB tests."
---

# Go GORM SQLite + Goose

Specialized overlay for the exact stack `GORM + SQLite + pressly/goose/v3`. It neither chooses SQLite on behalf of a project nor forces use of ORM where raw SQL or another toolkit already fits.

## Required Dependencies

- `go-gorm`

## Profile and version verification

Before modifying data access or migrations, verify:

1. `CODEX_PROJECT.md` explicitly enables SQLite + GORM + Goose with the matching rule/skill pair, not just an isolated dependency.
2. `go.mod` pins compatible Go, GORM, SQLite dialect/driver and Goose v3 versions; inspect selected drivers, build tags and CGO deployment support.
3. Actual SQLite engine versions and options for app/runtime and migration tool. GORM's common `gorm.io/driver/sqlite` uses a CGO driver, while Goose CLI's documented `sqlite3` driver uses `modernc.org/sqlite`; do not infer identical engines, compile flags or feature availability.
4. How Goose runs: approved CLI/deployment job versus in-process library, migration directory, database path/DSN, ordering/version convention, owner and rollback policy.
5. Which connection sets `PRAGMA foreign_keys`, `busy_timeout`, optional WAL and other settings, and how these settings apply to pooled connections.
6. SQLite file/backup policy, concurrent processes/workers, write load and the real transaction owner.

## Schema ownership: Goose only

- Migrations in versioned Goose SQL files are authoritative for production schema. GORM models are runtime mappings; they do not replace reviewed schema changes.
- No GORM `AutoMigrate` in production startup, request path or background worker when Goose owns the database. Do not let test setup hide missing migrations by using `AutoMigrate`.
- Prefer SQL migrations for transparent, reviewable schema/history. Use Go migrations only for justified complex transforms and explicit registration in the selected Goose execution model.
- By default Goose SQL migration files execute within a transaction. Use `-- +goose NO TRANSACTION` only after verifying a genuine requirement, failure recovery and data backup. An optional `Down` must be safe and non-destructive or deliberately omitted/documented.
- Never modify previously applied migration content without an explicit compatibility/history plan; failed upgrades require a checked repair plan, not blanket `reset` or manual modification of the version table.
- Coordinate migrations as a single-owner deployment step before service readiness. In-process startup migrations are allowed only under a declared single-owner/locking policy; never assume a multi-replica service can all migrate concurrently.
- Validate against both clean and realistically populated pre-migration database fixtures; test Up, schema/data preservation and Down only where supported and safe.

## SQLite connection semantics

- Default GORM SQLite driver uses CGO; assess Windows/Linux/ARM and cross-compilation, native library/build tags, reproducibility and whether a verified pure-Go alternative is needed. Don't introduce another driver without approval.
- Don't assume Goose CLI and runtime share driver or SQLite version. If a library mode is chosen, verify how the `*sql.DB`, dialect and migrations are wired; never assume a `*gorm.DB` is itself a Goose database handle.
- `PRAGMA foreign_keys=ON` must be verified per connection; setting it once on one pooled connection is insufficient. It cannot be toggled effectively within an active transaction.
- Choose and document `busy_timeout` per connection and a bounded retry policy; timeouts do not provide serializable application invariants.
- WAL is optional and supports concurrent reads with writes but still only one writer. Checkpointing, `-wal`/`-shm` file lifecycle, backup and storage restrictions require an explicit operational policy.
- Keep transactions short and use database constraints to guard concurrent correctness. A per-process writer queue is optional, not universally required; if selected, document multi-process consequences and durability.
- Never use an in-memory SQLite database to prove file locking, WAL, multi-connection behavior or migration safety.

## Connection/resource lifetime and native memory

- The composition root owns the long-lived GORM connection pool (accessed via the underlying `*sql.DB`); do not close that pool per request, and do not create a new pool per query without an explicit policy.
- For GORM `Rows()`/raw `database/sql` iteration, ensure `*sql.Rows.Close()` on every early return and check `Rows.Err()`. Close explicitly acquired `*sql.Conn` and statements; close/rollback transactions according to the owner contract. Treat unexpected persistent `db.Stats().InUse` growth as a symptom to investigate, not proof without workload context; healthy pools may retain idle connections.
- Monitor Go heap/`runtime/metrics` separately from OS RSS and file descriptors. The usual CGO SQLite driver may allocate native memory not represented in Go heap profiles. Diagnose engine/native allocation and OS memory before claiming a Go heap leak. A SQLite `-wal` sidecar file is not itself a heap leak.
- Profile, sample or change memory/connection settings only with project authorization; use `go-core/references/memory-and-resource-leaks.md` when independently active for the general evidence workflow.

## Queries, data and failure semantics

- Parameterize SQL through GORM and use explicit predicate scope, ordering, pagination and indexes.
- Map expected not-found/constraint/lock outcomes to stable repository errors; SQLite driver errors and optional GORM `TranslateError` mappings are version-dependent.
- Model money, booleans, timestamps, nullable fields and JSON with deliberate SQLite representation; avoid relying on implicit float precision or generic type affinity for business invariants.
- For complicated schema changes, apply SQLite's documented table-rebuild procedure. Review tables, indexes, triggers, views, FK references, data backfill and `foreign_key_check` with representative data.
- Use `EXPLAIN QUERY PLAN` and actual workload evidence to investigate query/index choices; do not invent performance guarantees.

## Integration tests and acceptance

- Create a private file-backed temporary DB. Run the **actual Goose migration runner** and assert version history and schema before creating GORM models/queries.
- Check actual `PRAGMA foreign_keys`, `busy_timeout` and journal mode through multiple new connections under the configured pool.
- Verify FKs, unique/check constraints, meaningful rollback, concurrent writes/lock failure policy, data-preserving schema changes and query behavior.
- Where affected, exercise repeated GORM queries with disposable SQLite DBs and verify `Rows`/transaction closure, expected pool `InUse` behavior after completion, no unbounded goroutines and explicit owner shutdown. Do not assert zero idle connections or exact native RSS values.
- For CGO vs pure-Go dual drivers, exercise both migration and runtime connections and verify compatible SQLite features/versions.
- Run the project-declared test commands and report unexecuted physical/production migration checks separately. No actual production mutation is authorized by this skill.

Use `references/migrations-and-connections.md` for specific pitfalls, `references/review-checklist.md` for acceptance and `references/official-sources.md` for authoritative sources.

