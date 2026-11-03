# Goose migrations, GORM mapping and SQLite connections

## Separate responsibilities

- GORM: typed runtime mappings, queries and repository transaction wiring.
- Goose: ordered migration scripts, version history, forward/reverse execution.
- SQLite driver: connection-local pragma initialization, locking behavior and engine build features.

**Do not mix competing schema owners.** A GORM struct changing fields does not update a Goose migration. Applying `AutoMigrate` before/after Goose can conceal required migration work and produce schema drift.

## Goose SQL shape

```sql
-- +goose Up
CREATE TABLE accounts (
    id INTEGER PRIMARY KEY,
    external_id TEXT NOT NULL UNIQUE,
    balance_cents INTEGER NOT NULL CHECK (balance_cents >= 0)
);

-- +goose Down
DROP TABLE accounts;
```

This is an illustrative **new-database** migration; a production `Down` that drops populated data would be destructive and should not be run without explicit decision. Omit `Down` if safe reversal is unavailable. Do not interpolate user values into migration DDL.

Use the project's declared migration directory, version naming, runner and tool version. Goose `-- +goose StatementBegin`/`StatementEnd` handles compound statements with internal semicolons where required. `NO TRANSACTION` and environment substitution are opt-in and need independent safety review.

## PRAGMA pitfalls

Bad: run `db.Exec("PRAGMA foreign_keys = ON")` once after creating a GORM `*gorm.DB` and assume every future pooled connection enforces it.

Good: use documented driver DSN options/connection initialization, verify pragma on multiple physical connections, and test rejection of orphan rows. Repeat connection validation for the migration runner.

Remember: `PRAGMA foreign_keys=ON` is a no-op if attempted inside an already opened SQLite transaction; it is not universal proof of FK enforcement unless the returned value and behavior are checked.

`journal_mode=WAL` changes concurrency behavior and file management; `busy_timeout` is per-connection; neither permits multiple simultaneous SQLite writers.

## Schema change procedure

Before altering a populated table, list columns, constraints, indexes, triggers, dependent views and FK references. For unsupported direct alterations, use the documented SQLite rebuild procedure with explicit field mapping and data checks. Preserve data and use `PRAGMA foreign_key_check`, representational tests and planned backup/recovery. Do not assume rename/drop behavior of ancient SQLite versions matches current docs.

## Connection lifetime and CGO memory

- When GORM returns a `*sql.Rows` (for example through `Rows()`), the caller must close it even on early returns; check iteration errors and transaction cleanup. GORM's underlying `*sql.DB` belongs to the composition root, not each repository method.
- Use `db.Stats()` to investigate growing `InUse`/waits under repeated queries at comparable workload; intentionally idle pooled connections are not a leak by themselves. Tests should use a temporary file-backed SQLite database.
- SQLite's default CGO driver can allocate native memory; Go heap metrics and heap profiles do not report all native allocations. Correlate process RSS, file descriptors and runtime metrics with driver-specific evidence before diagnosing a leak or changing pool limits.

## Driver boundary

A default GORM SQLite binary and a Goose CLI binary can embed distinct SQLite versions and extensions. Goose CLI documentation currently lists `sqlite3` driven by `modernc.org/sqlite`; the GORM SQLite default is CGO-based `mattn/go-sqlite3`. Verify actual binaries, runtime `sqlite_version()` and feature-sensitive migrations; avoid asserting universal driver parity.

## Verification matrix

- Fresh DB: Goose Up, schema/version state and expected constraints.
- Existing DB: copy representative old schema/data, Goose Up, preserved data, indexes, FKs and dependent consumers.
- Reversibility: only explicitly supported safe Down paths in disposable fixtures.
- Connection pool: FK enforcement on multiple new connections; busy timeout and WAL behavior where enabled.
- Contention: file-backed DB with concurrent readers/writers; assert selected lock/error policy.
- Application: GORM repository operations against migrated schema, typed errors and cancellation.

