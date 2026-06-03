// Package config handles environment-first configuration loading for the payment service.
package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/spf13/viper"
)

// schemaNameRE validates a Postgres schema name: letters, digits, underscores only.
// Empty string is explicitly allowed (means use public schema / default search_path).
var schemaNameRE = regexp.MustCompile(`^[a-zA-Z0-9_]+$`)

// Config holds all configuration for the payment service.
type Config struct {
	// Server
	Port int `mapstructure:"port"`

	// Postgres
	PostgresDSN string `mapstructure:"postgres_dsn"`

	// DBSchema is an optional Postgres schema name (default: "", meaning public).
	// Set to e.g. "payment" when sharing one Aiven database across services.
	// Each connection will run SET search_path = <schema> on connect.
	DBSchema string `mapstructure:"db_schema"`

	// DBMaxConns caps the total number of connections in the pgxpool (default 10).
	// Reduce when running many services against a small Aiven connection budget.
	DBMaxConns int `mapstructure:"db_max_conns"`

	// DBMinConns is the minimum number of connections kept alive in the pgxpool (default 2).
	DBMinConns int `mapstructure:"db_min_conns"`

	// Redis (optional — nil Redis = event publish no-op + in-process rate limiter)
	RedisURL string `mapstructure:"redis_url"`

	// Log level: DEBUG, INFO, WARN, ERROR
	LogLevel string `mapstructure:"log_level"`

	// Environment: development | production | test
	Env string `mapstructure:"env"`
}

// Load reads configuration from environment variables (prefix PAYMENT_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("PAYMENT")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":         "PAYMENT_PORT",
		"postgres_dsn": "PAYMENT_POSTGRES_DSN",
		"db_schema":    "PAYMENT_DB_SCHEMA",
		"db_max_conns": "PAYMENT_DB_MAX_CONNS",
		"db_min_conns": "PAYMENT_DB_MIN_CONNS",
		"redis_url":    "PAYMENT_REDIS_URL",
		"log_level":    "PAYMENT_LOG_LEVEL",
		"env":          "PAYMENT_ENV",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8084)
	v.SetDefault("log_level", "INFO")
	v.SetDefault("env", "development")
	v.SetDefault("db_max_conns", 10)
	v.SetDefault("db_min_conns", 2)

	var cfg Config

	if err := v.Unmarshal(&cfg); err != nil {
		return nil, fmt.Errorf("unmarshal config: %w", err)
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return &cfg, nil
}

func (c *Config) validate() error {
	var errs []string

	if c.PostgresDSN == "" {
		errs = append(errs, "PAYMENT_POSTGRES_DSN is required")
	}

	if c.Port <= 0 || c.Port > 65535 {
		errs = append(errs, "PAYMENT_PORT must be 1-65535")
	}

	validLogLevels := map[string]bool{"DEBUG": true, "INFO": true, "WARN": true, "ERROR": true}
	if !validLogLevels[strings.ToUpper(c.LogLevel)] {
		errs = append(errs, "PAYMENT_LOG_LEVEL must be DEBUG|INFO|WARN|ERROR")
	}

	validEnvs := map[string]bool{"development": true, "production": true, "test": true}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "PAYMENT_ENV must be development|production|test")
	}

	// Validate db_schema: empty is allowed (public/default); non-empty must be [a-zA-Z0-9_]+
	// to prevent SQL injection in the CREATE SCHEMA / SET search_path statement.
	if c.DBSchema != "" && !schemaNameRE.MatchString(c.DBSchema) {
		errs = append(errs, "PAYMENT_DB_SCHEMA must contain only letters, digits, and underscores")
	}

	if c.DBMaxConns <= 0 {
		errs = append(errs, "PAYMENT_DB_MAX_CONNS must be a positive integer (default 10)")
	}

	if c.DBMinConns < 0 {
		errs = append(errs, "PAYMENT_DB_MIN_CONNS must be >= 0 (default 2)")
	}

	if c.DBMaxConns > 0 && c.DBMinConns > c.DBMaxConns {
		errs = append(errs, "PAYMENT_DB_MIN_CONNS must be <= PAYMENT_DB_MAX_CONNS")
	}

	if len(errs) > 0 {
		return errors.New("config validation failed: " + strings.Join(errs, "; "))
	}

	return nil
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
