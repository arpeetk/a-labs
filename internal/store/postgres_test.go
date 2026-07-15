package store

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// TestPostgresConformance runs the same Store conformance suite as memory
// against a real Postgres. It resolves a DSN one of two ways:
//
//   - STORE_TEST_DSN set → use that database directly (e.g. a `docker run`
//     postgres, or CI's service container). Fastest local loop.
//   - otherwise → spin an ephemeral postgres via testcontainers-go (needs
//     Docker). Zero setup, hermetic, self-cleaning.
//
// If neither a DSN nor Docker is available the test SKIPS with a clear message
// — the suite never fails for lack of infrastructure (implementation-plan
// §WS-3).
func TestPostgresConformance(t *testing.T) {
	dsn := resolvePostgresDSN(t)

	testStore(t, func(t *testing.T) Store {
		ctx := context.Background()
		pg, err := NewPostgres(ctx, dsn)
		if err != nil {
			t.Fatalf("NewPostgres: %v", err)
		}
		t.Cleanup(pg.Close)
		// Fresh state per subtest: the conformance suite assumes an empty store.
		if _, err := pg.pool.Exec(ctx, `TRUNCATE projects, runs`); err != nil {
			t.Fatalf("truncate: %v", err)
		}
		return pg
	})
}

// TestPostgresMigrateIdempotent asserts NewPostgres can run twice against the
// same database without re-applying migrations (schema_version gate).
func TestPostgresMigrateIdempotent(t *testing.T) {
	dsn := resolvePostgresDSN(t)
	ctx := context.Background()

	pg1, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("first NewPostgres: %v", err)
	}
	pg1.Close()

	pg2, err := NewPostgres(ctx, dsn)
	if err != nil {
		t.Fatalf("second NewPostgres (idempotency): %v", err)
	}
	defer pg2.Close()

	var versions int
	if err := pg2.pool.QueryRow(ctx, `SELECT COUNT(*) FROM schema_version`).Scan(&versions); err != nil {
		t.Fatal(err)
	}
	if versions != 1 {
		t.Errorf("schema_version rows = %d, want 1 (one migration, applied once)", versions)
	}
}

// resolvePostgresDSN returns a usable DSN or skips the test.
func resolvePostgresDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("STORE_TEST_DSN"); dsn != "" {
		return dsn
	}
	return startTestcontainerPostgres(t)
}

func startTestcontainerPostgres(t *testing.T) string {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx,
		"postgres:16-alpine",
		tcpostgres.WithDatabase("wren"),
		tcpostgres.WithUsername("wren"),
		tcpostgres.WithPassword("wren"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Skipf("postgres conformance skipped: no STORE_TEST_DSN and testcontainers/Docker unavailable: %v", err)
	}
	t.Cleanup(func() {
		_ = testcontainers.TerminateContainer(container)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	return dsn
}
