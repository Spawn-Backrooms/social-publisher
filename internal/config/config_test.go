package config

import (
	"encoding/base64"
	"strings"
	"testing"
)

func env(t *testing.T) {
	t.Helper()
	t.Setenv("DATABASE_URL", "postgres://test:test@database.example.com/test?sslmode=verify-full")
	t.Setenv("DEV_MODE", "false")
	t.Setenv("PUBLIC_BASE_URL", "https://publisher.example.com")
	t.Setenv("JWT_HMAC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	t.Setenv("TOKEN_ENCRYPTION_KEYS", `{"primary":"`+base64.StdEncoding.EncodeToString(make([]byte, 32))+`"}`)
	t.Setenv("ACTIVE_TOKEN_KEY_ID", "primary")
	t.Setenv("JWT_ISSUER", "application")
	t.Setenv("JWT_AUDIENCE", "social-publisher")
	t.Setenv("OPERATIONS_TOKEN", base64.StdEncoding.EncodeToString(make([]byte, 32)))
}

func TestProductionRequiresStrongOperationsToken(t *testing.T) {
	for _, value := range []string{"", "short", base64.StdEncoding.EncodeToString(make([]byte, 16))} {
		t.Run(value, func(t *testing.T) {
			env(t)
			t.Setenv("OPERATIONS_TOKEN", value)
			if _, err := Load(); err == nil {
				t.Fatal("production accepted missing or weak operations authentication")
			}
		})
	}
}

func TestAdmissionAndRetentionSettingsAreBounded(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"API_REQUESTS_PER_MINUTE", "0"}, {"API_REQUESTS_PER_MINUTE", "10001"},
		{"API_OWNER_CONCURRENCY", "0"}, {"API_OWNER_CONCURRENCY", "65"},
		{"API_ACCOUNT_CONCURRENCY", "0"}, {"API_ACCOUNT_CONCURRENCY", "9"},
		{"PENDING_TARGETS_PER_OWNER", "0"}, {"PENDING_TARGETS_PER_OWNER", "100001"},
		{"EVENT_RETENTION_DAYS", "6"}, {"EVENT_RETENTION_DAYS", "3651"},
		{"RETENTION_BATCH_SIZE", "0"}, {"RETENTION_BATCH_SIZE", "2001"},
	} {
		t.Run(tc.key+"="+tc.value, func(t *testing.T) {
			env(t)
			t.Setenv(tc.key, tc.value)
			if _, err := Load(); err == nil {
				t.Fatal("unbounded admission or retention setting accepted")
			}
		})
	}
}
func TestConfigRejectsInsecureProductionCallback(t *testing.T) {
	env(t)
	t.Setenv("PUBLIC_BASE_URL", "http://publisher.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("HTTP production callback accepted")
	}
}
func TestConfigRejectsPartialProviderCredentials(t *testing.T) {
	env(t)
	t.Setenv("X_CLIENT_ID", "test-id")
	if _, err := Load(); err == nil {
		t.Fatal("incomplete credentials accepted")
	}
}
func TestConfigRejectsOutOfRangeConcurrency(t *testing.T) {
	env(t)
	t.Setenv("WORKER_CONCURRENCY", "9999")
	if _, err := Load(); err == nil {
		t.Fatal("unbounded worker count accepted")
	}
}
func TestConfigBuildsExactCallbacks(t *testing.T) {
	env(t)
	t.Setenv("PUBLIC_BASE_URL", "https://publisher.example.com/")
	c, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if c.X.RedirectURI != "https://publisher.example.com/oauth/x/callback" || c.TikTok.RedirectURI != "https://publisher.example.com/oauth/tiktok/callback" || c.Instagram.RedirectURI != "https://publisher.example.com/oauth/instagram/callback" {
		t.Fatal("callbacks not exact")
	}
}
func TestDevelopmentModeOnlyPermitsLocalHTTP(t *testing.T) {
	env(t)
	t.Setenv("DEV_MODE", "true")
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PUBLIC_BASE_URL", "http://internet.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("development mode allowed insecure public origin")
	}
}

func TestProductionDatabaseRequiresVerifiedTLS(t *testing.T) {
	for _, mode := range []string{"disable", "allow", "prefer", "require", "verify-ca", ""} {
		t.Run(mode, func(t *testing.T) {
			env(t)
			t.Setenv("DATABASE_URL", "postgres://test:never-log-this@database.example.com/test?sslmode="+mode)
			_, err := Load()
			if err == nil {
				t.Fatal("production accepted an unverified or plaintext-fallback database connection")
			}
			if strings.Contains(err.Error(), "never-log-this") {
				t.Fatal("database configuration error exposed its password")
			}
		})
	}
}

func TestDevelopmentDatabaseSupportsLocalCompose(t *testing.T) {
	env(t)
	t.Setenv("DEV_MODE", "true")
	t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	t.Setenv("DATABASE_URL", "postgres://test:test@127.0.0.1/test?sslmode=disable")
	if _, err := Load(); err != nil {
		t.Fatalf("loopback development database rejected: %v", err)
	}
	t.Setenv("DATABASE_URL", "postgres://test:test@postgres/test?sslmode=disable")
	if _, err := Load(); err != nil {
		t.Fatalf("local Compose development database rejected: %v", err)
	}
}

func TestDevelopmentModeCannotDisableSecureCookiesOnPublicOrigin(t *testing.T) {
	env(t)
	t.Setenv("DEV_MODE", "true")
	if _, err := Load(); err == nil {
		t.Fatal("development mode accepted a public origin with insecure OAuth cookies")
	}
}

func TestLifecycleGrantRequiresDistinctKeyAndCompleteCentralCustody(t *testing.T) {
	env(t)
	for _, name := range []string{"LIFECYCLE_JWT_HMAC_KEY", "LIFECYCLE_ACCOUNT_ID", "LIFECYCLE_PROVIDER_USER_ID", "LIFECYCLE_ACCOUNT_TENANT", "LIFECYCLE_ACCOUNT_OWNER", "LIFECYCLE_PUBLIC_DELIVERY_ORIGIN"} {
		t.Setenv(name, "")
	}
	c, err := Load()
	if err != nil || len(c.LifecycleJWTKey) != 0 {
		t.Fatal("default-off config changed")
	}
	t.Setenv("LIFECYCLE_ACCOUNT_ID", "acct-central")
	if _, err = Load(); err == nil {
		t.Fatal("partial central grant accepted")
	}
	t.Setenv("LIFECYCLE_PROVIDER_USER_ID", "42")
	t.Setenv("LIFECYCLE_ACCOUNT_TENANT", "central-project")
	t.Setenv("LIFECYCLE_ACCOUNT_OWNER", "central-owner")
	t.Setenv("LIFECYCLE_JWT_HMAC_KEY", base64.StdEncoding.EncodeToString(make([]byte, 32)))
	if _, err = Load(); err == nil {
		t.Fatal("customer JWT signing key reused by service")
	}
	t.Setenv("LIFECYCLE_JWT_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	c, err = Load()
	if err != nil || c.LifecycleAccountID != "acct-central" {
		t.Fatal("complete distinct central grant rejected", err)
	}
}

func TestLifecyclePublicOriginIsOptionalPinnedAndRequiresCompleteGrant(t *testing.T) {
	env(t)
	for _, name := range []string{"LIFECYCLE_JWT_HMAC_KEY", "LIFECYCLE_ACCOUNT_ID", "LIFECYCLE_PROVIDER_USER_ID", "LIFECYCLE_ACCOUNT_TENANT", "LIFECYCLE_ACCOUNT_OWNER", "LIFECYCLE_PUBLIC_DELIVERY_ORIGIN"} {
		t.Setenv(name, "")
	}
	t.Setenv("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN", "https://spawn.example.com")
	if _, err := Load(); err == nil {
		t.Fatal("public origin alone enabled an incomplete lifecycle grant")
	}
	t.Setenv("LIFECYCLE_JWT_HMAC_KEY", base64.StdEncoding.EncodeToString([]byte(strings.Repeat("s", 32))))
	t.Setenv("LIFECYCLE_ACCOUNT_ID", "acct-central")
	t.Setenv("LIFECYCLE_PROVIDER_USER_ID", "42")
	t.Setenv("LIFECYCLE_ACCOUNT_TENANT", "central-project")
	t.Setenv("LIFECYCLE_ACCOUNT_OWNER", "central-owner")
	for _, origin := range []string{"", "https://spawn.example.com"} {
		t.Setenv("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN", origin)
		c, err := Load()
		if err != nil || c.LifecyclePublicDeliveryOrigin != origin {
			t.Fatal("optional external origin changed or rejected native grant", origin, err)
		}
	}
	for _, origin := range []string{"http://spawn.example.com", "https://SPAWN.example.com", "https://spawn.example.com/", "https://spawn.example.com?", "https://spawn.example.com#", "https://user@spawn.example.com", "https://spawn.example.com:443"} {
		t.Setenv("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN", origin)
		if _, err := Load(); err == nil {
			t.Fatal("noncanonical external origin accepted", origin)
		}
	}
}
