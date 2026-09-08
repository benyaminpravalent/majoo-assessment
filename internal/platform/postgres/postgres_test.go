package postgres

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/bpsiregar/majoo-assessment/internal/platform/apierr"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestErrorClassifiers(t *testing.T) {
	t.Parallel()

	unique := &pgconn.PgError{Code: "23505", ConstraintName: "users_email_unique"}
	fk := &pgconn.PgError{Code: "23503", ConstraintName: "comments_parent_same_post"}
	check := &pgconn.PgError{Code: "23514", ConstraintName: "posts_comment_count_non_negative"}
	serialization := &pgconn.PgError{Code: "40001"}
	deadlock := &pgconn.PgError{Code: "40P01"}

	// Each classifier must match its own code and reject the others, or a
	// duplicate email could be reported as a foreign-key problem.
	assert.True(t, IsUniqueViolation(unique, "users_email_unique"))
	assert.True(t, IsUniqueViolation(unique, ""), "an empty constraint matches any")
	assert.False(t, IsUniqueViolation(unique, "users_username_unique"))
	assert.False(t, IsUniqueViolation(fk, ""))

	assert.True(t, IsForeignKeyViolation(fk, "comments_parent_same_post"))
	assert.False(t, IsForeignKeyViolation(unique, ""))

	assert.True(t, IsCheckViolation(check, "posts_comment_count_non_negative"))
	assert.False(t, IsCheckViolation(unique, ""))

	assert.True(t, IsRetryable(serialization))
	assert.True(t, IsRetryable(deadlock))
	assert.False(t, IsRetryable(unique))

	// Classifiers must see through wrapping, because repositories add context.
	assert.True(t, IsUniqueViolation(fmt.Errorf("insert user: %w", unique), "users_email_unique"))

	// And must not misclassify a plain error.
	plain := errors.New("connection reset")
	assert.False(t, IsUniqueViolation(plain, ""))
	assert.False(t, IsRetryable(plain))
	assert.False(t, IsUniqueViolation(nil, ""))
}

func TestIsNoRowsAndNotFoundOr(t *testing.T) {
	t.Parallel()

	assert.True(t, IsNoRows(fmt.Errorf("select post: %w", pgx.ErrNoRows)))
	assert.False(t, IsNoRows(errors.New("boom")))

	notFound := apierr.From(NotFoundOr(fmt.Errorf("select post: %w", pgx.ErrNoRows), "post"))
	assert.Equal(t, apierr.CodeNotFound, notFound.Code)
	assert.Equal(t, "post not found", notFound.Message)

	// Anything else is a server fault, and the driver text must not leak.
	internal := apierr.From(NotFoundOr(errors.New("password authentication failed"), "post"))
	assert.Equal(t, apierr.CodeInternal, internal.Code)
	assert.NotContains(t, internal.Message, "password")
}

// TestMigratorLoadPairsUpAndDownFiles uses an in-memory filesystem, so the
// parsing rules can be tested without a database.
func TestMigratorLoadPairsUpAndDownFiles(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{
		"0002_add_index.up.sql":   {Data: []byte("CREATE INDEX x;")},
		"0002_add_index.down.sql": {Data: []byte("DROP INDEX x;")},
		"0001_init.up.sql":        {Data: []byte("CREATE TABLE t;")},
		"0001_init.down.sql":      {Data: []byte("DROP TABLE t;")},
		"README.md":               {Data: []byte("not a migration")},
	}

	got, err := (&Migrator{files: files}).load()

	require.NoError(t, err)
	require.Len(t, got, 2)
	// Version order, not directory order.
	assert.Equal(t, int64(1), got[0].Version)
	assert.Equal(t, "init", got[0].Name)
	assert.Equal(t, "CREATE TABLE t;", got[0].Up)
	assert.Equal(t, "DROP TABLE t;", got[0].Down)
	assert.Equal(t, int64(2), got[1].Version)
}

func TestMigratorLoadAllowsAMissingDownFile(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{"0001_init.up.sql": {Data: []byte("CREATE TABLE t;")}}

	got, err := (&Migrator{files: files}).load()

	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Down, "an irreversible migration loads, and Down refuses it later")
}

func TestMigratorLoadRejectsMisnamedSQLFiles(t *testing.T) {
	t.Parallel()

	for name, files := range map[string]fstest.MapFS{
		"no version":     {"init.up.sql": {Data: []byte("x")}},
		"no direction":   {"0001_init.sql": {Data: []byte("x")}},
		"uppercase name": {"0001_INIT.up.sql": {Data: []byte("x")}},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := (&Migrator{files: files}).load()

			// Failing loudly matters: a silently ignored migration file is a
			// production schema that does not match the code.
			require.Error(t, err)
			assert.Contains(t, err.Error(), "does not match")
		})
	}
}

func TestMigratorLoadRejectsADownFileWithoutAnUp(t *testing.T) {
	t.Parallel()

	files := fstest.MapFS{"0001_init.down.sql": {Data: []byte("DROP TABLE t;")}}

	_, err := (&Migrator{files: files}).load()

	require.Error(t, err)
	assert.Contains(t, err.Error(), "no .up.sql")
}

// TestChecksumIgnoresLineEndings: the repository is authored on Windows and
// applied on Linux, so a CRLF checkout must not look like an edited migration.
func TestChecksumIgnoresLineEndings(t *testing.T) {
	t.Parallel()

	assert.Equal(t,
		checksum("CREATE TABLE t;\nCREATE INDEX i;\n"),
		checksum("CREATE TABLE t;\r\nCREATE INDEX i;\r\n"))

	assert.NotEqual(t,
		checksum("CREATE TABLE t;"),
		checksum("CREATE TABLE u;"))
}

func TestMigratorDownRejectsNonPositiveSteps(t *testing.T) {
	t.Parallel()

	m := &Migrator{files: fstest.MapFS{}}

	_, err := m.Down(context.Background(), 0)

	require.Error(t, err)
	assert.Contains(t, err.Error(), "steps")
}
