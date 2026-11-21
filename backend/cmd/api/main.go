package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/envfile"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/migration"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/web"
	_ "github.com/lib/pq"
)

func main() {
	envfile.Load(".env", "../.env", "../../.env")

	enableTraceConsole := !strings.EqualFold(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_CONSOLE")), "false")
	otelRT, err := telemetry.Init(telemetry.Config{
		ServiceName:   "if3110-db-platform-api",
		EnableConsole: enableTraceConsole,
	})
	if err != nil {
		slog.Error("gagal menginisialisasi telemetry", slog.String("error", err.Error()))
		os.Exit(1)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = otelRT.Shutdown(ctx)
	}()

	port := os.Getenv("BACKEND_PORT")
	if port == "" {
		port = "8080"
	}

	var apiHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "database platform belum dikonfigurasi"})
	})
	var adminPageAuth web.AdminPageAuth

	appCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL != "" {
		db, err := sql.Open("postgres", databaseURL)
		if err != nil {
			slog.Error("gagal membuka koneksi PostgreSQL")
			os.Exit(1)
		}
		defer db.Close()

		ctx, cancel := context.WithTimeout(appCtx, 10*time.Second)
		if err := db.PingContext(ctx); err != nil {
			cancel()
			slog.Error("PostgreSQL tidak bisa dihubungi")
			os.Exit(1)
		}
		acStore := accesscontrol.NewStore(db)
		if err := acStore.Migrate(ctx); err != nil {
			cancel()
			slog.Error("gagal menyiapkan tabel Access Control", slog.String("error", telemetry.RedactString(err.Error())))
			os.Exit(1)
		}
		dmStore := databasemanagement.NewStore(db)
		if err := dmStore.Migrate(ctx); err != nil {
			cancel()
			slog.Error("gagal menyiapkan tabel Database Management", slog.String("error", telemetry.RedactString(err.Error())))
			os.Exit(1)
		}
		migrationStore := migration.NewStore(db, dmStore, acStore)
		if err := migrationStore.Migrate(ctx); err != nil {
			cancel()
			slog.Error("gagal menyiapkan tabel Migration", slog.String("error", telemetry.RedactString(err.Error())))
			os.Exit(1)
		}
		cancel()

		// Embedded worker runs by default for easy local setup; set EMBEDDED_WORKER=false when running standalone cmd/worker.
		if !strings.EqualFold(strings.TrimSpace(os.Getenv("EMBEDDED_WORKER")), "false") {
			host, _ := os.Hostname()
			workerID := fmt.Sprintf("api-worker-%s-%s", host, telemetry.NewCorrelationID()[:8])
			worker := migration.NewWorker(migrationStore, migration.WorkerConfig{
				WorkerID:     workerID,
				Concurrency:  2,
				PollInterval: 400 * time.Millisecond,
				LockLeaseTTL: 30 * time.Second,
			})
			go worker.Run(appCtx)
		}

		adminUsername := os.Getenv("PLATFORM_ADMIN_USERNAME")
		adminPassword := os.Getenv("PLATFORM_ADMIN_PASSWORD")
		if adminUsername != "" || adminPassword != "" {
			if adminUsername == "" || adminPassword == "" {
				slog.Error("PLATFORM_ADMIN_USERNAME dan PLATFORM_ADMIN_PASSWORD harus diisi bersama")
				os.Exit(1)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := acStore.BootstrapAdmin(ctx, adminUsername, adminPassword)
			cancel()
			if err != nil {
				slog.Error("gagal menyiapkan Platform Admin awal", slog.String("error", telemetry.RedactString(err.Error())))
				os.Exit(1)
			}
		}

		sessions := accesscontrol.NewSessionManager(db, accesscontrol.DefaultSessionPolicy())
		acHandler := accesscontrol.NewHTTPHandler(acStore, sessions, strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"))
		adminPageAuth = acHandler
		dmViewer := databasemanagement.NewViewer(dmStore)
		dmHandler := databasemanagement.NewHTTPHandler(dmStore, dmViewer, acHandler, acStore)
		migrationHandler := migration.NewHTTPHandler(migrationStore, acHandler)

		apiMux := http.NewServeMux()
		dmHandler.RegisterRoutes(apiMux)
		migrationHandler.RegisterRoutes(apiMux)
		apiMux.Handle("/api/", acHandler)
		apiHandler = apiMux
	}

	mux, err := buildServerMux(apiHandler, adminPageAuth)
	if err != nil {
		slog.Error("gagal menyiapkan router web", slog.String("error", err.Error()))
		os.Exit(1)
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           telemetry.HTTPMiddleware(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-appCtx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()

	slog.Info("backend listening", slog.String("port", port))
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("backend server error", slog.String("error", err.Error()))
		os.Exit(1)
	}
}

func buildServerMux(apiHandler http.Handler, adminAuth ...web.AdminPageAuth) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.Handle("/api/", apiHandler)

	webHandler, err := web.NewHandler(adminAuth...)
	if err != nil {
		return nil, err
	}
	webHandler.RegisterRoutes(mux, nil, adminAuth...)
	return mux, nil
}
