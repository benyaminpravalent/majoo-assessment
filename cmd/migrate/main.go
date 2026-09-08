// Command migrate applies, reverts and reports database migrations.
//
// It is a separate binary from the API rather than a flag on it, so that a
// deployment can run migrations as an init container or a one-shot job with its
// own database credentials — typically a role with DDL rights that the running
// service does not have.
//
// Usage:
//
//	migrate up              apply all pending migrations
//	migrate down [n]        revert the newest n migrations (default 1)
//	migrate status          show applied and pending migrations
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/bpsiregar/majoo-assessment/internal/config"
	"github.com/bpsiregar/majoo-assessment/internal/platform/logging"
	"github.com/bpsiregar/majoo-assessment/internal/platform/postgres"
	"github.com/bpsiregar/majoo-assessment/migrations"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "fatal: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	flag.Usage = usage
	flag.Parse()

	args := flag.Args()
	if len(args) == 0 {
		usage()
		return errors.New("a command is required")
	}

	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load configuration: %w", err)
	}

	logger := logging.New(os.Stdout, cfg.Log.Level, cfg.Log.Format)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := postgres.Connect(ctx, cfg.DB)
	if err != nil {
		return fmt.Errorf("connect to database: %w", err)
	}
	defer pool.Close()

	migrator := postgres.NewMigrator(pool, migrations.FS, logger)

	switch cmd := args[0]; cmd {
	case "up":
		applied, err := migrator.Up(ctx)
		if err != nil {
			return err
		}
		if len(applied) == 0 {
			fmt.Println("database is already up to date")
			return nil
		}
		fmt.Printf("applied %d migration(s): %v\n", len(applied), applied)
		return nil

	case "down":
		steps := 1
		if len(args) > 1 {
			steps, err = strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("down: %q is not a number of steps", args[1])
			}
		}
		reverted, err := migrator.Down(ctx, steps)
		if err != nil {
			return err
		}
		fmt.Printf("reverted %d migration(s): %v\n", len(reverted), reverted)
		return nil

	case "status":
		return printStatus(ctx, migrator)

	default:
		usage()
		return fmt.Errorf("unknown command %q", cmd)
	}
}

func printStatus(ctx context.Context, m *postgres.Migrator) error {
	applied, err := m.Applied(ctx)
	if err != nil {
		return err
	}
	pending, err := m.Pending(ctx)
	if err != nil {
		return err
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VERSION\tNAME\tSTATE\tAPPLIED AT")
	for _, a := range applied {
		fmt.Fprintf(w, "%04d\t%s\t%s\t%s\n", a.Version, a.Name, "applied", a.AppliedAt.Format(time.RFC3339))
	}
	for _, p := range pending {
		fmt.Fprintf(w, "%04d\t%s\t%s\t%s\n", p.Version, p.Name, "pending", "-")
	}
	if err := w.Flush(); err != nil {
		return fmt.Errorf("write status table: %w", err)
	}

	fmt.Printf("\n%d applied, %d pending\n", len(applied), len(pending))
	return nil
}

func usage() {
	fmt.Fprint(os.Stderr, `migrate — apply database migrations

Usage:
  migrate up              apply all pending migrations
  migrate down [n]        revert the newest n migrations (default 1)
  migrate status          show applied and pending migrations

Configuration is read from the environment; DATABASE_URL and JWT_SECRET are
required (JWT_SECRET only because configuration is validated as a whole).
`)
}
