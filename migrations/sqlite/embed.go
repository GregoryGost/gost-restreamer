// Package sqlite содержит единственную начальную схему SQLite.
package sqlite

import "embed"

// Files предъявляет Goose единственный начальный SQL-скрипт в корне файловой системы.
//
//go:embed 00001_initial.sql
var Files embed.FS
