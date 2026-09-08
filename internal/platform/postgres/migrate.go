package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisoryLockID is an arbitrary but fixed key. Every migrator process takes
// this session-level lock before touching schema_migrations, so two API
// replicas starting at the same moment cannot apply the same migration twice.
const advisoryLockID int64 = 8_724_193_552_001

// migrationFilePattern matches "0001_init.up.sql" and "0001_init.down.sql".
var migrationFilePattern = regexp.MustCompile(`^(\d+)_([a-z0-9_-]+)\.(up|down)\.sql$`)

// Migration is one versioned change, with both directions where a down file
// exists.
type Migration struct {
	Version int64
	Name    string
	Up      string
	Down    string
}

// AppliedMigration is a row of schema_migrations.
type AppliedMigration struct {
	Version   int64
	Name      string
	Checksum  string
	AppliedAt time.Time
}

// Migrator applies versioned SQL migrations.
//
// This is a deliberately small, dependency-free runner rather than
// golang-migrate. It is ~200 lines, it does exactly what this project needs
// (ordered up/down, checksums, an advisory lock, one transaction per
// migration), and the migration files stay plain SQL that psql or any other
// tool can also apply. The trade-off — no support for the wider ecosystem of
// drivers and sources — is irrelevant to a single-database service. See
// docs/architecture-decisions.md, ADR-005.
type Migrator struct {
	pool   *pgxpool.Pool
	files  fs.FS
	logger *slog.Logger
}

// NewMigrator returns a Migrator reading .sql files from files.
//
// It takes a concrete *pgxpool.Pool rather than the Store interface because the
// advisory lock below is session-scoped and therefore needs one specific pinned
// connection, which only the pool can hand out.
func NewMigrator(pool *pgxpool.Pool, files fs.FS, logger *slog.Logger) *Migrator {
	if logger == nil {
		logger = slog.Default()
	}
	return &Migrator{pool: pool, files: files, logger: logger}
}

// Up applies every migration that has not been applied yet, in version order.
// It returns the versions it applied.
func (m *Migrator) Up(ctx context.Context) ([]int64, error) {
	migrations, err := m.load()
	if err != nil {
		return nil, err
	}

	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := m.ensureVersionTable(ctx); err != nil {
		return nil, err
	}

	applied, err := m.appliedByVersion(ctx)
	if err != nil {
		return nil, err
	}

	var run []int64
	for _, mig := range migrations {
		if prev, ok := applied[mig.Version]; ok {
			// A changed checksum means someone edited a migration that is already
			// in production. Refuse rather than silently diverge.
			if sum := checksum(mig.Up); sum != prev.Checksum {
				return run, fmt.Errorf(
					"migration %04d_%s was already applied with checksum %s but the file now hashes to %s: "+
						"edit history is not allowed, add a new migration instead",
					mig.Version, mig.Name, prev.Checksum, sum)
			}
			continue
		}

		if err := m.applyUp(ctx, mig); err != nil {
			return run, err
		}
		run = append(run, mig.Version)
		m.logger.Info("applied migration",
			slog.Int64("version", mig.Version), slog.String("name", mig.Name))
	}

	return run, nil
}

// Down rolls back the newest applied migrations, at most steps of them.
func (m *Migrator) Down(ctx context.Context, steps int) ([]int64, error) {
	if steps <= 0 {
		return nil, errors.New("steps must be greater than zero")
	}

	migrations, err := m.load()
	if err != nil {
		return nil, err
	}
	byVersion := make(map[int64]Migration, len(migrations))
	for _, mig := range migrations {
		byVersion[mig.Version] = mig
	}

	unlock, err := m.lock(ctx)
	if err != nil {
		return nil, err
	}
	defer unlock()

	if err := m.ensureVersionTable(ctx); err != nil {
		return nil, err
	}

	applied, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	// Applied returns ascending order; roll back from the newest.
	sort.Slice(applied, func(i, j int) bool { return applied[i].Version > applied[j].Version })

	var reverted []int64
	for i, row := range applied {
		if i >= steps {
			break
		}
		mig, ok := byVersion[row.Version]
		if !ok {
			return reverted, fmt.Errorf("migration %04d is recorded as applied but its file is missing", row.Version)
		}
		if strings.TrimSpace(mig.Down) == "" {
			return reverted, fmt.Errorf("migration %04d_%s has no .down.sql and cannot be rolled back", mig.Version, mig.Name)
		}
		if err := m.applyDown(ctx, mig); err != nil {
			return reverted, err
		}
		reverted = append(reverted, mig.Version)
		m.logger.Info("reverted migration",
			slog.Int64("version", mig.Version), slog.String("name", mig.Name))
	}

	return reverted, nil
}

// Applied returns the migrations recorded in schema_migrations, oldest first.
func (m *Migrator) Applied(ctx context.Context) ([]AppliedMigration, error) {
	rows, err := m.pool.Query(ctx,
		`SELECT version, name, checksum, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	var out []AppliedMigration
	for rows.Next() {
		var a AppliedMigration
		if err := rows.Scan(&a.Version, &a.Name, &a.Checksum, &a.AppliedAt); err != nil {
			return nil, fmt.Errorf("scan schema_migrations row: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// Pending returns the migrations on disk that have not been applied yet.
func (m *Migrator) Pending(ctx context.Context) ([]Migration, error) {
	migrations, err := m.load()
	if err != nil {
		return nil, err
	}
	if err := m.ensureVersionTable(ctx); err != nil {
		return nil, err
	}
	applied, err := m.appliedByVersion(ctx)
	if err != nil {
		return nil, err
	}

	var out []Migration
	for _, mig := range migrations {
		if _, ok := applied[mig.Version]; !ok {
			out = append(out, mig)
		}
	}
	return out, nil
}

func (m *Migrator) applyUp(ctx context.Context, mig Migration) error {
	// One transaction per migration: a failing statement leaves the database on
	// the previous version rather than half-migrated. PostgreSQL supports
	// transactional DDL, which is what makes this possible.
	return InTx(ctx, m.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, mig.Up); err != nil {
			return fmt.Errorf("apply migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		_, err := tx.Exec(ctx,
			`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)`,
			mig.Version, mig.Name, checksum(mig.Up))
		if err != nil {
			return fmt.Errorf("record migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		return nil
	})
}

func (m *Migrator) applyDown(ctx context.Context, mig Migration) error {
	return InTx(ctx, m.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, mig.Down); err != nil {
			return fmt.Errorf("revert migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		_, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, mig.Version)
		if err != nil {
			return fmt.Errorf("un-record migration %04d_%s: %w", mig.Version, mig.Name, err)
		}
		return nil
	})
}

func (m *Migrator) ensureVersionTable(ctx context.Context) error {
	const stmt = `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    BIGINT      PRIMARY KEY,
			name       TEXT        NOT NULL,
			checksum   TEXT        NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`
	if _, err := m.pool.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	return nil
}

func (m *Migrator) appliedByVersion(ctx context.Context) (map[int64]AppliedMigration, error) {
	rows, err := m.Applied(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]AppliedMigration, len(rows))
	for _, r := range rows {
		out[r.Version] = r
	}
	return out, nil
}

// lock takes the session-level advisory lock and returns the release function.
//
// The lock is taken on a dedicated connection acquired for the whole migration
// run; releasing it also returns that connection to the pool.
func (m *Migrator) lock(ctx context.Context) (func(), error) {
	// pg_advisory_lock is session-scoped, so it must be taken and released on the
	// same connection — hence the explicit Acquire. The transaction-scoped
	// variant would not work here: each migration commits separately, so the lock
	// would be released after the first one.
	conn, err := m.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection for migration lock: %w", err)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, advisoryLockID); err != nil {
		conn.Release()
		return nil, fmt.Errorf("take migration advisory lock: %w", err)
	}
	return func() {
		unlockCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if _, err := conn.Exec(unlockCtx, `SELECT pg_advisory_unlock($1)`, advisoryLockID); err != nil {
			m.logger.Warn("failed to release migration advisory lock", slog.Any("error", err))
		}
		conn.Release()
	}, nil
}

// load reads and pairs the .up.sql / .down.sql files, sorted by version.
func (m *Migrator) load() ([]Migration, error) {
	entries, err := fs.ReadDir(m.files, ".")
	if err != nil {
		return nil, fmt.Errorf("read migrations directory: %w", err)
	}

	byVersion := map[int64]*Migration{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationFilePattern.FindStringSubmatch(path.Base(e.Name()))
		if match == nil {
			if strings.HasSuffix(e.Name(), ".sql") {
				return nil, fmt.Errorf("migration file %q does not match NNNN_name.(up|down).sql", e.Name())
			}
			continue
		}

		version, err := strconv.ParseInt(match[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("migration file %q has an unparsable version: %w", e.Name(), err)
		}

		body, err := fs.ReadFile(m.files, e.Name())
		if err != nil {
			return nil, fmt.Errorf("read migration %q: %w", e.Name(), err)
		}

		mig, ok := byVersion[version]
		if !ok {
			mig = &Migration{Version: version, Name: match[2]}
			byVersion[version] = mig
		}
		if match[3] == "up" {
			mig.Up = string(body)
		} else {
			mig.Down = string(body)
		}
	}

	out := make([]Migration, 0, len(byVersion))
	for _, mig := range byVersion {
		if strings.TrimSpace(mig.Up) == "" {
			return nil, fmt.Errorf("migration %04d_%s has no .up.sql file", mig.Version, mig.Name)
		}
		out = append(out, *mig)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func checksum(s string) string {
	// Normalise line endings so a checkout with CRLF does not appear to be a
	// different migration from the same file checked out with LF.
	normalised := strings.ReplaceAll(s, "\r\n", "\n")
	sum := sha256.Sum256([]byte(normalised))
	return hex.EncodeToString(sum[:])
}
