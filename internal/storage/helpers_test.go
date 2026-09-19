package storage_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
	"gorm.io/gorm"

	"github.com/JetManiack/mcp-sandbox/internal/storage"
)

// testPostgresDSNEnv names the environment variable that points the suite at a
// live Postgres. Unset, every test runs against SQLite only; set, each test runs
// twice. The Postgres-specific code paths (advisory-lock migration,
// driver-specific SQL, NULL handling in the active-block unique index) are
// otherwise never exercised at all.
const testPostgresDSNEnv = "TEST_POSTGRES_DSN"

// backends returns the databases each test should run against: SQLite always,
// Postgres additionally when TEST_POSTGRES_DSN is set.
func backends(t *testing.T) map[string]func(*testing.T) *gorm.DB {
	t.Helper()
	out := map[string]func(*testing.T) *gorm.DB{"sqlite": openSQLiteTestDB}
	if os.Getenv(testPostgresDSNEnv) != "" {
		out["postgres"] = openPostgresTestDB
	}
	return out
}

// runOnEachBackend runs fn against every available backend as a subtest.
func runOnEachBackend(t *testing.T, fn func(*testing.T, *gorm.DB)) {
	t.Helper()
	for name, open := range backends(t) {
		t.Run(name, func(t *testing.T) {
			fn(t, open(t))
		})
	}
}

func openSQLiteTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("storage.Open (sqlite): %v", err)
	}
	return db
}

// openPostgresTestDB gives each test its own schema on the shared Postgres
// instance, so tests never see each other's rows and don't have to clean up
// (several of them register agents under the same display name, which shares a
// unique index).
//
// The schema DDL goes through database/sql rather than storage.Open, because
// storage.Open migrates: using it for the admin connection would create this
// app's whole schema in `public` as a side effect of setting up an isolated
// schema.
func openPostgresTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv(testPostgresDSNEnv)
	schema := postgresSchemaName(t.Name())

	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("sql.Open (postgres admin): %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	// Identifiers can't be parameterized, hence the formatting — schema is
	// derived from the test name through postgresSchemaName, which admits only
	// [a-z0-9_].
	if _, err := admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema)); err != nil {
		t.Fatalf("drop schema %s: %v", schema, err)
	}
	if _, err := admin.Exec(fmt.Sprintf("CREATE SCHEMA %s", schema)); err != nil {
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(fmt.Sprintf("DROP SCHEMA IF EXISTS %s CASCADE", schema))
	})

	separator := "?"
	if strings.Contains(dsn, "?") {
		separator = "&"
	}
	db, err := storage.Open(dsn + separator + "search_path=" + schema)
	if err != nil {
		t.Fatalf("storage.Open (postgres schema %s): %v", schema, err)
	}
	return db
}

// postgresSchemaName turns a test name into a safe, bounded schema identifier.
func postgresSchemaName(testName string) string {
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '_':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '_'
		}
	}, testName)

	name := "test_" + safe
	// Postgres truncates identifiers at 63 bytes; truncating here keeps the
	// CREATE and the DROP referring to the same schema.
	if len(name) > 60 {
		name = name[:60]
	}
	return name
}

func mustAgent(t *testing.T, db *gorm.DB, name string) *storage.Actor {
	t.Helper()
	agent, err := storage.CreateAgent(db, name)
	if err != nil {
		t.Fatalf("CreateAgent(%q): %v", name, err)
	}
	return agent
}

// mustHuman creates a human Actor to stand in for the administrator pressing
// the block button.
func mustHuman(t *testing.T, db *gorm.DB, subject, name string) *storage.Actor {
	t.Helper()
	actor, err := storage.GetOrCreateHumanActor(db, subject, name, "admin")
	if err != nil {
		t.Fatalf("GetOrCreateHumanActor(%q): %v", subject, err)
	}
	return actor
}
