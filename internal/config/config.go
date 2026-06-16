// Package config handles environment-first configuration loading for the payment service.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"github.com/redis/go-redis/v9"
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

	// GatewayCIDR is the IP CIDR of the API gateway/load-balancer that forwards
	// requests to this service. When set, Gin is told to trust X-Forwarded-For
	// only from this source, so c.ClientIP() returns the real end-user IP rather
	// than the gateway's IP. This restores per-client behavior the gateway
	// keystone depends on:
	//   - the IP rate limiter keys per real client IP (not one shared gateway
	//     bucket — otherwise the limiter collapses to a single global bucket /
	//     self-DoS behind the gateway).
	// Example: "10.0.0.0/16" (k8s cluster CIDR), "172.16.0.0/12" (VPC internal).
	// Empty (default): trusted-proxy list is nil — c.ClientIP() returns RemoteAddr
	// (safe fallback; use in local dev when no proxy forwards X-Forwarded-For).
	// NEVER set to "0.0.0.0/0" / "::/0": that lets any client spoof their IP via
	// the header, defeating per-client rate limiting.
	// Env: PAYMENT_GATEWAY_CIDR
	GatewayCIDR string `mapstructure:"gateway_cidr"`

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

	// PlatformUserID is the system UUID used as PayerUserID in all settlement
	// disbursement transactions, ensuring payer != payee (self-transfer guard).
	// Non-dev (staging/production): REQUIRED, must be a valid non-nil UUID.
	// Dev: may be empty (zero UUID used as fallback).
	// Env: PAYMENT_PLATFORM_USER_ID
	PlatformUserID string `mapstructure:"platform_user_id"`

	// UserRateLimitPerMin is the per-authenticated-user request budget per minute
	// for the user-facing API group (/v1). 0 disables the per-user limiter (the
	// IP-level limiter still applies). Default: 120.
	// Env: PAYMENT_USER_RATE_LIMIT_PER_MIN
	UserRateLimitPerMin int `mapstructure:"user_rate_limit_per_min"`

	// UserRateLimitBurst is the token-bucket burst allowance for the per-user limiter.
	// Must be > 0 when the per-user limiter is enabled (UserRateLimitPerMin > 0). Default: 20.
	// Env: PAYMENT_USER_RATE_LIMIT_BURST
	UserRateLimitBurst int `mapstructure:"user_rate_limit_burst"`
}

// Load reads configuration from environment variables (prefix PAYMENT_).
func Load() (*Config, error) {
	_ = godotenv.Load(".env.local") // local dev/test (optional, does not override existing env)
	_ = godotenv.Load(".env")       // prod fallback (optional, does not override existing env)

	v := viper.New()

	v.SetEnvPrefix("PAYMENT")
	v.AutomaticEnv()
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))

	bindings := map[string]string{
		"port":                    "PAYMENT_PORT",
		"postgres_dsn":            "PAYMENT_POSTGRES_DSN",
		"db_schema":               "PAYMENT_DB_SCHEMA",
		"db_max_conns":            "PAYMENT_DB_MAX_CONNS",
		"db_min_conns":            "PAYMENT_DB_MIN_CONNS",
		"redis_url":               "PAYMENT_REDIS_URL",
		"log_level":               "PAYMENT_LOG_LEVEL",
		"env":                     "PAYMENT_ENV",
		"gateway_hmac_secret":     "PAYMENT_GATEWAY_HMAC_SECRET",
		"gateway_cidr":            "PAYMENT_GATEWAY_CIDR",
		"settlement_s2s_token":    "PAYMENT_SETTLEMENT_S2S_TOKEN",
		"workspace_base_url":      "PAYMENT_WORKSPACE_BASE_URL",
		"workspace_s2s_token":     "PAYMENT_WORKSPACE_S2S_TOKEN",
		"platform_user_id":        "PAYMENT_PLATFORM_USER_ID",
		"user_rate_limit_per_min": "PAYMENT_USER_RATE_LIMIT_PER_MIN",
		"user_rate_limit_burst":   "PAYMENT_USER_RATE_LIMIT_BURST",
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
	v.SetDefault("user_rate_limit_per_min", 120)
	v.SetDefault("user_rate_limit_burst", 20)

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
	errs = append(errs, c.validateGatewayCIDR()...)
	errs = append(errs, c.validateSettlementS2SToken()...)
	errs = append(errs, c.validateWorkspace()...)
	errs = append(errs, c.validateRedis()...)
	errs = append(errs, c.validatePlatformUserID()...)
	errs = append(errs, c.validateUserRateLimit()...)

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

// validateGatewayCIDR validates PAYMENT_GATEWAY_CIDR and returns any error messages.
// An empty CIDR is valid (trusted-proxy list falls back to nil, safe for local dev).
// A non-empty value must be a valid CIDR block (e.g. "10.0.0.0/16").
// NEVER set to "0.0.0.0/0" / "::/0" — that allows clients to spoof their IP via
// X-Forwarded-For, collapsing the per-IP rate limiter into one shared bucket.
func (c *Config) validateGatewayCIDR() []string {
	if c.GatewayCIDR == "" {
		return nil
	}

	_, ipNet, err := net.ParseCIDR(c.GatewayCIDR)
	if err != nil {
		return []string{fmt.Sprintf("PAYMENT_GATEWAY_CIDR must be a valid CIDR block (e.g. 10.0.0.0/16): %v", err)}
	}

	// Reject wildcard CIDRs (0.0.0.0/0, ::/0): trusting all peers lets any client
	// spoof their IP via X-Forwarded-For, defeating per-client rate limiting.
	if ones, _ := ipNet.Mask.Size(); ones == 0 {
		return []string{
			"PAYMENT_GATEWAY_CIDR must not be a wildcard (0.0.0.0/0 or ::/0): " +
				"it lets any client spoof their IP via X-Forwarded-For",
		}
	}

	return nil
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
//     WorkspaceBaseURL MUST use https:// scheme (sec MAJOR-3).
//   - dev: may be empty (integration tests mock the workspace endpoint).
func (c *Config) validateWorkspace() []string {
	var errs []string

	if !c.IsDev() {
		if c.WorkspaceBaseURL == "" {
			errs = append(errs, "PAYMENT_WORKSPACE_BASE_URL is required in non-dev (staging/production) environments")
		} else {
			parsed, parseErr := url.ParseRequestURI(c.WorkspaceBaseURL)
			if parseErr != nil || parsed.Scheme != "https" {
				errs = append(errs, "PAYMENT_WORKSPACE_BASE_URL must use https:// scheme in non-dev (staging/production) environments")
			}
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

// validateRedis enforces auth and TLS requirements for PAYMENT_REDIS_URL in non-dev:
//   - non-dev: URL must include auth (password); prefer rediss:// (TLS); errors if neither.
//   - dev: may be empty or unauthenticated (local dev Redis is common without auth).
func (c *Config) validateRedis() []string {
	if c.RedisURL == "" {
		// Redis is optional (noop publisher path).
		return nil
	}

	var errs []string

	opts, parseErr := redis.ParseURL(c.RedisURL)
	if parseErr != nil {
		errs = append(errs, "PAYMENT_REDIS_URL is not a valid Redis URL: "+parseErr.Error())
		return errs
	}

	if !c.IsDev() {
		if opts.Password == "" {
			errs = append(errs, "PAYMENT_REDIS_URL must include authentication (password) in non-dev (staging/production) environments")
		}

		if opts.TLSConfig == nil {
			// TLSConfig is non-nil only for the rediss:// scheme; nil means plain redis:// (no TLS).
			errs = append(errs, "PAYMENT_REDIS_URL must use rediss:// (TLS) scheme in non-dev (staging/production) environments")
		}
	}

	return errs
}

// validatePlatformUserID enforces that PAYMENT_PLATFORM_USER_ID is set and a valid UUID
// in non-dev environments (self-transfer guard: payer != payee in disburse transactions).
func (c *Config) validatePlatformUserID() []string {
	if c.PlatformUserID != "" {
		if _, err := uuid.Parse(c.PlatformUserID); err != nil {
			return []string{"PAYMENT_PLATFORM_USER_ID must be a valid UUID"}
		}
	}

	if !c.IsDev() && c.PlatformUserID == "" {
		return []string{"PAYMENT_PLATFORM_USER_ID is required in non-dev (staging/production) environments"}
	}

	return nil
}

// validateUserRateLimit validates the per-user rate-limit configuration:
//   - UserRateLimitPerMin must be >= 0 (0 = disabled).
//   - UserRateLimitBurst must be > 0 when per-user limiting is enabled.
//
// Per-user limiter is disabled (no-op) when UserRateLimitPerMin == 0,
// so burst is irrelevant in that case and not validated.
func (c *Config) validateUserRateLimit() []string {
	var errs []string

	if c.UserRateLimitPerMin < 0 {
		errs = append(errs, "PAYMENT_USER_RATE_LIMIT_PER_MIN must be >= 0 (0 = disabled)")
	}

	if c.UserRateLimitPerMin > 0 && c.UserRateLimitBurst <= 0 {
		errs = append(errs, "PAYMENT_USER_RATE_LIMIT_BURST must be > 0 when per-user limiter is enabled (PAYMENT_USER_RATE_LIMIT_PER_MIN > 0)")
	}

	return errs
}

// PlatformUserUUID returns the platform user ID as a uuid.UUID.
// Returns uuid.Nil in dev when not configured.
func (c *Config) PlatformUserUUID() uuid.UUID {
	if c.PlatformUserID == "" {
		return uuid.Nil
	}

	id, err := uuid.Parse(c.PlatformUserID)
	if err != nil {
		return uuid.Nil
	}

	return id
}
