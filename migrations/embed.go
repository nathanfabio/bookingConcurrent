// Package migrations embeds the SQL migration files so goose can run them
// from the binary itself — no need to ship the migrations/ directory next
// to a deployed executable.
package migrations

import "embed"

// FS contains every *.sql migration file in this directory. goose's
// provider walks it; sqlc reads the same files from disk for schema
// inference, so the embedded copy and the generator's input can never
// drift apart.
//
//go:embed *.sql
var FS embed.FS
