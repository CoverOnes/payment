// Package postgres provides pgxpool-based store implementations for the payment service.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// NewPool creates and validates a pgxpool.Pool with sensible production defaults.
// Connection budget per CONVENTIONS §12 and backend-security-design §5.3.
//
// If schema is non-empty, the pool will:
//  1. CREATE SCHEMA IF NOT EXISTS <schema> (once, before first use).
//  2. Set search_path=<schema> on every connection via AfterConnect hook.
//
// If schema is empty, behavior is identical to the pre-schema version (public schema).
// The schema name MUST already be validated by config (only [a-zA-Z0-9_]+).
func NewPool(ctx context.Context, dsn string, schema ...string) (*pgxpool.Pool, error) {
	schemaName := ""
	if len(schema) > 0 {
		schemaName = schema[0]
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}

	cfg.MaxConns = 10
	cfg.MinConns = 2
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	// If a schema is configured, set search_path on every acquired connection.
	// schemaName is already validated as [a-zA-Z0-9_]+ by config.validate(); safe for interpolation.
	if schemaName != "" {
		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			// SET search_path uses the validated schema name (config.validate ensures [a-zA-Z0-9_]+).
			_, execErr := conn.Exec(ctx, "SET search_path = "+schemaName)
			if execErr != nil {
				return fmt.Errorf("set search_path=%s: %w", schemaName, execErr)
			}

			return nil
		}
	}

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pgxpool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	// Ensure the schema exists (idempotent). Only needed when schema is non-empty.
	if schemaName != "" {
		// schemaName validated as [a-zA-Z0-9_]+ by config.validate() — safe for interpolation.
		if _, execErr := pool.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schemaName); execErr != nil {
			pool.Close()
			return nil, fmt.Errorf("create schema %q: %w", schemaName, execErr)
		}
	}

	return pool, nil
}
