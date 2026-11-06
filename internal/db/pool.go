// Package db manages the PostgreSQL connection pool and query helpers.
package db

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Pool wraps a pgxpool.Pool with convenience methods.
type Pool struct {
	pool *pgxpool.Pool
}

// NewPool creates a new connection pool for the given database URL.
// It configures sensible defaults (min 5, max 20 connections) and pings
// to verify connectivity before returning.
func NewPool(ctx context.Context, databaseURL string) (*Pool, error) {
	pgxConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}

	pgxConfig.MaxConns = 20
	pgxConfig.MinConns = 5

	pool, err := pgxpool.NewWithConfig(ctx, pgxConfig)
	if err != nil {
		return nil, fmt.Errorf("create connection pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}

	return &Pool{pool: pool}, nil
}

// Close closes the connection pool.
func (p *Pool) Close() {
	p.pool.Close()
}

// Ping verifies the database connection is alive.
func (p *Pool) Ping(ctx context.Context) error {
	return p.pool.Ping(ctx)
}

// PgxPool returns the raw pgxpool.Pool for direct queries.
func (p *Pool) PgxPool() *pgxpool.Pool {
	return p.pool
}

// ConnectWithRetry retries a connection function with exponential backoff.
// It is a standalone helper used by main.go to connect to both Postgres and Redis.
// The backoff starts at 1 second and doubles each attempt, capped at 30 seconds.
func ConnectWithRetry(ctx context.Context, name string, maxRetries int, connect func(ctx context.Context) error) error {
	var err error
	for i := 0; i < maxRetries; i++ {
		err = connect(ctx)
		if err == nil {
			return nil
		}

		if i == maxRetries-1 {
			break
		}

		delay := time.Duration(1<<uint(i)) * time.Second
		if delay > 30*time.Second {
			delay = 30 * time.Second
		}

		slog.Warn("connection failed, retrying",
			"service", name,
			"attempt", i+1,
			"delay", delay,
			"error", err,
		)

		select {
		case <-ctx.Done():
			return fmt.Errorf("connect to %s cancelled: %w", name, ctx.Err())
		case <-time.After(delay):
		}
	}

	return fmt.Errorf("failed to connect to %s after %d attempts: %w", name, maxRetries, err)
}
