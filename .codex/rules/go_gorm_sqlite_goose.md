# Go GORM + SQLite + Goose rules

Activate only via exact `go_gorm_sqlite_goose.md` Active Rules entry, `go-gorm-sqlite-goose` Active Stack Profiles entry, or a complete enabled SQLite/GORM/Goose profile naming `go_gorm_sqlite_goose.md + go-gorm-sqlite-goose`. Requires confirmed SQLite + GORM + Goose selection; do not activate for generic SQL, standalone GORM or another backend.

## Required skills

- `go-core`
- `go-gorm`
- `go-gorm-sqlite-goose`

## Verified stack gate

Read `CODEX_PROJECT.md`, `go.mod`, GORM SQLite dialect/driver, Goose v3 version and operation mode (CLI or library), SQLite runtime version(s), migrations and deployment topology. Confirm CGO policy and driver compatibility: the default `gorm.io/driver/sqlite` uses `mattn/go-sqlite3`/CGO; current Goose CLI documents `modernc.org/sqlite`. These may use different SQLite engines/build options. Check actual configured drivers and features; never assume parity.

## Contract and migration ownership

- Goose is the sole production schema history/execution owner. Do not run GORM `AutoMigrate`/`Migrator` for schema changes at startup or alongside Goose. A test/prototype exception must be explicit and cannot mask migration tests.
- Create ordered, reviewed SQL migrations (or approved Go migrations) with `-- +goose Up` and optional `-- +goose Down`. Prefer ordinary transactions; `NO TRANSACTION` is exceptional, justified and reviewed.
- For any SQLite table rebuild, preserve data, indexes, constraints, triggers, foreign key relationships and migration history. Test old-to-new data, not only an empty schema.
- Never rewrite an already applied migration without approved history/reconciliation strategy; do not apply `down`/`reset`/`redo` to user/production DBs just to validate changes.
- Require explicit target DB, backup/rollback approach and migration approval before actual database mutation. Repository documentation changes do not authorize DB operations.
- Migrate in a controlled single-owner deployment step and verify Goose version state before service readiness. Do not let all service instances race to migrate.

## SQLite correctness

- Treat `foreign_keys` and `busy_timeout` as connection-local. Enable/enforce with the configured driver or connection initializer for every connection, including the migration connection. `PRAGMA foreign_keys` changes inside a transaction do not take effect.
- WAL is an optional declared policy, not a cure for write contention; SQLite permits one active writer. Keep write transactions short; establish lock/retry behavior and worker/process assumptions.
- Do not force `MaxOpenConns(1)` universally; size pools from concurrency semantics and test the chosen settings.
- Ensure GORM `Rows()`/`database/sql` rows, acquired connections and transactions are closed or rolled back at their owned boundary; keep the underlying `*sql.DB` shared for its declared lifespan. Investigate `db.Stats().InUse` and CGO/native memory separately from Go heap; growth alone is not proof of a leak.
- Use file-based temporary databases for connection, WAL, lock, migration and FK tests.
- Test integrity, query/index plans where relevant, rollback and error translation through real GORM + SQLite connections.
- Additional testing specifics and examples belong in the corresponding skill references, not in the rule.

