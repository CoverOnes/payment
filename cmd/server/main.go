// Command server starts the CoverOnes payment microservice.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/CoverOnes/payment/internal/config"
	"github.com/CoverOnes/payment/internal/consumer"
	"github.com/CoverOnes/payment/internal/events"
	"github.com/CoverOnes/payment/internal/handler"
	"github.com/CoverOnes/payment/internal/platform/logger"
	"github.com/CoverOnes/payment/internal/service"
	"github.com/CoverOnes/payment/internal/store/postgres"
	"github.com/CoverOnes/payment/internal/workspaceclient"
	"github.com/redis/go-redis/v9"
)

func main() {
	healthcheck := flag.Bool("healthcheck", false, "perform a liveness check against /healthz and exit 0/1")
	flag.Parse()

	// Docker HEALTHCHECK mode: GET /healthz and exit immediately.
	if *healthcheck {
		if err := runHealthCheck(); err != nil {
			slog.Error("healthcheck failed", "err", err)
			os.Exit(1)
		}

		os.Exit(0)
	}

	if err := run(); err != nil {
		slog.Error("server exited with error", "err", err)
		os.Exit(1)
	}
}

// runHealthCheck issues a GET to the local /healthz endpoint.
func runHealthCheck() error {
	port := os.Getenv("PAYMENT_PORT")
	if port == "" {
		port = "8084"
	}

	url := fmt.Sprintf("http://127.0.0.1:%s/healthz", port)

	client := &http.Client{Timeout: 2 * time.Second}

	resp, err := client.Get(url) //nolint:noctx // healthcheck is a one-shot process; no request context needed
	if err != nil {
		return fmt.Errorf("GET %s: %w", url, err)
	}

	defer resp.Body.Close() //nolint:errcheck // best-effort close on healthcheck response

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("unexpected status %d from %s", resp.StatusCode, url)
	}

	return nil
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	// Logger — JSON to stdout (CONVENTIONS §5).
	log := logger.New(cfg.LogLevel)
	slog.SetDefault(log)

	ctx := context.Background()

	// Postgres pool (CONVENTIONS §12).
	// Pass cfg.DBSchema so the pool sets search_path when sharing one Aiven DB across services.
	// Pass cfg.DBMaxConns / cfg.DBMinConns so connection budget is configurable per environment.
	pool, err := postgres.NewPoolWithConfig(ctx, cfg.PostgresDSN, postgres.PoolConfig{
		Schema:   cfg.DBSchema,
		MaxConns: int32(cfg.DBMaxConns),
		MinConns: int32(cfg.DBMinConns),
	})
	if err != nil {
		return fmt.Errorf("connect postgres: %w", err)
	}

	defer pool.Close()

	slog.Info("postgres connected")

	// Redis client (optional — nil means noop publisher + in-process rate limiter).
	var redisClient *redis.Client

	var publisher events.Publisher

	if cfg.RedisURL != "" {
		opts, parseErr := redis.ParseURL(cfg.RedisURL)
		if parseErr != nil {
			return fmt.Errorf("parse redis url: %w", parseErr)
		}

		redisClient = redis.NewClient(opts)

		pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()

		if pingErr := redisClient.Ping(pingCtx).Err(); pingErr != nil {
			slog.Warn("redis ping failed; event publishing and rate limiting will use noop/fallback", "err", pingErr)
			redisClient = nil
		} else {
			slog.Info("redis connected")
		}
	}

	if redisClient != nil {
		publisher = events.NewRedisPublisher(redisClient)
	} else {
		publisher = events.NewNoopPublisher()
	}

	// Store layer.
	txStore := postgres.NewTransactionStore(pool)
	auditStore := postgres.NewAuditStore(pool)
	txManager := postgres.NewTxManager(pool)

	// Settlement store layer.
	settlementPlanStore := postgres.NewSettlementPlanStore(pool)
	settlementAllocStore := postgres.NewSettlementAllocationStore(pool)
	settlementMilestoneDisburseStore := postgres.NewSettlementMilestoneDisbursementStore(pool)
	settlementAuditStore := postgres.NewSettlementAuditStore(pool)
	settlementTxMgr := postgres.NewSettlementTxManager(pool)

	// Workspace roster client (S2S call to workspace to fetch frozen party roster).
	rosterClient := workspaceclient.NewHTTPRosterClient(
		cfg.WorkspaceBaseURL,
		"payment",
		cfg.WorkspaceS2SToken,
	)

	// Service layer.
	txSvc := service.NewTransactionService(txStore, auditStore, txManager, publisher)
	settlementSvc := service.NewSettlementService(
		settlementPlanStore,
		settlementAllocStore,
		settlementMilestoneDisburseStore,
		settlementAuditStore,
		settlementTxMgr,
		rosterClient,
		cfg.PlatformUserUUID(),
	)

	// Start settlement event consumers (workspace.contract_activated + workspace.contract_completed).
	// Only wired when Redis is available — dev without Redis skips consumers gracefully.
	if redisClient != nil {
		settlementConsumer := consumer.NewSettlementConsumer(redisClient, settlementSvc)

		serverCtx, serverCancel := context.WithCancel(context.Background())
		defer serverCancel()

		go settlementConsumer.Start(serverCtx)
		slog.Info("settlement event consumer started")
	} else {
		slog.Warn("redis not connected; settlement event consumers not started")
	}

	// Router.
	r := handler.NewRouter(handler.RouterConfig{
		TransactionSvc:     txSvc,
		SettlementSvc:      settlementSvc,
		Pool:               pool,
		Redis:              redisClient,
		GatewayHMACSecret:  cfg.GatewayHMACSecret,
		SettlementS2SToken: cfg.SettlementS2SToken,
	})

	srv := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      r,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 30 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		slog.Info("server starting", "addr", srv.Addr)

		if listenErr := srv.ListenAndServe(); listenErr != nil && !errors.Is(listenErr, http.ErrServerClosed) {
			slog.Error("server listen error", "err", listenErr)
			os.Exit(1)
		}
	}()

	<-quit
	slog.Info("shutting down gracefully")

	shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if shutdownErr := srv.Shutdown(shutdownCtx); shutdownErr != nil {
		return fmt.Errorf("server shutdown: %w", shutdownErr)
	}

	slog.Info("server stopped")

	return nil
}
