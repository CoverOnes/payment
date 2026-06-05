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

	// SettlementS2SToken is the shared secret used by the S2S middleware on the
	// settlement disburse endpoint (backend-security-design §5.5).
	// Trusted internal callers (workspace service) MUST present this token in
	// X-Service-Token.
	// Non-dev (staging/production): REQUIRED, MUST be ≥32 chars.
	// Dev: may be empty; if set must be ≥32 chars.
	// Env: PAYMENT_SETTLEMENT_S2S_TOKEN
	SettlementS2SToken string `mapstructure:"settlement_s2s_token"`

	// WorkspaceBaseURL is the base URL of the workspace service used for S2S calls
	// (e.g. fetching the frozen party roster at plan creation).
	// Non-dev (staging/production): REQUIRED, must be a non-empty URL.
	// Dev: may be empty (consumers still subscribe to Redis; S2S call will fail if called).
	// Env: PAYMENT_WORKSPACE_BASE_URL
	WorkspaceBaseURL string `mapstructure:"workspace_base_url"`

	// WorkspaceS2SToken is the shared secret sent in X-Service-Token when calling
	// workspace internal endpoints (e.g. GET /internal/v1/contracts/:id/parties).
	// Non-dev (staging/production): REQUIRED, MUST be ≥32 chars.
	// Dev: may be empty; if set must be ≥32 chars.
	// Env: PAYMENT_WORKSPACE_S2S_TOKEN
	WorkspaceS2SToken string `mapstructure:"workspace_s2s_token"`
}

// Load reads configuration from environment variables (prefix PAYMENT_).
func Load() (*Config, error) {
	v := viper.New()

	v.SetEnvPrefix("PAYMENT")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                 "PAYMENT_PORT",
		"postgres_dsn":         "PAYMENT_POSTGRES_DSN",
		"db_schema":            "PAYMENT_DB_SCHEMA",
		"db_max_conns":         "PAYMENT_DB_MAX_CONNS",
		"db_min_conns":         "PAYMENT_DB_MIN_CONNS",
		"redis_url":            "PAYMENT_REDIS_URL",
		"log_level":            "PAYMENT_LOG_LEVEL",
		"env":                  "PAYMENT_ENV",
		"gateway_hmac_secret":  "PAYMENT_GATEWAY_HMAC_SECRET",
		"settlement_s2s_token": "PAYMENT_SETTLEMENT_S2S_TOKEN",
		"workspace_base_url":   "PAYMENT_WORKSPACE_BASE_URL",
		"workspace_s2s_token":  "PAYMENT_WORKSPACE_S2S_TOKEN",
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

	if c.DBMaxConns < 0 {
		errs = append(errs, "PAYMENT_DB_MAX_CONNS must be >= 0 (0 = use default 10)")
	}

	if c.DBMinConns < 0 {
		errs = append(errs, "PAYMENT_DB_MIN_CONNS must be >= 0 (default 2)")
	}

	if c.DBMaxConns > 0 && c.DBMinConns > c.DBMaxConns {
		errs = append(errs, "PAYMENT_DB_MIN_CONNS must be <= PAYMENT_DB_MAX_CONNS")
	}

	errs = append(errs, c.validateGatewayHMAC()...)
	errs = append(errs, c.validateSettlementS2SToken()...)
	errs = append(errs, c.validateWorkspace()...)

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

// minS2STokenLen is the minimum length of the settlement S2S token.
// Matches minHMACSecretLen (32 chars) per backend-security-design §5.5.
const minS2STokenLen = 32

// validateSettlementS2SToken enforces the fail-closed secret posture for the
// settlement S2S token (backend-security-design §5.5):
//   - non-dev (staging/production): REQUIRED and MUST be ≥32 chars.
//   - dev: may be empty; if set must be ≥32 chars.
func (c *Config) validateSettlementS2SToken() []string {
	var errs []string

	if !c.IsDev() {
		if len(c.SettlementS2SToken) < minS2STokenLen {
			errs = append(errs, "PAYMENT_SETTLEMENT_S2S_TOKEN must be at least 32 characters in non-dev (staging/production) environments")
		}

		return errs
	}

	// Dev: empty is allowed; non-empty must be ≥32.
	if c.SettlementS2SToken != "" && len(c.SettlementS2SToken) < minS2STokenLen {
		errs = append(errs, "PAYMENT_SETTLEMENT_S2S_TOKEN, when set, must be at least 32 characters")
	}

	return errs
}

// validateWorkspace enforces the fail-closed posture for workspace S2S config:
//   - non-dev (staging/production): WorkspaceBaseURL and WorkspaceS2SToken are REQUIRED.
//   - dev: may be empty (integration tests mock the workspace endpoint).
func (c *Config) validateWorkspace() []string {
	var errs []string

	if !c.IsDev() {
		if c.WorkspaceBaseURL == "" {
			errs = append(errs, "PAYMENT_WORKSPACE_BASE_URL is required in non-dev (staging/production) environments")
		}

		if len(c.WorkspaceS2SToken) < minS2STokenLen {
			errs = append(errs, "PAYMENT_WORKSPACE_S2S_TOKEN must be at least 32 characters in non-dev (staging/production) environments")
		}

		return errs
	}

	// Dev: if set, WorkspaceS2SToken must be ≥32.
	if c.WorkspaceS2SToken != "" && len(c.WorkspaceS2SToken) < minS2STokenLen {
		errs = append(errs, "PAYMENT_WORKSPACE_S2S_TOKEN, when set, must be at least 32 characters")
	}

	return errs
}
