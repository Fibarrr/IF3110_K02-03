package web

import (
	"bytes"
	"embed"
	"html/template"
	"io/fs"
	"net/http"
	"strings"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
)

//go:embed templates/*.html static/*
var content embed.FS

type pageData struct {
	Title      string
	Page       string
	DatabaseID string
}

type Handler struct {
	http.Handler
	pages map[string]*template.Template
}

// AdminPageAuth provides the same session and role checks used by admin APIs.
type AdminPageAuth interface {
	RequireUser(next http.Handler) http.Handler
}

func NewHandler(adminAuth ...AdminPageAuth) (*Handler, error) {
	pages := make(map[string]*template.Template)
	for _, name := range []string{"login.html", "workspace.html", "database.html", "admin.html", "admin_forbidden.html"} {
		tmpl, err := template.ParseFS(content, "templates/layout.html", "templates/"+name)
		if err != nil {
			return nil, err
		}
		pages[name] = tmpl
	}

	staticFS, err := fs.Sub(content, "static")
	if err != nil {
		return nil, err
	}

	h := &Handler{pages: pages}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux, staticFS, adminAuth...)
	h.Handler = mux
	return h, nil
}

func (h *Handler) RegisterRoutes(mux *http.ServeMux, staticFS fs.FS, adminAuth ...AdminPageAuth) {
	if staticFS == nil {
		staticFS, _ = fs.Sub(content, "static")
	}
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))
	mux.HandleFunc("GET /login", h.renderLogin)
	var adminRoute http.Handler
	if len(adminAuth) > 0 && adminAuth[0] != nil {
		adminRoute = adminAuth[0].RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, ok := accesscontrol.UserFromContext(r.Context())
			if !ok {
				http.Error(w, "perlu login", http.StatusUnauthorized)
				return
			}
			if !user.PlatformAdmin {
				h.renderAdminForbidden(w, r)
				return
			}
			h.renderAdmin(w, r)
		}))
	} else {
		adminRoute = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "akses admin belum dikonfigurasi", http.StatusServiceUnavailable)
		})
	}
	mux.Handle("GET /admin", adminRoute)
	mux.HandleFunc("GET /databases/{databaseID}", h.renderDatabase)
	mux.HandleFunc("GET /{$}", h.renderWorkspace)
}

func (h *Handler) renderLogin(w http.ResponseWriter, _ *http.Request) {
	h.render(w, "login.html", pageData{
		Title: "Masuk — Platform Manajemen Database",
		Page:  "login",
	})
}

func (h *Handler) renderWorkspace(w http.ResponseWriter, _ *http.Request) {
	h.render(w, "workspace.html", pageData{
		Title: "Pilih Database — Platform Manajemen Database",
		Page:  "workspace",
	})
}

func (h *Handler) renderDatabase(w http.ResponseWriter, r *http.Request) {
	dbID := strings.TrimSpace(r.PathValue("databaseID"))
	h.render(w, "database.html", pageData{
		Title:      "Explorer Database " + dbID + " — Platform Manajemen Database",
		Page:       "database",
		DatabaseID: dbID,
	})
}

func (h *Handler) renderAdmin(w http.ResponseWriter, _ *http.Request) {
	h.render(w, "admin.html", pageData{
		Title: "Panel Platform Admin — Platform Manajemen Database",
		Page:  "admin",
	})
}

func (h *Handler) renderAdminForbidden(w http.ResponseWriter, _ *http.Request) {
	h.renderStatus(w, http.StatusForbidden, "admin_forbidden.html", pageData{
		Title: "Akses Ditolak â€” Platform Manajemen Database",
		Page:  "admin-forbidden",
	})
}

func (h *Handler) render(w http.ResponseWriter, name string, data pageData) {
	h.renderStatus(w, http.StatusOK, name, data)
}

func (h *Handler) renderStatus(w http.ResponseWriter, status int, name string, data pageData) {
	tmpl, ok := h.pages[name]
	if !ok {
		http.Error(w, "halaman tidak ditemukan", http.StatusNotFound)
		return
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "layout", data); err != nil {
		http.Error(w, "gagal merender template", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(buf.Bytes())
}
