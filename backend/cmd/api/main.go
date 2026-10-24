package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/web"
	_ "github.com/lib/pq"
)

func main() {
	port := os.Getenv("BACKEND_PORT")
	if port == "" {
		port = "8080"
	}

	var apiHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "database platform belum dikonfigurasi"})
	})

	databaseURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if databaseURL != "" {
		db, err := sql.Open("postgres", databaseURL)
		if err != nil {
			log.Fatal("gagal membuka koneksi PostgreSQL")
		}
		defer db.Close()

		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := db.PingContext(ctx); err != nil {
			cancel()
			log.Fatal("PostgreSQL tidak bisa dihubungi")
		}
		acStore := accesscontrol.NewStore(db)
		if err := acStore.Migrate(ctx); err != nil {
			cancel()
			log.Fatal("gagal menyiapkan tabel Access Control")
		}
		dmStore := databasemanagement.NewStore(db)
		if err := dmStore.Migrate(ctx); err != nil {
			cancel()
			log.Fatal("gagal menyiapkan tabel Database Management")
		}
		cancel()

		adminUsername := os.Getenv("PLATFORM_ADMIN_USERNAME")
		adminPassword := os.Getenv("PLATFORM_ADMIN_PASSWORD")
		if adminUsername != "" || adminPassword != "" {
			if adminUsername == "" || adminPassword == "" {
				log.Fatal("PLATFORM_ADMIN_USERNAME dan PLATFORM_ADMIN_PASSWORD harus diisi bersama")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := acStore.BootstrapAdmin(ctx, adminUsername, adminPassword)
			cancel()
			if err != nil {
				log.Fatal("gagal menyiapkan Platform Admin awal")
			}
		}

		sessions := accesscontrol.NewSessionManager(db, accesscontrol.DefaultSessionPolicy())
		acHandler := accesscontrol.NewHTTPHandler(acStore, sessions, strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"))
		dmViewer := databasemanagement.NewViewer(dmStore)
		dmHandler := databasemanagement.NewHTTPHandler(dmStore, dmViewer, acHandler, acStore)

		apiMux := http.NewServeMux()
		dmHandler.RegisterRoutes(apiMux)
		apiMux.Handle("/api/", acHandler)
		apiHandler = apiMux
	}

	mux, err := buildServerMux(apiHandler)
	if err != nil {
		log.Fatal("gagal menyiapkan router web")
	}

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("backend listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}

func buildServerMux(apiHandler http.Handler) (*http.ServeMux, error) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	mux.Handle("/api/", apiHandler)

	webHandler, err := web.NewHandler()
	if err != nil {
		return nil, err
	}
	webHandler.RegisterRoutes(mux, nil)
	return mux, nil
}
