// Command migrate applies schema migrations explicitly (CLAUDE.md §2):
// CI and production run this as a deliberate deploy step, while dev
// auto-migrates at boot via cmd/api.
//
//	go run ./cmd/migrate up       # apply pending migrations
//	go run ./cmd/migrate status   # show applied/pending versions
package main

import (
	"context"
	"fmt"
	"os"

	postgresadapter "github.com/nathanfabio/bookingConcurrent/internal/adapters/postgres"
	"github.com/nathanfabio/bookingConcurrent/internal/platform/config"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: migrate <up|status>")
		os.Exit(2)
	}

	cfg, err := config.Load()
	if err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}

	ctx := context.Background()
	switch os.Args[1] {
	case "up":
		applied, err := postgresadapter.MigrateUp(ctx, cfg.Postgres)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("migrations applied: %d (database now up to date)\n", applied)
	case "status":
		out, err := postgresadapter.MigrateStatus(ctx, cfg.Postgres)
		if err != nil {
			fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
			os.Exit(1)
		}
		fmt.Print(out)
	default:
		fmt.Fprintln(os.Stderr, "usage: migrate <up|status>")
		os.Exit(2)
	}
}
