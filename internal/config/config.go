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

	// Redis (optional — nil Redis = event publish no-op + in-process rate limiter)
	RedisURL string `mapstructure:"redis_url"`

	// Log level: DEBUG, INFO, WARN, ERROR
	LogLevel string `mapstructure:"log_level"`

	// Environment: development | staging | production. REQUIRED — there is no
	// safe default. An empty or unknown value is a boot error (fail-closed): a
	// silent default to "production" would mask a misconfigured deploy, and a
	// silent default to "development" would disable gateway-signature
	// verification §24.1 (the forged-identity-header hole). Operators MUST set
	// PAYMENT_ENV explicitly.
	Env string `mapstructure:"env"`

	// GatewayHMACSecret is the shared secret used to verify the gateway-origin
	// identity signature (conventions §24.1). It MUST equal the gateway's
	// GATEWAY_HMAC_SECRET. Non-dev (staging/production) fails fast at boot if
	// empty or shorter than 32 chars; development may omit it (verification
	// disabled, mirroring the gateway which also disables signing in dev).
	// chmod 0600 the file that provides it; prefer the env var as canonical.
	// Env: PAYMENT_GATEWAY_HMAC_SECRET
	GatewayHMACSecret string `mapstructure:"gateway_hmac_secret"`
}

// Load reads configuration from environment variables (prefix PAYMENT_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("PAYMENT")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                "PAYMENT_PORT",
		"postgres_dsn":        "PAYMENT_POSTGRES_DSN",
		"db_schema":           "PAYMENT_DB_SCHEMA",
		"redis_url":           "PAYMENT_REDIS_URL",
		"log_level":           "PAYMENT_LOG_LEVEL",
		"env":                 "PAYMENT_ENV",
		"gateway_hmac_secret": "PAYMENT_GATEWAY_HMAC_SECRET",
	}

	for key, envKey := range bindings {
		if err := v.BindEnv(key, envKey); err != nil {
			return nil, fmt.Errorf("config bind %q: %w", key, err)
		}
	}

	v.SetDefault("port", 8084)
	v.SetDefault("log_level", "INFO")
	// NOTE: PAYMENT_ENV has NO default — it is required and validated
	// explicitly below. A silent default to "production" would mask a
	// misconfigured deploy; a default to "development" would disable
	// gateway-signature verification §24.1. Fail-closed: empty env → boot error.

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

	// Fail-closed env posture: PAYMENT_ENV MUST be one of the three known
	// values. Empty (unset) or any unknown string is a boot error — no silent
	// default. This guards §24.1: a misconfigured env must never silently land
	// in a posture that disables gateway-signature verification.
	validEnvs := map[string]bool{"development": true, "staging": true, "production": true}
	if !validEnvs[strings.ToLower(c.Env)] {
		errs = append(errs, "PAYMENT_ENV must be explicitly set to development|staging|production")
	}

	// Validate db_schema: empty is allowed (public/default); non-empty must be [a-zA-Z0-9_]+
	// to prevent SQL injection in the CREATE SCHEMA / SET search_path statement.
	if c.DBSchema != "" && !schemaNameRE.MatchString(c.DBSchema) {
		errs = append(errs, "PAYMENT_DB_SCHEMA must contain only letters, digits, and underscores")
	}

	errs = append(errs, c.validateGatewayHMAC()...)

	if len(errs) > 0 {
		return errors.New("config validation failed: " + strings.Join(errs, "; "))
	}

	return nil
}

// minHMACSecretLen is the minimum length of the gateway HMAC secret. It mirrors
// the gateway's GATEWAY_HMAC_SECRET ≥32-char requirement (conventions §24.1).
const minHMACSecretLen = 32

// validateGatewayHMAC enforces the §24.1 fail-closed secret posture:
//   - non-dev (staging/production): secret is REQUIRED and MUST be ≥32 chars —
//     boot fails fast otherwise (mirrors the gateway which fails fast in non-dev).
//   - dev: secret may be empty (verification disabled, mirroring the gateway's
//     dev signing-skip); but if a secret IS provided it must still be ≥32 chars
//     so a too-short dev secret never masquerades as a valid one.
func (c *Config) validateGatewayHMAC() []string {
	var errs []string

	if !c.IsDev() {
		if len(c.GatewayHMACSecret) < minHMACSecretLen {
			errs = append(errs, "PAYMENT_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev (staging/production) environments")
		}

		return errs
	}

	// Dev: empty is allowed (verification disabled); non-empty must be ≥32.
	if c.GatewayHMACSecret != "" && len(c.GatewayHMACSecret) < minHMACSecretLen {
		errs = append(errs, "PAYMENT_GATEWAY_HMAC_SECRET, when set, must be at least 32 characters")
	}

	return errs
}

// IsDev reports whether the service is running in development mode.
func (c *Config) IsDev() bool {
	return strings.EqualFold(c.Env, "development")
}
