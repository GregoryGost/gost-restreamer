# GORM patterns and review

## Correct transaction ownership

Good: the service invokes `db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { ... })` and passes that `tx` to all repositories in the unit of work.

Bad: one repository silently commits while another writes outside the transaction, or a callback uses the global `db` instead of `tx`.

## Query construction

Good: `Where("email = ?", email)` with bound user input and verified unique indexes.

Bad: `Where("email = '" + email + "'")`; unverified `Order(userSupplied)`; unfiltered `Delete`.

## Results and errors

Good: check Traditional `.Error` or Generics returned error; map `ErrRecordNotFound` to stable domain not-found.

Bad: treat any query error as not-found; leak raw SQLite error strings to HTTP callers.

## DTOs, queries and migrations

Good: map data models to response DTOs explicitly and migrate schema through the selected migration owner.

Bad: serialize GORM model with sensitive/internal fields as the public response, run `AutoMigrate` in every service startup while Goose also applies SQL scripts, or assume associations preload automatically.

## Review checklist

- GORM version and API generation verified.
- Correct query parameters, scoping and stable ordering.
- No accidental global update/delete, N+1 or implicit soft-delete behavior.
- Transaction owner and context propagation explicit.
- Typed errors and constraints tested against real DB.
- Model/schema alignment with the declared migration owner validated.

