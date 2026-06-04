// Package postgres provides pgxpool-based store implementations for the payment service.
package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PoolConfig holds optional configuration overrides for NewPool.
// Zero values fall back to sensible production defaults.
type PoolConfig struct {
	// Schema is the Postgres schema name for schema-per-service isolation.
	// If non-empty every connection runs SET search_path = <quoted-schema>.
	// Must be a validated name (e.g. [a-zA-Z0-9_]+) or a PG reserved word —
	// the identifier is always double-quoted via pgx.Identifier.Sanitize().
	Schema string

	// MaxConns caps the total number of pooled connections (default 10).
	MaxConns int32

	// MinConns is the minimum number of connections kept alive (default 2).
	MinConns int32
}

// NewPool creates and validates a pgxpool.Pool with sensible production defaults.
// Connection budget per CONVENTIONS §12 and backend-security-design §5.3.
//
// Pass a [PoolConfig] (or nil) to override schema / connection counts.
// Legacy variadic-string schema-only form is preserved for backwards compatibility.
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

	return newPoolInternal(ctx, dsn, PoolConfig{Schema: schemaName})
}

// NewPoolWithConfig creates a pgxpool.Pool using the provided [PoolConfig].
// It provides full control over schema and connection-count settings.
func NewPoolWithConfig(ctx context.Context, dsn string, pcfg PoolConfig) (*pgxpool.Pool, error) {
	return newPoolInternal(ctx, dsn, pcfg)
}

func newPoolInternal(ctx context.Context, dsn string, pcfg PoolConfig) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse pgx config: %w", err)
	}

	// Connection count — caller-supplied values take precedence; fall back to sane defaults.
	maxConns := pcfg.MaxConns
	if maxConns <= 0 {
		maxConns = 10
	}

	minConns := pcfg.MinConns
	if minConns < 0 {
		minConns = 0
	}

	if minConns == 0 && pcfg.MinConns == 0 {
		// Default when not explicitly set.
		minConns = 2
	}

	cfg.MaxConns = maxConns
	cfg.MinConns = minConns
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 1 * time.Minute

	schemaName := pcfg.Schema

	// If a schema is configured, set search_path on every acquired connection.
	// Use pgx.Identifier.Sanitize() so PG reserved words (e.g. "user") are double-quoted
	// and safe at the protocol level — validation in config.validate() guards against
	// injection; quoting here handles reserved-word identifiers that PG rejects unquoted.
	if schemaName != "" {
		quotedSchema := pgx.Identifier{schemaName}.Sanitize()
		setSearchPath := "SET search_path = " + quotedSchema

		cfg.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
			if _, execErr := conn.Exec(ctx, setSearchPath); execErr != nil {
				return fmt.Errorf("set search_path=%s: %w", quotedSchema, execErr)
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
	// Use pgx.Identifier.Sanitize() to double-quote the identifier so reserved words work.
	if schemaName != "" {
		quotedSchema := pgx.Identifier{schemaName}.Sanitize()
		createSQL := "CREATE SCHEMA IF NOT EXISTS " + quotedSchema

		if _, execErr := pool.Exec(ctx, createSQL); execErr != nil {
			pool.Close()
			return nil, fmt.Errorf("create schema %q: %w", schemaName, execErr)
		}
	}

	return pool, nil
}
