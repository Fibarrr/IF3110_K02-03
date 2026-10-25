package migration

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
)

type AuthGuard interface {
	RequireCSRF(next http.Handler) http.Handler
	RequireDatabaseAccess(databaseID func(*http.Request) string, required accesscontrol.DatabaseAccess, next http.Handler) http.Handler
}

type HTTPHandler struct {
	http.Handler
	store *Store
	auth  AuthGuard
}

func NewHTTPHandler(store *Store, auth AuthGuard) *HTTPHandler {
	h := &HTTPHandler{store: store, auth: auth}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	h.Handler = mux
	return h
}

func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
	databaseID := func(r *http.Request) string { return strings.ToLower(strings.TrimSpace(r.PathValue("databaseID"))) }
	view := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(databaseID, accesscontrol.AccessViewOnly, handler)
	}
	edit := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(databaseID, accesscontrol.AccessViewEdit, h.auth.RequireCSRF(handler))
	}

	mux.Handle("GET /api/databases/{databaseID}/migrations", view(h.list))
	mux.Handle("POST /api/databases/{databaseID}/migrations", edit(h.create))
	mux.Handle("GET /api/databases/{databaseID}/migrations/{migrationID}", view(h.get))
	mux.Handle("PUT /api/databases/{databaseID}/migrations/{migrationID}", edit(h.update))
	mux.Handle("POST /api/databases/{databaseID}/migrations/{migrationID}/execute", edit(h.execute))
}

func (h *HTTPHandler) list(w http.ResponseWriter, r *http.Request) {
	migrations, err := h.store.List(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil riwayat migration")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"migrations": migrations})
}

func (h *HTTPHandler) get(w http.ResponseWriter, r *http.Request) {
	id, ok := parseMigrationID(w, r)
	if !ok {
		return
	}
	m, err := h.store.Get(r.Context(), r.PathValue("databaseID"), id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *HTTPHandler) create(w http.ResponseWriter, r *http.Request) {
	user, ok := accesscontrol.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "perlu login")
		return
	}
	var input Input
	if !decodeJSON(w, r, &input) {
		return
	}
	m, err := h.store.Create(r.Context(), r.PathValue("databaseID"), user.ID, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, m)
}

func (h *HTTPHandler) update(w http.ResponseWriter, r *http.Request) {
	user, ok := accesscontrol.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "perlu login")
		return
	}
	id, ok := parseMigrationID(w, r)
	if !ok {
		return
	}
	var input Input
	if !decodeJSON(w, r, &input) {
		return
	}
	m, err := h.store.Update(r.Context(), r.PathValue("databaseID"), id, user.ID, input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func (h *HTTPHandler) execute(w http.ResponseWriter, r *http.Request) {
	user, ok := accesscontrol.UserFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "perlu login")
		return
	}
	id, ok := parseMigrationID(w, r)
	if !ok {
		return
	}
	m, err := h.store.Execute(r.Context(), r.PathValue("databaseID"), id, user.ID)
	if err != nil {
		switch {
		case errors.Is(err, ErrExecution):
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"migration": m, "error": ErrExecution.Error()})
		case errors.Is(err, databasemanagement.ErrTargetConnection):
			writeJSON(w, http.StatusBadGateway, map[string]any{"migration": m, "error": "gagal terhubung ke database target"})
		default:
			writeStoreError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, m)
}

func parseMigrationID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("migrationID"), 10, 64)
	if err != nil || id < 1 {
		writeError(w, http.StatusBadRequest, "ID migration tidak valid")
		return 0, false
	}
	return id, true
}

func decodeJSON(w http.ResponseWriter, r *http.Request, target any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		writeError(w, http.StatusBadRequest, "body JSON tidak valid")
		return false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "body harus berisi satu objek JSON")
		return false
	}
	return true
}

func writeStoreError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrNotFound), errors.Is(err, databasemanagement.ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrAlreadyApplied), errors.Is(err, ErrAlreadyRunning), errors.Is(err, ErrNotEditable), errors.Is(err, ErrOutOfOrder):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, databasemanagement.ErrTargetConnection):
		writeError(w, http.StatusBadGateway, "gagal terhubung ke database target")
	default:
		var inputErr InputError
		if errors.As(err, &inputErr) {
			writeError(w, http.StatusBadRequest, inputErr.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "terjadi kesalahan di server")
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	data, err := json.Marshal(value)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengenkode respons JSON")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
