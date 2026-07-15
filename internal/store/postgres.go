package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// migrationsFS holds the forward-only SQL migrations. Two tables don't justify
// a migration framework (spec §5.2 / implementation-plan §WS-3): a tiny in-code
// migrator runs the embedded files in filename order and records the highest
// applied version in schema_version. Revisit at v0.3.
//
//go:embed migrations/*.sql
var migrationsFS embed.FS

// Postgres is a Store backed by Postgres via pgx/v5. It is the durable
// counterpart to Memory; callers depend only on the Store interface. Semantics
// (ErrNotFound / ErrExists, copy-on-write, newest-first ListRuns) match Memory
// exactly — the conformance suite runs against both.
type Postgres struct {
	pool *pgxpool.Pool
}

var _ Store = (*Postgres)(nil)

// NewPostgres opens a pool against dsn, runs pending migrations, and returns a
// ready store. The caller owns Close.
func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("open postgres pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return p, nil
}

// Close releases the connection pool.
func (p *Postgres) Close() { p.pool.Close() }

// migrate applies every embedded migration whose version exceeds the highest
// recorded in schema_version, in filename order, each in its own transaction.
func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_version (
			version    integer     PRIMARY KEY,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("ensure schema_version: %w", err)
	}

	var current int
	if err := p.pool.QueryRow(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM schema_version`).Scan(&current); err != nil {
		return fmt.Errorf("read schema_version: %w", err)
	}

	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations dir: %w", err)
	}
	// ReadDir returns lexical order; filenames are zero-padded so it is also
	// numeric order (0001_, 0002_, ...).
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		version, err := migrationVersion(name)
		if err != nil {
			return err
		}
		if version <= current {
			continue
		}
		sqlBytes, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		if err := p.applyMigration(ctx, version, string(sqlBytes)); err != nil {
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
	}
	return nil
}

func (p *Postgres) applyMigration(ctx context.Context, version int, sql string) error {
	tx, err := p.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort; commit path returns the real error

	if _, err := tx.Exec(ctx, sql); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx,
		`INSERT INTO schema_version (version) VALUES ($1)`, version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// migrationVersion parses the leading integer of a "NNNN_name.sql" filename.
func migrationVersion(name string) (int, error) {
	base := strings.SplitN(name, "_", 2)[0]
	v, err := strconv.Atoi(base)
	if err != nil {
		return 0, fmt.Errorf("migration %q: filename must start with a version number: %w", name, err)
	}
	return v, nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation (23505),
// which we surface as ErrExists to match Memory.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// --- Projects ---

func (p *Postgres) CreateProject(ctx context.Context, proj *Project) error {
	if proj.EgressAllowlist == nil {
		proj.EgressAllowlist = []string{}
	}
	_, err := p.pool.Exec(ctx, `
		INSERT INTO projects (
			name, repo, default_harness, harness_image, default_model,
			runtime_class, cpu, memory, disk, checkpoint_bucket,
			egress_allowlist, namespace, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		proj.Name, proj.Repo, proj.DefaultHarness, proj.HarnessImage, proj.DefaultModel,
		proj.RuntimeClass, proj.CPU, proj.Memory, proj.Disk, proj.CheckpointBucket,
		proj.EgressAllowlist, proj.Namespace, proj.CreatedAt)
	if isUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert project: %w", err)
	}
	return nil
}

const projectCols = `
	name, repo, default_harness, harness_image, default_model,
	runtime_class, cpu, memory, disk, checkpoint_bucket,
	egress_allowlist, namespace, created_at`

func scanProject(row pgx.Row) (*Project, error) {
	var p Project
	if err := row.Scan(
		&p.Name, &p.Repo, &p.DefaultHarness, &p.HarnessImage, &p.DefaultModel,
		&p.RuntimeClass, &p.CPU, &p.Memory, &p.Disk, &p.CheckpointBucket,
		&p.EgressAllowlist, &p.Namespace, &p.CreatedAt,
	); err != nil {
		return nil, err
	}
	// Normalize timestamps to UTC so store impls compare equal in tests.
	p.CreatedAt = p.CreatedAt.UTC()
	return &p, nil
}

func (p *Postgres) GetProject(ctx context.Context, name string) (*Project, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+projectCols+` FROM projects WHERE name = $1`, name)
	proj, err := scanProject(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get project: %w", err)
	}
	return proj, nil
}

func (p *Postgres) ListProjects(ctx context.Context) ([]*Project, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+projectCols+` FROM projects ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("list projects: %w", err)
	}
	defer rows.Close()

	out := []*Project{}
	for rows.Next() {
		proj, err := scanProject(rows)
		if err != nil {
			return nil, fmt.Errorf("scan project: %w", err)
		}
		out = append(out, proj)
	}
	return out, rows.Err()
}

// --- Runs ---

func (p *Postgres) CreateRun(ctx context.Context, r *Run) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO runs (
			id, project, "user", prompt, harness, model, base_ref,
			interactive, runtime, namespace, phase, pr_url, restart_count, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		r.ID, r.Project, r.User, r.Prompt, r.Harness, r.Model, r.BaseRef,
		r.Interactive, r.Runtime, r.Namespace, r.Phase, r.PRURL, r.RestartCount, r.CreatedAt)
	if isUniqueViolation(err) {
		return ErrExists
	}
	if err != nil {
		return fmt.Errorf("insert run: %w", err)
	}
	return nil
}

const runCols = `
	id, project, "user", prompt, harness, model, base_ref,
	interactive, runtime, namespace, phase, pr_url, restart_count, created_at`

func scanRun(row pgx.Row) (*Run, error) {
	var r Run
	if err := row.Scan(
		&r.ID, &r.Project, &r.User, &r.Prompt, &r.Harness, &r.Model, &r.BaseRef,
		&r.Interactive, &r.Runtime, &r.Namespace, &r.Phase, &r.PRURL, &r.RestartCount, &r.CreatedAt,
	); err != nil {
		return nil, err
	}
	r.CreatedAt = r.CreatedAt.UTC()
	return &r, nil
}

func (p *Postgres) GetRun(ctx context.Context, id string) (*Run, error) {
	row := p.pool.QueryRow(ctx, `SELECT `+runCols+` FROM runs WHERE id = $1`, id)
	r, err := scanRun(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	return r, nil
}

func (p *Postgres) ListRuns(ctx context.Context, f RunFilter) ([]*Run, error) {
	// Build a filtered query; empty filter fields are omitted. Newest-first with
	// an id tie-break matches Memory's ordering.
	conds := []string{}
	args := []any{}
	if f.User != "" {
		args = append(args, f.User)
		conds = append(conds, `"user" = $`+strconv.Itoa(len(args)))
	}
	if f.Project != "" {
		args = append(args, f.Project)
		conds = append(conds, `project = $`+strconv.Itoa(len(args)))
	}
	q := `SELECT ` + runCols + ` FROM runs`
	if len(conds) > 0 {
		q += ` WHERE ` + strings.Join(conds, " AND ")
	}
	q += ` ORDER BY created_at DESC, id ASC`

	rows, err := p.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list runs: %w", err)
	}
	defer rows.Close()

	out := []*Run{}
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (p *Postgres) UpdateRun(ctx context.Context, r *Run) error {
	tag, err := p.pool.Exec(ctx, `
		UPDATE runs SET
			project = $2, "user" = $3, prompt = $4, harness = $5, model = $6,
			base_ref = $7, interactive = $8, runtime = $9, namespace = $10,
			phase = $11, pr_url = $12, restart_count = $13, created_at = $14
		WHERE id = $1`,
		r.ID, r.Project, r.User, r.Prompt, r.Harness, r.Model,
		r.BaseRef, r.Interactive, r.Runtime, r.Namespace,
		r.Phase, r.PRURL, r.RestartCount, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// UpsertRun inserts or replaces a run by id. It backs reconcile-on-boot: a
// restarted apiserver re-learns in-flight runs from the AgentRun CRs, and must
// tolerate rows that already exist (from before the restart) as well as ones it
// has never seen. Not part of the Store interface — it is a durability seam used
// only at boot, so it lives on the concrete types (Memory has a matching one).
var _ upserter = (*Postgres)(nil)

func (p *Postgres) UpsertRun(ctx context.Context, r *Run) error {
	_, err := p.pool.Exec(ctx, `
		INSERT INTO runs (
			id, project, "user", prompt, harness, model, base_ref,
			interactive, runtime, namespace, phase, pr_url, restart_count, created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (id) DO UPDATE SET
			project = EXCLUDED.project, "user" = EXCLUDED."user",
			prompt = EXCLUDED.prompt, harness = EXCLUDED.harness,
			model = EXCLUDED.model, base_ref = EXCLUDED.base_ref,
			interactive = EXCLUDED.interactive, runtime = EXCLUDED.runtime,
			namespace = EXCLUDED.namespace, phase = EXCLUDED.phase,
			pr_url = EXCLUDED.pr_url, restart_count = EXCLUDED.restart_count`,
		r.ID, r.Project, r.User, r.Prompt, r.Harness, r.Model, r.BaseRef,
		r.Interactive, r.Runtime, r.Namespace, r.Phase, r.PRURL, r.RestartCount, r.CreatedAt)
	if err != nil {
		return fmt.Errorf("upsert run: %w", err)
	}
	return nil
}
