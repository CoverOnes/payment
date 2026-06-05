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
			env:     "development",
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
			env:     "development",
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
			env:       "development",
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
			env:     "development",
			secret:  testHMACSecret,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", tc.secret)
			// Non-dev requires S2S token + workspace config; set them so HMAC is the only variable.
			if tc.env == "production" || tc.env == "staging" {
				t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
				t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "http://workspace:8081")
				t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
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
		{"development", true},
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
			// and workspace config (PAYMENT_WORKSPACE_BASE_URL + PAYMENT_WORKSPACE_S2S_TOKEN).
			t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
			t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
			t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "http://workspace:8081")
			t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)

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
			env:     "development",
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
			env:       "development",
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
			env:     "development",
			token:   testS2SToken,
			wantErr: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			t.Setenv("PAYMENT_ENV", tc.env)
			// Provide the HMAC secret + workspace config so they don't interfere with S2S token tests.
			if tc.env != "development" {
				t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
				t.Setenv("PAYMENT_WORKSPACE_BASE_URL", "http://workspace:8081")
				t.Setenv("PAYMENT_WORKSPACE_S2S_TOKEN", testWorkspaceToken)
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
			env:            "development",
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
			baseURL:        "http://workspace:8081",
			workspaceToken: "",
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_S2S_TOKEN must be at least 32 characters",
		},
		{
			name:           "production: too-short workspace token fails",
			env:            "production",
			baseURL:        "http://workspace:8081",
			workspaceToken: "tooshort",
			wantErr:        true,
			errSubstr:      "PAYMENT_WORKSPACE_S2S_TOKEN must be at least 32 characters",
		},
		{
			name:           "production: valid workspace config passes",
			env:            "production",
			baseURL:        "http://workspace:8081",
			workspaceToken: testWorkspaceToken,
			wantErr:        false,
		},
		{
			name:           "dev: too-short workspace token fails even in dev",
			env:            "development",
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

			if tc.env != "development" {
				t.Setenv("PAYMENT_GATEWAY_HMAC_SECRET", testHMACSecret)
				t.Setenv("PAYMENT_SETTLEMENT_S2S_TOKEN", testS2SToken)
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
