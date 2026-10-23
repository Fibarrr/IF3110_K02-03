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
	_ "github.com/lib/pq"
)

func main() {
	port := os.Getenv("BACKEND_PORT")
	if port == "" {
		port = "8080"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{\"status\":\"ok\"}\n"))
	})
	var apiHandler http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "database access control belum dikonfigurasi"})
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
		store := accesscontrol.NewStore(db)
		if err := store.Migrate(ctx); err != nil {
			cancel()
			log.Fatal("gagal menyiapkan tabel Access Control")
		}
		cancel()

		adminUsername := os.Getenv("PLATFORM_ADMIN_USERNAME")
		adminPassword := os.Getenv("PLATFORM_ADMIN_PASSWORD")
		if adminUsername != "" || adminPassword != "" {
			if adminUsername == "" || adminPassword == "" {
				log.Fatal("PLATFORM_ADMIN_USERNAME dan PLATFORM_ADMIN_PASSWORD harus diisi bersama")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := store.BootstrapAdmin(ctx, adminUsername, adminPassword)
			cancel()
			if err != nil {
				log.Fatal("gagal menyiapkan Platform Admin awal")
			}
		}

		apiHandler = accesscontrol.NewHTTPHandler(store, accesscontrol.NewSessionManager(db, accesscontrol.DefaultSessionPolicy()), strings.EqualFold(os.Getenv("COOKIE_SECURE"), "true"))
	}
	mux.Handle("/api/", apiHandler)

	server := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("backend listening on :%s", port)
	log.Fatal(server.ListenAndServe())
}
