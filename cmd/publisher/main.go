package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
	_ "time/tzdata"

	"socialpublisher/internal/account"
	"socialpublisher/internal/api"
	"socialpublisher/internal/buildinfo"
	"socialpublisher/internal/config"
	"socialpublisher/internal/model"
	"socialpublisher/internal/ops"
	"socialpublisher/internal/provider"
	"socialpublisher/internal/provider/instagram"
	"socialpublisher/internal/provider/tiktok"
	xprovider "socialpublisher/internal/provider/x"
	"socialpublisher/internal/security"
	"socialpublisher/internal/store"
	"socialpublisher/internal/worker"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:], logger); err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
}
func run(ctx context.Context, args []string, logger *slog.Logger) error {
	mode := "serve"
	if len(args) > 0 {
		mode = args[0]
		args = args[1:]
	}
	if mode == "keygen" {
		return keygen()
	}
	if mode == "token" {
		return mint(args)
	}
	if mode == "version" {
		return json.NewEncoder(os.Stdout).Encode(map[string]string{"build_revision": buildinfo.Revision})
	}
	if mode == "serve" || mode == "api" || mode == "worker" || mode == "migrate" {
		if err := buildinfo.ValidateExpected(os.Getenv("EXPECTED_BUILD_REVISION")); err != nil {
			return err
		}
	}
	if mode == "migrate" {
		if os.Getenv("STORAGE_BACKEND") == "cloudflare-do" {
			return errors.New("native migrations require the authorized controller migrate-storage action while stopped in maintenance")
		}
		if backend := os.Getenv("STORAGE_BACKEND"); backend != "" && backend != "postgres" {
			return errors.New("STORAGE_BACKEND must be postgres or cloudflare-do")
		}
		dsn := os.Getenv("DATABASE_URL")
		if dsn == "" {
			return errors.New("DATABASE_URL is required")
		}
		if err := config.ValidateDatabaseURL(dsn, os.Getenv("DEV_MODE") == "true"); err != nil {
			return err
		}
		if err := store.Migrate(ctx, dsn); err != nil {
			return errors.New("database migration failed; inspect database access and migration permissions")
		}
		logger.Info("database_migrated")
		return nil
	}
	if mode != "serve" && mode != "api" && mode != "worker" {
		return errors.New("usage: publisher [serve|api|worker|migrate|keygen|token|version]")
	}
	c, err := config.Load()
	if err != nil {
		return err
	}
	vault, err := security.NewVault(c.EncryptionKeys, c.ActiveKey)
	if err != nil {
		return err
	}
	jwt, err := security.NewJWT(c.JWTKey, c.JWTIssuer, c.JWTAudience)
	if err != nil {
		return err
	}
	media, err := security.NewMediaFetcher(c.MediaHosts, c.MaxMediaBytes)
	if err != nil {
		return err
	}
	database, err := openStore(ctx, c)
	if err != nil {
		return errors.New("storage connection failed; inspect selected storage configuration and private bridge availability")
	}
	defer database.Close()
	readyCtx, readyCancel := context.WithTimeout(ctx, 10*time.Second)
	err = database.Ping(readyCtx)
	readyCancel()
	if err != nil {
		return errors.New("database schema is not ready; apply the matching publisher migrations before startup")
	}
	client := &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("provider redirects are disabled") }}
	registry := provider.Registry{}
	if c.TikTok.ClientID != "" {
		c.TikTok.HTTPClient = client
		c.TikTok.Media = media
		registry[model.TikTok] = tiktok.New(c.TikTok)
	}
	if c.X.ClientID != "" {
		c.X.HTTPClient = client
		c.X.Media = media
		registry[model.X] = xprovider.New(c.X)
	}
	if c.Instagram.ClientID != "" {
		c.Instagram.HTTPClient = client
		c.Instagram.Media = media
		registry[model.Instagram] = instagram.New(c.Instagram)
	}
	tokens := &account.Manager{Store: database, Vault: vault, Adapters: registry, Logger: logger}
	engine := &worker.Engine{Store: database, Tokens: tokens, Adapters: registry, Media: media, Concurrency: c.WorkerConcurrency, JobTimeout: c.JobTimeout, Logger: logger}
	operations, err := ops.New(database, ops.Config{Role: mode, BuildRevision: buildinfo.Revision,
		WorkerStaleAfter: c.JobTimeout + 3*time.Minute, EventRetention: c.EventRetention,
		RetentionBatchSize: c.RetentionBatchSize, Logger: logger})
	if err != nil {
		return errors.New("operations configuration is invalid")
	}
	engine.OnProgress = operations.WorkerProgress
	if err := operations.Heartbeat(ctx); err != nil {
		return errors.New("initial process heartbeat failed; check database access")
	}
	runctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); operations.Run(runctx) }()
	if mode == "serve" || mode == "worker" {
		wg.Add(2)
		go func() { defer wg.Done(); engine.Run(runctx) }()
		go func() { defer wg.Done(); tokens.RunMaintenance(runctx, time.Minute) }()
	}
	if mode == "worker" {
		logger.Info("worker_started", "concurrency", c.WorkerConcurrency, "configured_platforms", len(registry))
		<-ctx.Done()
		cancel()
		wg.Wait()
		return nil
	}
	service := &api.Server{Store: database, Adapters: registry, JWT: jwt, Vault: vault, Tokens: tokens, Media: media,
		PublicBaseURL: c.PublicBaseURL, SecureCookies: !c.DevMode, Logger: logger,
		Operations: operations, OperationsToken: c.OperationsToken,
		AdmissionLimits: store.AdmissionLimits{RequestsPerMinute: c.APIRequestsPerMinute,
			OwnerConcurrency: c.APIOwnerConcurrency, AccountConcurrency: c.APIAccountConcurrency}}
	if len(c.LifecycleJWTKey) > 0 {
		serviceJWT, err := security.NewJWT(c.LifecycleJWTKey, "spawn-lifecycle-service", "spawn-lifecycle-publisher")
		if err != nil {
			return err
		}
		service.Lifecycle = &api.LifecycleGrant{JWT: serviceJWT, Owner: model.Principal{TenantID: c.LifecycleTenant, Subject: c.LifecycleOwner}, AccountID: c.LifecycleAccountID, ProviderUserID: c.LifecycleProviderID, PublicDeliveryOrigin: c.LifecyclePublicDeliveryOrigin}
	}
	srv := &http.Server{Addr: c.ListenAddress, Handler: service.Handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 120 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 << 10, BaseContext: func(net.Listener) context.Context { return runctx }}
	failure := make(chan error, 1)
	go func() { failure <- srv.ListenAndServe() }()
	logger.Info("api_started", "address", c.ListenAddress, "mode", mode, "configured_platforms", len(registry))
	var serveErr error
	select {
	case <-ctx.Done():
	case err := <-failure:
		if !errors.Is(err, http.ErrServerClosed) {
			serveErr = errors.New("HTTP listener failed")
		}
	}
	cancel()
	shutdown, stop := context.WithTimeout(context.Background(), 15*time.Second)
	defer stop()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	wg.Wait()
	return serveErr
}

type publisherStore interface {
	store.Store
	ops.Backend
}

func openStore(ctx context.Context, c config.Config) (publisherStore, error) {
	if c.StorageBackend == "cloudflare-do" {
		return store.NewNative(c.NativeStorageURL, c.NativeStorageSession)
	}
	database, err := store.New(ctx, c.DatabaseURL, c.DBMaxConns)
	if err != nil {
		return nil, err
	}
	database.PendingTargetsPerOwner = c.PendingTargetsPerOwner
	return database, nil
}
func keygen() error {
	a, b, opsKey, controlKey := make([]byte, 32), make([]byte, 32), make([]byte, 32), make([]byte, 32)
	for _, key := range [][]byte{a, b, opsKey, controlKey} {
		if _, err := rand.Read(key); err != nil {
			return errors.New("secure random generation failed")
		}
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{"JWT_HMAC_KEY": base64.StdEncoding.EncodeToString(a),
		"TOKEN_ENCRYPTION_KEYS": map[string]string{"primary": base64.StdEncoding.EncodeToString(b)}, "ACTIVE_TOKEN_KEY_ID": "primary",
		"OPERATIONS_TOKEN": base64.StdEncoding.EncodeToString(opsKey), "CONTROL_TOKEN": base64.StdEncoding.EncodeToString(controlKey)})
}
func mint(args []string) error {
	flags := flag.NewFlagSet("token", flag.ContinueOnError)
	tenant := flags.String("tenant", "", "tenant ID")
	user := flags.String("user", "", "subject/user ID")
	ttl := flags.Duration("ttl", time.Hour, "validity, at most 24h")
	if err := flags.Parse(args); err != nil {
		return errors.New("invalid token command arguments")
	}
	secret, err := base64.StdEncoding.DecodeString(os.Getenv("JWT_HMAC_KEY"))
	if err != nil {
		return errors.New("JWT_HMAC_KEY must contain base64")
	}
	signer, err := security.NewJWT(secret, os.Getenv("JWT_ISSUER"), os.Getenv("JWT_AUDIENCE"))
	if err != nil {
		return err
	}
	token, err := signer.Mint(model.Principal{TenantID: *tenant, Subject: *user}, *ttl, time.Now())
	if err != nil {
		return errors.New("valid tenant, user and token lifetime required")
	}
	fmt.Println(token)
	return nil
}
