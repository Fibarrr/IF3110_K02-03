package web

import (
	"embed"
	"io/fs"
	"net/http"
	"path"
	"strings"
)

//go:embed all:dist
var distContent embed.FS

// Handler serves the embedded React + Vite Single-Page Application (SPA)
// and falls back to index.html for client-side routes.
type Handler struct {
	http.Handler
	distFS    fs.FS
	indexHTML []byte
}

func NewHandler() (*Handler, error) {
	distFS, err := fs.Sub(distContent, "dist")
	if err != nil {
		return nil, err
	}
	indexHTML, err := fs.ReadFile(distFS, "index.html")
	if err != nil {
		return nil, err
	}

	h := &Handler{distFS: distFS, indexHTML: indexHTML}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, distFS)
	h.Handler = mux
	return h, nil
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, assetFS fs.FS) {
	if assetFS == nil {
		assetFS = h.distFS
	}
	fileServer := http.FileServer(http.FS(assetFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/health" {
			http.NotFound(w, r)
			return
		}
		cleanPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if cleanPath != "" && cleanPath != "." {
			if info, err := fs.Stat(assetFS, cleanPath); err == nil && !info.IsDir() {
				fileServer.ServeHTTP(w, r)
				return
			}
			if strings.HasPrefix(cleanPath, "assets/") {
				http.NotFound(w, r)
				return
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(h.indexHTML)
	})
}
