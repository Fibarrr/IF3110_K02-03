package databasemanagement

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
)

type CatalogStore interface {
	ListConnections(ctx context.Context) ([]DatabaseConnection, error)
	GetConnection(ctx context.Context, databaseID string) (DatabaseConnection, error)
	CreateConnection(ctx context.Context, input ConnectionInput) (DatabaseConnection, error)
	UpdateConnection(ctx context.Context, databaseID string, input ConnectionInput) (DatabaseConnection, error)
	DeleteConnection(ctx context.Context, databaseID string) error
}

type SchemaViewer interface {
	PingConnection(ctx context.Context, databaseID string) (ConnectionStatus, error)
	ListSchemas(ctx context.Context, databaseID string) ([]SchemaSummary, error)
	GetTableDetails(ctx context.Context, databaseID, schemaName, tableName string) (TableDetails, error)
	GetTableRows(ctx context.Context, databaseID, schemaName, tableName string, limit, offset int) (TableRowsPage, error)
}

type AuthGuard interface {
	RequireUser(next http.Handler) http.Handler
	RequirePlatformAdmin(next http.Handler) http.Handler
	RequireCSRF(next http.Handler) http.Handler
	RequireDatabaseAccess(databaseID func(*http.Request) string, required accesscontrol.DatabaseAccess, next http.Handler) http.Handler
}

type AccessResolver interface {
	EffectiveAccess(ctx context.Context, userID int64, databaseID string) (accesscontrol.DatabaseAccess, error)
	ListGroups(ctx context.Context) ([]accesscontrol.Group, error)
	RemoveDatabaseAccess(ctx context.Context, groupID int64, databaseID string) error
}

type AccessibleDatabase struct {
	DatabaseConnection
	Access accesscontrol.DatabaseAccess `json:"access"`
}

type HTTPHandler struct {
	http.Handler
	store  CatalogStore
	viewer SchemaViewer
	auth   AuthGuard
	access AccessResolver
}

func NewHTTPHandler(store CatalogStore, viewer SchemaViewer, auth AuthGuard, access AccessResolver) *HTTPHandler {
	h := &HTTPHandler{
		store:  store,
		viewer: viewer,
		auth:   auth,
		access: access,
	}
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	h.Handler = mux
	return h
}

// RegisterRoutes mendaftarkan seluruh endpoint Database Management ke ServeMux yang diberikan.
func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
	adminRead := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireUser(h.auth.RequirePlatformAdmin(handler))
	}
	adminMutate := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireUser(h.auth.RequirePlatformAdmin(h.auth.RequireCSRF(handler)))
	}
	dbIDFromPath := func(r *http.Request) string {
		return strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	}
	dbView := func(handler http.HandlerFunc) http.Handler {
		return h.auth.RequireDatabaseAccess(dbIDFromPath, accesscontrol.AccessViewOnly, handler)
	}

	mux.Handle("GET /api/admin/databases", adminRead(h.listAdminDatabases))
	mux.Handle("POST /api/admin/databases", adminMutate(h.createDatabase))
	mux.Handle("GET /api/admin/databases/{databaseID}", adminRead(h.getAdminDatabase))
	mux.Handle("PUT /api/admin/databases/{databaseID}", adminMutate(h.updateDatabase))
	mux.Handle("DELETE /api/admin/databases/{databaseID}", adminMutate(h.deleteDatabase))
	mux.Handle("POST /api/admin/databases/{databaseID}/test", adminMutate(h.testDatabaseConnection))

	mux.Handle("GET /api/databases", h.auth.RequireUser(http.HandlerFunc(h.listAccessibleDatabases)))
	mux.Handle("GET /api/databases/{databaseID}", dbView(h.getDatabaseSummary))
	mux.Handle("GET /api/databases/{databaseID}/schemas", dbView(h.listSchemas))
	mux.Handle("GET /api/databases/{databaseID}/schemas/{schemaName}/tables/{tableName}", dbView(h.getTableDetails))
	mux.Handle("GET /api/databases/{databaseID}/schemas/{schemaName}/tables/{tableName}/rows", dbView(h.getTableRows))
}

func (h *HTTPHandler) listAdminDatabases(w http.ResponseWriter, r *http.Request) {
	databases, err := h.store.ListConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil daftar koneksi database")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": databases})
}

func (h *HTTPHandler) getAdminDatabase(w http.ResponseWriter, r *http.Request) {
	conn, err := h.store.GetConnection(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (h *HTTPHandler) createDatabase(w http.ResponseWriter, r *http.Request) {
	var input ConnectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	conn, err := h.store.CreateConnection(r.Context(), input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, conn)
}

func (h *HTTPHandler) updateDatabase(w http.ResponseWriter, r *http.Request) {
	var input ConnectionInput
	if !decodeJSON(w, r, &input) {
		return
	}
	conn, err := h.store.UpdateConnection(r.Context(), r.PathValue("databaseID"), input)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, conn)
}

func (h *HTTPHandler) deleteDatabase(w http.ResponseWriter, r *http.Request) {
	dbID := strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	if _, err := h.store.GetConnection(r.Context(), dbID); err != nil {
		writeStoreError(w, err)
		return
	}
	// Jika store bukan *Store yang sudah menghapus grant dalam transaksi SQL yang sama,
	// cabut semua grant melalui AccessResolver dan pastikan tidak ada error sebelum menghapus koneksi.
	if _, isSQLStore := h.store.(*Store); !isSQLStore && h.access != nil {
		groups, err := h.access.ListGroups(r.Context())
		if err != nil {
			writeError(w, http.StatusInternalServerError, "gagal membersihkan izin grup untuk database")
			return
		}
		for _, g := range groups {
			if err := h.access.RemoveDatabaseAccess(r.Context(), g.ID, dbID); err != nil && !errors.Is(err, accesscontrol.ErrNotFound) {
				writeError(w, http.StatusInternalServerError, "gagal membersihkan izin grup untuk database")
				return
			}
		}
	}
	if err := h.store.DeleteConnection(r.Context(), dbID); err != nil {
		writeStoreError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (h *HTTPHandler) testDatabaseConnection(w http.ResponseWriter, r *http.Request) {
	status, err := h.viewer.PingConnection(r.Context(), r.PathValue("databaseID"))
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			writeStoreError(w, err)
			return
		}
		writeJSON(w, http.StatusBadGateway, status)
		return
	}
	writeJSON(w, http.StatusOK, status)
}

func (h *HTTPHandler) listAccessibleDatabases(w http.ResponseWriter, r *http.Request) {
	user, ok := h.currentUser(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "perlu login")
		return
	}
	all, err := h.store.ListConnections(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "gagal mengambil daftar database")
		return
	}
	accessible := make([]AccessibleDatabase, 0)
	for _, conn := range all {
		access, err := h.access.EffectiveAccess(r.Context(), user.ID, conn.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "gagal mengecek akses database")
			return
		}
		if access.CanView() {
			accessible = append(accessible, AccessibleDatabase{
				DatabaseConnection: conn,
				Access:             access,
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"databases": accessible})
}

func (h *HTTPHandler) getDatabaseSummary(w http.ResponseWriter, r *http.Request) {
	dbID := strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	conn, err := h.store.GetConnection(r.Context(), dbID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	status, err := h.viewer.PingConnection(r.Context(), dbID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var access accesscontrol.DatabaseAccess = accesscontrol.AccessViewOnly
	if user, ok := h.currentUser(r); ok && h.access != nil {
		if resolved, err := h.access.EffectiveAccess(r.Context(), user.ID, dbID); err == nil {
			access = resolved
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database": AccessibleDatabase{DatabaseConnection: conn, Access: access},
		"status":   status,
	})
}

func (h *HTTPHandler) listSchemas(w http.ResponseWriter, r *http.Request) {
	dbID := strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	conn, err := h.store.GetConnection(r.Context(), dbID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	schemas, err := h.viewer.ListSchemas(r.Context(), dbID)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	var access accesscontrol.DatabaseAccess = accesscontrol.AccessViewOnly
	if user, ok := h.currentUser(r); ok && h.access != nil {
		if resolved, err := h.access.EffectiveAccess(r.Context(), user.ID, dbID); err == nil {
			access = resolved
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"database": conn,
		"access":   access,
		"schemas":  schemas,
	})
}

func (h *HTTPHandler) getTableDetails(w http.ResponseWriter, r *http.Request) {
	dbID := strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	details, err := h.viewer.GetTableDetails(r.Context(), dbID, r.PathValue("schemaName"), r.PathValue("tableName"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, details)
}

func (h *HTTPHandler) getTableRows(w http.ResponseWriter, r *http.Request) {
	limit, offset, err := normalizePagination(r.URL.Query().Get("limit"), r.URL.Query().Get("offset"))
	if err != nil {
		writeStoreError(w, err)
		return
	}
	dbID := strings.ToLower(strings.TrimSpace(r.PathValue("databaseID")))
	page, err := h.viewer.GetTableRows(r.Context(), dbID, r.PathValue("schemaName"), r.PathValue("tableName"), limit, offset)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (h *HTTPHandler) currentUser(r *http.Request) (accesscontrol.User, bool) {
	if u, ok := accesscontrol.UserFromContext(r.Context()); ok {
		return u, true
	}
	if custom, ok := h.auth.(interface {
		UserFromRequest(*http.Request) (accesscontrol.User, bool)
	}); ok {
		return custom.UserFromRequest(r)
	}
	return accesscontrol.User{}, false
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
	case errors.Is(err, ErrNotFound):
		writeError(w, http.StatusNotFound, err.Error())
	case errors.Is(err, ErrConflict):
		writeError(w, http.StatusConflict, err.Error())
	case errors.Is(err, ErrTargetConnection):
		writeError(w, http.StatusBadGateway, err.Error())
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
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"gagal mengenkode respons JSON"}` + "\n"))
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(append(data, '\n'))
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
