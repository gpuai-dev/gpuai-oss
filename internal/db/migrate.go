package db

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

// migrationFiles holds the forward SQL migrations, applied in filename order.
// Files ending in .down.sql are rollback scripts and are never applied
// automatically.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationLockKey serializes concurrent migrators (two gpuctl replicas
// booting at once) on a session-level advisory lock. ASCII "GPU_MIGR".
const migrationLockKey int64 = 0x4750555F4D494752

// Migrations returns the forward migration filenames in apply order.
func Migrations() ([]string, error) {
	entries, err := fs.ReadDir(migrationFiles, "migrations")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if e.IsDir() || !strings.HasSuffix(n, ".sql") || strings.HasSuffix(n, ".down.sql") {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	return names, nil
}

// Migrate applies every pending forward migration, each in its own
// transaction, and records it in schema_migrations (version = filename). It
// holds an advisory lock for the duration so concurrent callers apply each
// file exactly once. Returns the filenames it applied.
func (p *Pool) Migrate(ctx context.Context) ([]string, error) {
	names, err := Migrations()
	if err != nil {
		return nil, fmt.Errorf("list migrations: %w", err)
	}

	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire connection: %w", err)
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, migrationLockKey); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	defer func() {
		_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, migrationLockKey)
	}()

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version VARCHAR(255) PRIMARY KEY,
		applied_at TIMESTAMPTZ DEFAULT NOW()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	applied := map[string]bool{}
	rows, err := conn.Query(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			rows.Close()
			return nil, err
		}
		applied[v] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var done []string
	for _, name := range names {
		if applied[name] {
			continue
		}
		body, err := migrationFiles.ReadFile("migrations/" + name)
		if err != nil {
			return done, fmt.Errorf("read %s: %w", name, err)
		}
		err = pgx.BeginFunc(ctx, conn, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(body)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, name)
			return err
		})
		if err != nil {
			return done, fmt.Errorf("apply %s: %w", name, err)
		}
		slog.Info("applied migration", "version", name)
		done = append(done, name)
	}
	return done, nil
}
