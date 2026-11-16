package config

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

type Config struct {
	StorageBackend         string
	NativeStorageURL       string
	NativeStorageSession   string
	DatabaseURL            string
	PublicBaseURL          string
	ListenAddress          string
	DevMode                bool
	LifecycleJWTKey        []byte
	LifecycleAccountID     string
	LifecycleProviderID    string
	LifecycleTenant        string
	LifecycleOwner         string
	JWTKey                 []byte
	JWTIssuer              string
	JWTAudience            string
	EncryptionKeys         map[string][]byte
	ActiveKey              string
	MediaHosts             []string
	MaxMediaBytes          int64
	WorkerConcurrency      int
	DBMaxConns             int32
	JobTimeout             time.Duration
	OperationsToken        string
	APIRequestsPerMinute   int
	APIOwnerConcurrency    int
	APIAccountConcurrency  int
	PendingTargetsPerOwner int
	EventRetention         time.Duration
	RetentionBatchSize     int
	TikTok                 provider.Config
	X                      provider.Config
	Instagram              provider.Config

	// Empty keeps external-work lifecycle publication disabled.
	LifecyclePublicDeliveryOrigin string
}

func Load() (Config, error) {
	c := Config{StorageBackend: os.Getenv("STORAGE_BACKEND"), NativeStorageURL: os.Getenv("NATIVE_STORAGE_URL"), NativeStorageSession: os.Getenv("NATIVE_STORAGE_SESSION"), DatabaseURL: os.Getenv("DATABASE_URL"), PublicBaseURL: strings.TrimRight(os.Getenv("PUBLIC_BASE_URL"), "/"), JWTIssuer: os.Getenv("JWT_ISSUER"), JWTAudience: os.Getenv("JWT_AUDIENCE"), ActiveKey: os.Getenv("ACTIVE_TOKEN_KEY_ID")}
	var err error
	if c.StorageBackend == "" {
		c.StorageBackend = "postgres"
	}
	if c.StorageBackend != "postgres" && c.StorageBackend != "cloudflare-do" {
		return c, errors.New("STORAGE_BACKEND must be postgres or cloudflare-do")
	}
	if c.StorageBackend == "postgres" && c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	if c.StorageBackend == "cloudflare-do" {
		if c.DatabaseURL != "" {
			return c, errors.New("cloudflare-do storage must not configure DATABASE_URL")
		}
		if err := store.ValidateNativeStorage(c.NativeStorageURL, c.NativeStorageSession); err != nil {
			return c, err
		}
	}
	c.DevMode, err = boolean("DEV_MODE", false)
	if err != nil {
		return c, err
	}
	u, err := url.Parse(c.PublicBaseURL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.Path != "" || u.Opaque != "" {
		return c, errors.New("PUBLIC_BASE_URL must be an absolute origin without a path, credentials, query, or fragment")
	}
	local := u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1" || u.Hostname() == "[::1]" || u.Hostname() == "::1"
	if c.DevMode && !local {
		return c, errors.New("DEV_MODE requires a loopback PUBLIC_BASE_URL; use production mode for public origins")
	}
	if u.Scheme != "https" && !(c.DevMode && local && u.Scheme == "http") {
		return c, errors.New("PUBLIC_BASE_URL requires HTTPS; DEV_MODE allows HTTP only on loopback")
	}
	if len(c.PublicBaseURL) > 400 {
		return c, errors.New("PUBLIC_BASE_URL is too long")
	}
	if c.StorageBackend == "postgres" {
		if err := ValidateDatabaseURL(c.DatabaseURL, c.DevMode); err != nil {
			return c, err
		}
	}
	c.JWTKey, err = decodeKey(os.Getenv("JWT_HMAC_KEY"))
	if err != nil || len(c.JWTKey) < 32 {
		return c, errors.New("JWT_HMAC_KEY must be base64 containing at least 32 random bytes")
	}
	if c.JWTIssuer == "" || c.JWTAudience == "" {
		return c, errors.New("JWT_ISSUER and JWT_AUDIENCE are required")
	}
	if os.Getenv("LIFECYCLE_JWT_HMAC_KEY") != "" {
		c.LifecycleJWTKey, err = decodeKey(os.Getenv("LIFECYCLE_JWT_HMAC_KEY"))
		c.LifecycleAccountID = os.Getenv("LIFECYCLE_ACCOUNT_ID")
		c.LifecycleProviderID = os.Getenv("LIFECYCLE_PROVIDER_USER_ID")
		c.LifecycleTenant = os.Getenv("LIFECYCLE_ACCOUNT_TENANT")
		c.LifecycleOwner = os.Getenv("LIFECYCLE_ACCOUNT_OWNER")
		c.LifecyclePublicDeliveryOrigin = os.Getenv("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN")
		if err != nil || len(c.LifecycleJWTKey) < 32 || bytes.Equal(c.LifecycleJWTKey, c.JWTKey) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString(c.LifecycleAccountID) || !regexp.MustCompile(`^[0-9]{1,30}$`).MatchString(c.LifecycleProviderID) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`).MatchString(c.LifecycleTenant) || !regexp.MustCompile(`^[A-Za-z0-9_-]{1,160}$`).MatchString(c.LifecycleOwner) {
			return c, errors.New("LIFECYCLE requires a distinct service key and exact central account custody grant")
		}
		if c.LifecyclePublicDeliveryOrigin != "" && !security.ValidPublicDeliveryOrigin(c.LifecyclePublicDeliveryOrigin) {
			return c, errors.New("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN must be an exact canonical HTTPS DNS origin without a port, path, credentials, query, or fragment")
		}
	} else if os.Getenv("LIFECYCLE_ACCOUNT_ID") != "" || os.Getenv("LIFECYCLE_PROVIDER_USER_ID") != "" || os.Getenv("LIFECYCLE_ACCOUNT_TENANT") != "" || os.Getenv("LIFECYCLE_ACCOUNT_OWNER") != "" || os.Getenv("LIFECYCLE_PUBLIC_DELIVERY_ORIGIN") != "" {
		return c, errors.New("LIFECYCLE grant is incomplete")
	}
	c.OperationsToken = os.Getenv("OPERATIONS_TOKEN")
	if c.OperationsToken != "" || !c.DevMode {
		key, keyErr := decodeKey(c.OperationsToken)
		if keyErr != nil || len(key) < 32 {
			return c, errors.New("OPERATIONS_TOKEN must be base64 containing at least 32 random bytes; it is required in production")
		}
	}
	for _, setting := range []struct {
		name                       string
		fallback, minimum, maximum int64
		target                     *int
	}{
		{"API_REQUESTS_PER_MINUTE", 60, 1, 10000, &c.APIRequestsPerMinute},
		{"API_OWNER_CONCURRENCY", 4, 1, 64, &c.APIOwnerConcurrency},
		{"API_ACCOUNT_CONCURRENCY", 1, 1, 8, &c.APIAccountConcurrency},
		{"PENDING_TARGETS_PER_OWNER", 1000, 1, 100000, &c.PendingTargetsPerOwner},
		{"RETENTION_BATCH_SIZE", 500, 1, 2000, &c.RetentionBatchSize},
	} {
		value, valueErr := integer(setting.name, setting.fallback, setting.minimum, setting.maximum)
		if valueErr != nil {
			return c, valueErr
		}
		*setting.target = int(value)
	}
	retentionDays, err := integer("EVENT_RETENTION_DAYS", 90, 7, 3650)
	if err != nil {
		return c, err
	}
	c.EventRetention = time.Duration(retentionDays) * 24 * time.Hour
	var encoded map[string]string
	if json.Unmarshal([]byte(os.Getenv("TOKEN_ENCRYPTION_KEYS")), &encoded) != nil || len(encoded) == 0 || c.ActiveKey == "" {
		return c, errors.New("TOKEN_ENCRYPTION_KEYS must be a JSON keyring and ACTIVE_TOKEN_KEY_ID must select a key")
	}
	c.EncryptionKeys = make(map[string][]byte, len(encoded))
	for name, value := range encoded {
		key, err := decodeKey(value)
		if err != nil || len(key) != 32 {
			return c, errors.New("every token encryption key must be exactly 32 base64-encoded random bytes")
		}
		c.EncryptionKeys[name] = key
	}
	if _, ok := c.EncryptionKeys[c.ActiveKey]; !ok {
		return c, errors.New("ACTIVE_TOKEN_KEY_ID does not exist in TOKEN_ENCRYPTION_KEYS")
	}
	if v := os.Getenv("MEDIA_ALLOWED_HOSTS"); v != "" {
		for _, host := range strings.Split(v, ",") {
			c.MediaHosts = append(c.MediaHosts, strings.TrimSpace(host))
		}
	}
	maxBytes, err := integer("MAX_MEDIA_BYTES", 1<<30, 1, 16<<30)
	if err != nil {
		return c, err
	}
	c.MaxMediaBytes = maxBytes
	concurrency, err := integer("WORKER_CONCURRENCY", 6, 1, 32)
	if err != nil {
		return c, err
	}
	c.WorkerConcurrency = int(concurrency)
	conns, err := integer("DB_MAX_CONNS", 32, 4, 128)
	if err != nil {
		return c, err
	}
	c.DBMaxConns = int32(conns)
	timeout, err := integer("JOB_TIMEOUT_SECONDS", 600, 30, 1800)
	if err != nil {
		return c, err
	}
	c.JobTimeout = time.Duration(timeout) * time.Second
	port, err := integer("PORT", 8080, 1, 65535)
	if err != nil {
		return c, err
	}
	c.ListenAddress = fmt.Sprintf(":%d", port)
	c.TikTok = provider.Config{ClientID: os.Getenv("TIKTOK_CLIENT_KEY"), ClientSecret: os.Getenv("TIKTOK_CLIENT_SECRET"), RedirectURI: c.PublicBaseURL + "/oauth/tiktok/callback"}
	c.TikTok.Audited, err = boolean("TIKTOK_AUDITED", false)
	if err != nil {
		return c, err
	}
	c.X = provider.Config{ClientID: os.Getenv("X_CLIENT_ID"), ClientSecret: os.Getenv("X_CLIENT_SECRET"), RedirectURI: c.PublicBaseURL + "/oauth/x/callback"}
	version := os.Getenv("INSTAGRAM_GRAPH_VERSION")
	if version == "" {
		version = "v26.0"
	}
	if !regexp.MustCompile(`^v[1-9][0-9]*\.[0-9]+$`).MatchString(version) {
		return c, errors.New("INSTAGRAM_GRAPH_VERSION must have the form v26.0")
	}
	c.Instagram = provider.Config{ClientID: os.Getenv("INSTAGRAM_CLIENT_ID"), ClientSecret: os.Getenv("INSTAGRAM_CLIENT_SECRET"), RedirectURI: c.PublicBaseURL + "/oauth/instagram/callback", GraphVersion: version}
	for name, p := range map[string]provider.Config{"TikTok": c.TikTok, "X": c.X, "Instagram": c.Instagram} {
		if (p.ClientID == "") != (p.ClientSecret == "") {
			return c, fmt.Errorf("%s client credentials must both be configured", name)
		}
	}
	return c, nil
}

// ValidateDatabaseURL validates the effective pgx transport configuration,
// including fallback hosts and PGSSL environment settings. Production traffic
// requires server identity verification, with no plaintext fallback. Explicit
// local development mode permits the plaintext database in Docker Compose.
// Errors intentionally omit parser details because a DSN contains credentials.
func ValidateDatabaseURL(dsn string, devMode bool) error {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return errors.New("DATABASE_URL is invalid")
	}
	if devMode {
		return nil
	}
	if cfg.TLSConfig == nil || cfg.TLSConfig.InsecureSkipVerify || cfg.TLSConfig.ServerName == "" {
		return errors.New("production DATABASE_URL requires verified TLS; configure sslmode=verify-full")
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.TLSConfig == nil || fallback.TLSConfig.InsecureSkipVerify || fallback.TLSConfig.ServerName == "" {
			return errors.New("production DATABASE_URL cannot include an unverified or plaintext fallback")
		}
	}
	return nil
}
func decodeKey(v string) ([]byte, error) { return base64.StdEncoding.Strict().DecodeString(v) }
func integer(name string, def, min, max int64) (int64, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n < min || n > max {
		return 0, fmt.Errorf("%s must be between %d and %d", name, min, max)
	}
	return n, nil
}
func boolean(name string, def bool) (bool, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	if v != "true" && v != "false" {
		return false, fmt.Errorf("%s must be true or false", name)
	}
	return v == "true", nil
}
