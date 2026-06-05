package config_test

import (
	"testing"

	"github.com/CoverOnes/payment/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testDSN is a placeholder DSN used in tests only — not a real credential.
//
//nolint:gosec // G101: test fixture placeholder, not a real credential; user:pass are inert strings
const testDSN = "postgres://user:pass@localhost/db"

// testHMACSecret is a 32-char placeholder HMAC secret used in tests only — not a real secret.
const testHMACSecret = "0123456789abcdef0123456789abcdef"

// testS2SToken is a 32-char placeholder S2S token used in tests only — not a real secret.
const testS2SToken = "abcdef0123456789abcdef0123456789"

// testWorkspaceToken is a 32-char placeholder workspace S2S token for tests only — not a real secret.
const testWorkspaceToken = "workspace0123456789abcdef0123456"

// testPlatformUserID is a valid UUID used as PAYMENT_PLATFORM_USER_ID in non-dev tests.
const testPlatformUserID = "00000000-0000-0000-0000-000000000001"

// envDevelopment is the development environment name used as a test constant to satisfy goconst.
const envDevelopment = "development"

// setValidEnv sets the minimum valid environment variables for a development config.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PAYMENT_POSTGRES_DSN", testDSN)
	t.Setenv("PAYMENT_PORT", "8084")
	t.Setenv("PAYMENT_LOG_LEVEL", "INFO")
	t.Setenv("PAYMENT_ENV", "development")
}

func TestLoad_MissingDSN(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PAYMENT_POSTGRES_DSN", "")

	_, err := config.Load()
	require.Error(t, err, "Load() must fail when PAYMENT_POSTGRES_DSN is empty")
	assert.Contains(t, err.Error(), "PAYMENT_POSTGRES_DSN")
}

func TestLoad_InvalidPort(t *testing.T) {
	tests := []struct {
		name string
		port string
	}{
		{"zero", "0"},
		{"negative", "-1"},
		{"too_large", "99999"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_PORT", tc.port)

			_, err := config.Load()
			require.Error(t, err, "Load() must fail for port %s", tc.port)
			assert.Contains(t, err.Error(), "PAYMENT_PORT")
		})
	}
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	setValidEnv(t)
	t.Setenv("PAYMENT_LOG_LEVEL", "VERBOSE")

	_, err := config.Load()
	require.Error(t, err, "Load() must fail for unknown log level")
	assert.Contains(t, err.Error(), "PAYMENT_LOG_LEVEL")
}

// TestLoad_Env_FailClosed verifies the fail-closed env posture: PAYMENT_ENV MUST be
// set explicitly; empty or unknown values are boot errors (no silent default).
func TestLoad_Env_FailClosed(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "development is valid",
			env:     envDevelopment,
			wantErr: false,
		},
		{
			name:      "staging is valid",
			env:       "staging",
			wantErr:   true, // staging requires HMAC secret — this test sets no secret
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET",
		},
		{
			name:      "production is valid",
			env:       "production",
			wantErr:   true, // production requires HMAC secret — this test sets no secret
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET",
		},
		{
			name:      "empty env is rejected (fail-closed: no silent default)",
			env:       "",
			wantErr:   true,
			errSubstr: "PAYMENT_ENV must be explicitly set",
		},
		{
			name:      "unknown env 'prod' is rejected",
			env:       "prod",
			wantErr:   true,
			errSubstr: "PAYMENT_ENV must be explicitly set",
		},
		{
			name:      "'test' is not a valid env (fail-closed)",
			env:       "test",
			wantErr:   true,
			errSubstr: "PAYMENT_ENV must be explicitly set",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			// Clear HMAC secret so only env is the variable under test for the dev case.
			t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", "")

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_GatewayHMAC verifies §24.1 fail-closed secret posture for PAYMENT_GATEWAY_HMAC_SECRET.
func TestLoad_GatewayHMAC(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		secret    string
		wantErr   bool
		errSubstr string
	}{
		{
			// §24.1: dev may omit the secret (verification disabled).
			name:    "dev with empty secret is allowed",
			env:     envDevelopment,
			secret:  "",
			wantErr: false,
		},
		{
			// §24.1: non-dev MUST have a ≥32-char secret — boot fails fast.
			name:      "production without gateway secret fails (fail-closed)",
			env:       "production",
			secret:    "",
			wantErr:   true,
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			name:      "staging without gateway secret fails (fail-closed)",
			env:       "staging",
			secret:    "",
			wantErr:   true,
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			// Even in dev a too-short secret is an error (catches typos).
			name:      "dev with too-short secret is rejected",
			env:       envDevelopment,
			secret:    "tooshort",
			wantErr:   true,
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET, when set, must be at least 32 characters",
		},
		{
			name:      "production with too-short secret is rejected",
			env:       "production",
			secret:    "tooshort",
			wantErr:   true,
			errSubstr: "PAYMENT_GATEWAY_HMAC_SECRET must be at least 32 characters in non-dev",
		},
		{
			name:    "production with valid 32-char secret passes",
			env:     "production",
			secret:  testHMACSecret,
			wantErr: false,
		},
		{
			name:    "staging with valid 32-char secret passes",
			env:     "staging",
			secret:  testHMACSecret,
			wantErr: false,
		},
		{
			name:    "dev with valid 32-char secret passes",
			env:     envDevelopment,
			secret:  testHMACSecret,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", tc.secret)
			// Non-dev requires S2S token + workspace config + platform user ID; set them so HMAC is the only variable.
			if tc.env == "production" || tc.env == "staging" {
				t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
				t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "https://workspace:8081")
				t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
				t.Setenv("PAYMENT_PLATFORM_USER_ID", testPlatformUserID)
			}

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestIsDev(t *testing.T) {
	tests := []struct {
		env   string
		isDev bool
	}{
		{envDevelopment, true},
		{"DEVELOPMENT", true},
		{"production", false},
		{"staging", false},
	}

	for _, tc := range tests {
		t.Run(tc.env, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			// Non-dev envs require gateway HMAC secret (§24.1 fail-closed),
			// settlement S2S token (backend-security-design §5.5),
			// workspace config (PAYMENT_WORKSPACE_BASE_URL + PAYMENT_WORKSPACE_S2S_TOKEN),
			// and platform user ID (self-transfer guard).
			t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
			t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
			t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "https://workspace:8081")
			t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
			t.Setenv("PAYMENT_PLATFORM_USER_ID", testPlatformUserID)

			cfg, err := config.Load()
			require.NoError(t, err)
			assert.Equal(t, tc.isDev, cfg.IsDev(), "IsDev() for env=%s", tc.env)
		})
	}
}

// TestLoad_SettlementS2SToken verifies fail-closed secret posture for PAYMENT_SETTLEMENT_S2S_TOKEN.
func TestLoad_SettlementS2SToken(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		token     string
		wantErr   bool
		errSubstr string
	}{
		{
			// dev: empty token is allowed (endpoint not yet wired in PR1).
			name:    "dev with empty token is allowed",
			env:     envDevelopment,
			token:   "",
			wantErr: false,
		},
		{
			// non-dev: token is required and MUST be ≥32 chars.
			name:      "production without token fails (fail-closed)",
			env:       "production",
			token:     "",
			wantErr:   true,
			errSubstr: "PAYMENT_SETTLEMENT_S2S_TOKEN must be at least 32 characters in non-dev",
		},
		{
			name:      "staging without token fails (fail-closed)",
			env:       "staging",
			token:     "",
			wantErr:   true,
			errSubstr: "PAYMENT_SETTLEMENT_S2S_TOKEN must be at least 32 characters in non-dev",
		},
		{
			// dev: too-short token is rejected (catches typos).
			name:      "dev with too-short token is rejected",
			env:       envDevelopment,
			token:     "tooshort",
			wantErr:   true,
			errSubstr: "PAYMENT_SETTLEMENT_S2S_TOKEN, when set, must be at least 32 characters",
		},
		{
			name:      "production with too-short token is rejected",
			env:       "production",
			token:     "tooshort",
			wantErr:   true,
			errSubstr: "PAYMENT_SETTLEMENT_S2S_TOKEN must be at least 32 characters in non-dev",
		},
		{
			name:    "production with valid 32-char token passes",
			env:     "production",
			token:   testS2SToken,
			wantErr: false,
		},
		{
			name:    "dev with valid 32-char token passes",
			env:     envDevelopment,
			token:   testS2SToken,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			// Provide the HMAC secret + workspace config + platform user ID so they don't interfere.
			if tc.env != envDevelopment {
				t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
				t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "https://workspace:8081")
				t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
				t.Setenv("PAYMENT_PLATFORM_USER_ID", testPlatformUserID)
			}
			t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", tc.token)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_WorkspaceConfig verifies fail-closed posture for workspace S2S config.
func TestLoad_WorkspaceConfig(t *testing.T) {
	tests := []struct {
		name           string
		env            string
		baseURL        string
		workspaceToken string
		wantErr        bool
		errSubstr      string
	}{
		{
			name:           "dev: empty workspace config allowed",
			env:            envDevelopment,
			baseURL:        "",
			workspaceToken: "",
			wantErr:        false,
		},
		{
			name:           "production: missing base URL fails",
			env:            "production",
			baseURL:        "",
			workspaceToken: testWorkspaceToken,
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_BASE_URL is required",
		},
		{
			name:           "production: missing workspace token fails",
			env:            "production",
			baseURL:        "https://workspace:8081",
			workspaceToken: "",
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_S2S_TOKEN must be at least 32 characters",
		},
		{
			name:           "production: too-short workspace token fails",
			env:            "production",
			baseURL:        "https://workspace:8081",
			workspaceToken: "tooshort",
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_S2S_TOKEN must be at least 32 characters",
		},
		{
			name:           "production: valid workspace config passes",
			env:            "production",
			baseURL:        "https://workspace:8081",
			workspaceToken: testWorkspaceToken,
			wantErr:        false,
		},
		{
			name:           "dev: too-short workspace token fails even in dev",
			env:            envDevelopment,
			baseURL:        "http://localhost:8081",
			workspaceToken: "tooshort",
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_S2S_TOKEN, when set, must be at least 32 characters",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			t.Setenv("PAYMENT_WORKSPACE_BASE_URL", tc.baseURL)
			t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", tc.workspaceToken)

			if tc.env != envDevelopment {
				t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
				t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
				t.Setenv("PAYMENT_PLATFORM_USER_ID", testPlatformUserID)
			}

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestLoad_DBSchema(t *testing.T) {
	tests := []struct {
		name      string
		schema    string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "empty schema allowed (public default)",
			schema:  "",
			wantErr: false,
		},
		{
			name:    "valid alphanumeric schema",
			schema:  "payment",
			wantErr: false,
		},
		{
			name:      "schema with hyphen rejected",
			schema:    "my-schema",
			wantErr:   true,
			errSubstr: "PAYMENT_DB_SCHEMA",
		},
		{
			name:      "schema with semicolon rejected (SQL injection attempt)",
			schema:    "payment;DROP TABLE audit",
			wantErr:   true,
			errSubstr: "PAYMENT_DB_SCHEMA",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_DB_SCHEMA", tc.schema)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// setValidNonDevEnv sets up a valid non-dev (production) environment for tests.
// Only use this when the test subject is something OTHER than the fields set here.
func setValidNonDevEnv(t *testing.T) {
	t.Helper()
	t.Setenv("PAYMENT_POSTGRES_DSN", testDSN)
	t.Setenv("PAYMENT_PORT", "8084")
	t.Setenv("PAYMENT_LOG_LEVEL", "INFO")
	t.Setenv("PAYMENT_ENV", "production")
	t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
	t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
	t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "https://workspace:8081")
	t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
	t.Setenv("PAYMENT_PLATFORM_USER_ID", testPlatformUserID)
}

// TestLoad_WorkspaceBaseURL_HTTPS verifies that non-dev requires https:// scheme (sec MAJOR-3).
func TestLoad_WorkspaceBaseURL_HTTPS(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		baseURL   string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "production: https:// passes",
			env:     "production",
			baseURL: "https://workspace:8081",
			wantErr: false,
		},
		{
			name:      "production: http:// fails (insecure)",
			env:       "production",
			baseURL:   "http://workspace:8081",
			wantErr:   true,
			errSubstr: "PAYMENT_WORKSPACE_BASE_URL must use https://",
		},
		{
			name:      "staging: http:// fails (insecure)",
			env:       "staging",
			baseURL:   "http://workspace:8081",
			wantErr:   true,
			errSubstr: "PAYMENT_WORKSPACE_BASE_URL must use https://",
		},
		{
			name:    "dev: http:// allowed",
			env:     envDevelopment,
			baseURL: "http://localhost:8081",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == envDevelopment {
				setValidEnv(t)
			} else {
				setValidNonDevEnv(t)
				t.Setenv("PAYMENT_ENV", tc.env)
			}

			t.Setenv("PAYMENT_WORKSPACE_BASE_URL", tc.baseURL)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_PlatformUserID verifies PAYMENT_PLATFORM_USER_ID validation (self-transfer guard).
func TestLoad_PlatformUserID(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		id        string
		wantErr   bool
		errSubstr string
	}{
		{
			name:    "dev: empty ID allowed",
			env:     envDevelopment,
			id:      "",
			wantErr: false,
		},
		{
			name:    "dev: valid UUID allowed",
			env:     envDevelopment,
			id:      testPlatformUserID,
			wantErr: false,
		},
		{
			name:      "dev: invalid UUID rejected",
			env:       envDevelopment,
			id:        "not-a-uuid",
			wantErr:   true,
			errSubstr: "PAYMENT_PLATFORM_USER_ID must be a valid UUID",
		},
		{
			name:      "production: empty ID rejected (self-transfer guard required)",
			env:       "production",
			id:        "",
			wantErr:   true,
			errSubstr: "PAYMENT_PLATFORM_USER_ID is required",
		},
		{
			name:    "production: valid UUID passes",
			env:     "production",
			id:      testPlatformUserID,
			wantErr: false,
		},
		{
			name:      "production: invalid UUID rejected",
			env:       "production",
			id:        "not-valid-uuid",
			wantErr:   true,
			errSubstr: "PAYMENT_PLATFORM_USER_ID must be a valid UUID",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == envDevelopment {
				setValidEnv(t)
			} else {
				setValidNonDevEnv(t)
			}

			t.Setenv("PAYMENT_PLATFORM_USER_ID", tc.id)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_UserRateLimit verifies validateUserRateLimit: perMin<0 rejected, burst<=0 with
// perMin>0 rejected, perMin=0 disabled (pass without burst), and a valid enabled case.
func TestLoad_UserRateLimit(t *testing.T) {
	tests := []struct {
		name      string
		perMin    string
		burst     string
		wantErr   bool
		errSubstr string
	}{
		{
			name:      "perMin<0 is rejected",
			perMin:    "-1",
			burst:     "10",
			wantErr:   true,
			errSubstr: "PAYMENT_USER_RATE_LIMIT_PER_MIN must be >= 0",
		},
		{
			name:      "perMin>0 with burst<=0 is rejected",
			perMin:    "60",
			burst:     "0",
			wantErr:   true,
			errSubstr: "PAYMENT_USER_RATE_LIMIT_BURST must be > 0",
		},
		{
			// perMin=0 disables the per-user limiter; burst is irrelevant and not validated.
			name:    "perMin=0 disabled passes regardless of burst",
			perMin:  "0",
			burst:   "0",
			wantErr: false,
		},
		{
			name:    "valid perMin>0 and burst>0 passes",
			perMin:  "120",
			burst:   "20",
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_USER_RATE_LIMIT_PER_MIN", tc.perMin)
			t.Setenv("PAYMENT_USER_RATE_LIMIT_BURST", tc.burst)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}

// TestLoad_Redis_Auth verifies validateRedis (sec MAJOR-2): non-dev requires auth + TLS.
func TestLoad_Redis_Auth(t *testing.T) {
	tests := []struct {
		name      string
		env       string
		redisURL  string
		wantErr   bool
		errSubstr string
	}{
		{
			name:     "dev: empty redis allowed (noop publisher)",
			env:      envDevelopment,
			redisURL: "",
			wantErr:  false,
		},
		{
			name:     "dev: unauthenticated redis allowed in dev",
			env:      envDevelopment,
			redisURL: "redis://localhost:6379",
			wantErr:  false,
		},
		{
			name:      "production: unauthenticated redis rejected",
			env:       "production",
			redisURL:  "redis://localhost:6379",
			wantErr:   true,
			errSubstr: "PAYMENT_REDIS_URL must include authentication",
		},
		{
			name:      "production: redis:// (no TLS) rejected even with auth",
			env:       "production",
			redisURL:  "redis://:password@localhost:6379",
			wantErr:   true,
			errSubstr: "PAYMENT_REDIS_URL must use rediss://",
		},
		{
			name:     "production: rediss:// with auth passes",
			env:      "production",
			redisURL: "rediss://:password@localhost:6379",
			wantErr:  false,
		},
		{
			name:      "invalid URL rejected in any env",
			env:       envDevelopment,
			redisURL:  "not-a-url://[invalid",
			wantErr:   true,
			errSubstr: "PAYMENT_REDIS_URL is not a valid Redis URL",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env == envDevelopment {
				setValidEnv(t)
			} else {
				setValidNonDevEnv(t)
			}

			t.Setenv("PAYMENT_REDIS_URL", tc.redisURL)

			_, err := config.Load()
			if tc.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.errSubstr)
			} else {
				require.NoError(t, err)
			}
		})
	}
}
