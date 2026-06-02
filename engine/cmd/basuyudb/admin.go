package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/cloudtasticdev/basuyudb/engine/internal/auth"
	"github.com/cloudtasticdev/basuyudb/engine/internal/executor"
	"github.com/cloudtasticdev/basuyudb/engine/internal/session"
	"github.com/cloudtasticdev/basuyudb/engine/internal/storage"
)

// maybeRunSubcommand handles offline admin subcommands (run while the server is
// stopped, since they take the BadgerDB lock):
//
//	basuyudb backup  <file>   stream a consistent backup to <file>
//	basuyudb restore <file>   load a backup into an empty data dir
//
// It calls os.Exit on completion; it returns only when there is no subcommand.
func maybeRunSubcommand(logger *slog.Logger) {
	if len(os.Args) < 2 {
		return
	}
	cmd := os.Args[1]
	if cmd != "backup" && cmd != "restore" {
		return // not an admin subcommand; fall through to the server
	}
	if len(os.Args) < 3 {
		logger.Error("usage: basuyudb " + cmd + " <file>")
		os.Exit(2)
	}
	file := os.Args[2]
	store, err := storage.Open(storage.Options{
		DataDir:       envStr("BASUYUDB_DATA_DIR", "/data/badger"),
		EncryptionKey: []byte(os.Getenv("BASUYUDB_ENCRYPTION_KEY")),
		Logger:        logger,
	})
	if err != nil {
		logger.Error("admin: open store failed", "err", err)
		os.Exit(1)
	}
	defer store.Close()

	switch cmd {
	case "backup":
		f, err := os.Create(file)
		if err != nil {
			logger.Error("admin: create backup file failed", "err", err)
			os.Exit(1)
		}
		ver, err := store.Backup(f)
		_ = f.Close()
		if err != nil {
			logger.Error("admin: backup failed", "err", err)
			os.Exit(1)
		}
		logger.Info("backup complete", "file", file, "version", ver)
	case "restore":
		f, err := os.Open(file)
		if err != nil {
			logger.Error("admin: open backup file failed", "err", err)
			os.Exit(1)
		}
		err = store.Restore(f)
		_ = f.Close()
		if err != nil {
			logger.Error("admin: restore failed", "err", err)
			os.Exit(1)
		}
		logger.Info("restore complete", "file", file)
	}
	os.Exit(0)
}

// startRetentionJob runs a periodic OTel span retention sweep when
// BASUYUDB_OTEL_RETENTION_HOURS > 0, deleting spans older than that window for
// the configured namespace.
func startRetentionJob(ctx context.Context, exec executor.Executor, logger *slog.Logger) {
	hours := envInt("BASUYUDB_OTEL_RETENTION_HOURS", 0)
	if hours <= 0 {
		return
	}
	ns := envStr("BASUYUDB_NAMESPACE", "defaultdb")
	authSess, err := auth.DevSession(ns, "main")
	if err != nil {
		logger.Error("retention: session build failed", "err", err)
		return
	}
	sess := session.New(authSess, 0, nil)
	window := time.Duration(hours) * time.Hour

	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				cutoff := time.Now().Add(-window).UTC().Format(time.RFC3339)
				n, err := exec.SweepOTelRetention(ctx, sess, cutoff)
				if err != nil {
					logger.Warn("retention sweep failed", "err", err)
					continue
				}
				if n > 0 {
					logger.Info("retention sweep removed spans", "removed", n, "older_than", cutoff)
				}
			}
		}
	}()
}
