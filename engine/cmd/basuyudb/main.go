// Command basuyudb is the BasuyuDB engine entry point. It bootstraps the
// managed-mode BadgerDB storage layer and the PG wire v3 server, then serves
// until SIGINT/SIGTERM, at which point it closes the store gracefully so the
// BadgerDB LOCK file is released (G-ADR-26).
//
// Milestone-1 (Mode D Sprint Cluster 1+2): native engine path to Gate 1
// (`psql` connects, `SELECT 1`). No PostgreSQL proxy, no pgx. The downstream
// PostgreSQL dependency (BASUYUDB_PG_DSN) is removed entirely.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"

	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/otel"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
	"github.com/cloudtasticdev/basuyudb/engine/internal/transactions"
	"github.com/cloudtasticdev/basuyudb/engine/internal/version"
	"github.com/cloudtasticdev/basuyudb/engine/internal/wire"
)

func main() {
	logger := newLogger()
	slog.SetDefault(logger)
	runtime.GOMAXPROCS(runtime.NumCPU())

	devMode := envBool("BASUYUDB_DEV_MODE", false)

	logger.Info("BasuyuDB starting",
		"version", version.Number,
		"go_version", runtime.Version(),
		"num_cpu", runtime.NumCPU(),
		"dev_mode", devMode,
	)
	if devMode {
		logger.Warn("BASUYUDB_DEV_MODE=true — trust authentication enabled; do not use in production")
	}

	// --- managed-mode storage (by design) ---
	dataDir := envStr("BASUYUDB_DATA_DIR", "/data/badger")
	store, err := storage.Open(storage.Options{
		DataDir: dataDir,
		EncryptionKey: []byte(os.Getenv("BASUYUDB_ENCRYPTION_KEY")),
		Logger: logger,
	})
	if err != nil {
		logger.Error("failed to open managed storage", "err", err, "data_dir", dataDir)
		os.Exit(1)
	}

	// Single-node transaction engine (managed-mode snapshots; the LocalCommitter
	// is swapped for the Raft Propose path at Gate 4).
	nodeID := uint64(1)
	txnEngine := transactions.New(store, nodeID, nil)
	exec := executor.New(store, txnEngine)

	// --- PG wire v3 server (ADR-001; port 5432) ---
	wireAddr := envStr("BASUYUDB_PG_ADDR", ":5432")
	srv := wire.NewServer(wire.Config{
		Addr: wireAddr,
		Executor: exec,
		DevMode: devMode,
		Logger: logger,
	})
	if err := srv.Listen(); err != nil {
		logger.Error("failed to bind PG wire listener", "err", err, "addr", wireAddr)
		_ = store.Close()
		os.Exit(1)
	}

	// --- signal-driven graceful shutdown (G-ADR-26: release BadgerDB LOCK) ---
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ctx) }()

	// OTLP gRPC ingestion (ADR-007; port 4317).
	otlpAddr := envStr("BASUYUDB_OTLP_GRPC_ADDR", ":4317")
	if ing, ok := exec.(executor.SpanIngester); ok {
		rcv := otel.NewReceiver(otel.Config{Ingester: ing, DevMode: devMode, Logger: logger})
		go func() {
			if err := rcv.Serve(ctx, otlpAddr); err != nil {
				logger.Warn("OTLP receiver stopped", "err", err)
			}
		}()
	}

	logger.Info("BasuyuDB ready", "pg_addr", srv.Addr(), "otlp_addr", otlpAddr, "data_dir", dataDir)

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received; draining")
	case err := <-serveErr:
		if err != nil {
			logger.Error("wire server error", "err", err)
		}
	}

	_ = srv.Close()
	if err := store.Close(); err != nil {
		logger.Error("storage close error", "err", err)
		os.Exit(1)
	}
	logger.Info("BasuyuDB stopped cleanly")
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	switch os.Getenv("BASUYUDB_LOG_LEVEL") {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	return slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
}

func envStr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envBool(key string, def bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return def
	}
	return b
}
