# GORM + SQLite + Goose review checklist

## Activation and architecture

- [ ] Target profile explicitly selects Go, GORM, SQLite and Goose v3.
- [ ] Exact rule/skill pair and required go-core/go-gorm are active.
- [ ] Go toolchain, GORM version, SQLite driver and Goose version verified.
- [ ] CGO/cross-build constraints and driver divergence reviewed.
- [ ] Application and migration ownership, backup/restore scope and deployment topology clear.

## Migrations

- [ ] Goose is authoritative for runtime schema and history; no competing AutoMigrate.
- [ ] Ordered Up scripts and optional safe Down reviewed.
- [ ] Already-applied migrations remain immutable absent approved history plan.
- [ ] Single migration owner, failure recovery, before/after version state defined.
- [ ] Real data preservation, constraints, indexes, FK references, triggers and views tested.

## SQLite runtime

- [ ] `foreign_keys` and `busy_timeout` configured and verified per connection.
- [ ] WAL policy and checkpoint/backup/sidecar behavior understood if enabled.
- [ ] Single-writer contention assumptions, pool and retries bounded.
- [ ] GORM/`database/sql` pool owner identified; returned `*sql.Rows`/`*sql.Conn`, statements and transactions released on all exit paths.
- [ ] Diagnostic distinction between `db.Stats().InUse`, idle pool connections, Go heap, native CGO SQLite memory and process RSS preserved.
- [ ] No unbounded or external I/O inside write transactions.
- [ ] Data representation, constraints, indexes and ordering explicit.

## Tests and safety

- [ ] File-based temporary DB with actual Goose migration execution.
- [ ] Real GORM integration tests for queries, rollback, FK and unique constraints.
- [ ] Multiple-connection and concurrent-write cases covered where relevant.
- [ ] Migrator/runtime engines checked for compatibility.
- [ ] Repeated-query lifecycle and cleanup regression verified when connection ownership changes; no speculative absolute memory threshold.
- [ ] No production `goose up/down/reset/redo` or irreversible DB operation performed without separate authorization.

