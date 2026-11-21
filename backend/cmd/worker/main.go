package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/envfile"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/migration"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	_ "github.com/lib/pq"
)

func main() {
	envfile.Load(".env", "../.env", "../../.env")

	enableTraceConsole := !strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_CONSOLE")), "false")
	otelRT, err := telemetry.Init(telemetry.Config{
		ServiceName:   "if3110-migration-worker",
		EnableConsole: enableTraceConsole,
	})
	if err != nil {
		slog.Error("gagal menginisialisasi telemetry worker", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = otelRT.Shutdown(ctx)
	}()

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL == "" {
		slog.Error("DATABASE_URL wajib diisi untuk menjalankan migration worker")
		os.Exit(1)
	}

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		slog.Error("gagal membuka koneksi PostgreSQL")
		os.Exit(1)
	}
	defer db.Close()

	initCtx, initCancel := context.WithTimeout(context.Background(), 10*time.Second)
	if err := db.PingContext(initCtx); err != nil {
		initCancel()
		slog.Error("PostgreSQL tidak bisa dihubungi")
		os.Exit(1)
	}
	acStore := accesscontrol.NewStore(db)
	dmStore := databasemanagement.NewStore(db)
	migrationStore := migration.NewStore(db, dmStore, acStore)
	for _, step := range []struct {
		name string
		fn   func(context.Context) error
	}{
		{"Access Control", acStore.Migrate},
		{"Database Management", dmStore.Migrate},
		{"Migration", migrationStore.Migrate},
	} {
		if err := step.fn(initCtx); err != nil {
			initCancel()
			slog.Error("gagal menyiapkan tabel", slog.String("module", step.name), slog.String("error", telemetry.RedactString(err.Error())))
			os.Exit(1)
		}
	}
	initCancel()

	concurrency := 2
	if raw := strings.TrimSpace(os.Getenv("WORKER_CONCURRENCY")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 16 {
			concurrency = parsed
		}
	}
	pollInterval := 400 * time.Millisecond
	if raw := strings.TrimSpace(os.Getenv("WORKER_POLL_INTERVAL_MS")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 50 {
			pollInterval = time.Duration(parsed) * time.Millisecond
		}
	}

	worker := migration.NewWorker(migrationStore, migration.WorkerConfig{
		WorkerID:     strings.TrimSpace(os.Getenv("WORKER_ID")),
		Concurrency:  concurrency,
		PollInterval: pollInterval,
		LockLeaseTTL: 30 * time.Second,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	slog.Info("migration worker started", slog.Int("concurrency", concurrency), slog.String("poll", pollInterval.String()))
	worker.Run(ctx)
	slog.Info("migration worker stopped cleanly")
}
