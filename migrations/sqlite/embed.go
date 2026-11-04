// Package sqlite содержит встроенную историю схемы SQLite.
package sqlite

import "embed"

// Files содержит неизменяемые последовательные SQL-миграции.
//
//go:embed *.sql
var Files embed.FS
