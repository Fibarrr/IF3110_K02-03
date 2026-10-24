package databasemanagement

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
)

func TestNormalizePagination(t *testing.T) {
	tests := []struct {
		name       string
		rawLimit   string
		rawOffset  string
		wantLimit  int
		wantOffset int
		wantErr    bool
	}{
		{name: "defaults when empty", rawLimit: "", rawOffset: "", wantLimit: 50, wantOffset: 0},
		{name: "valid custom values", rawLimit: "25", rawOffset: "100", wantLimit: 25, wantOffset: 100},
		{name: "clamps limit above max", rawLimit: "999", rawOffset: "0", wantLimit: 200, wantOffset: 0},
		{name: "rejects zero limit", rawLimit: "0", rawOffset: "0", wantErr: true},
		{name: "rejects negative offset", rawLimit: "10", rawOffset: "-5", wantErr: true},
		{name: "rejects non-numeric limit", rawLimit: "abc", rawOffset: "0", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			limit, offset, err := normalizePagination(tc.rawLimit, tc.rawOffset)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizePagination(%q, %q) seharusnya gagal", tc.rawLimit, tc.rawOffset)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePagination(%q, %q) gagal: %v", tc.rawLimit, tc.rawOffset, err)
			}
			if limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("normalizePagination(%q, %q) = (%d, %d), mau (%d, %d)",
					tc.rawLimit, tc.rawOffset, limit, offset, tc.wantLimit, tc.wantOffset)
			}
		})
	}
}

func TestFormatCellValue(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	if got := formatCellValue(nil); got != nil {
		t.Fatalf("formatCellValue(nil) = %#v, mau nil", got)
	}
	if got := formatCellValue([]byte("halo")); got != "halo" {
		t.Fatalf("formatCellValue([]byte) = %#v, mau %q", got, "halo")
	}
	if got := formatCellValue(now); got != "2026-10-24T12:00:00Z" {
		t.Fatalf("formatCellValue(time.Time) = %#v, mau RFC3339", got)
	}
	if got := formatCellValue(int64(42)); got != int64(42) {
		t.Fatalf("formatCellValue(int64) = %#v, mau 42", got)
	}
	if got := formatCellValue(math.NaN()); got != "NaN" {
		t.Fatalf("formatCellValue(NaN) = %#v, mau %q", got, "NaN")
	}
	if got := formatCellValue(math.Inf(1)); got != "Infinity" {
		t.Fatalf("formatCellValue(+Inf) = %#v, mau %q", got, "Infinity")
	}
	if got := formatCellValue(math.Inf(-1)); got != "-Infinity" {
		t.Fatalf("formatCellValue(-Inf) = %#v, mau %q", got, "-Infinity")
	}
	if got := formatCellValue(float32(math.Inf(1))); got != "Infinity" {
		t.Fatalf("formatCellValue(float32 +Inf) = %#v, mau %q", got, "Infinity")
	}
}

func TestIndexColumnsQueryUsesOneBasedOrdinality(t *testing.T) {
	if strings.Contains(indexDetailsQuery, "generate_subscripts") {
		t.Fatal("indexDetailsQuery tidak boleh memakai generate_subscripts berbasis 0 untuk pg_get_indexdef")
	}
	if !strings.Contains(indexDetailsQuery, "WITH ORDINALITY") {
		t.Fatal("indexDetailsQuery harus memakai WITH ORDINALITY (1-based) untuk pg_get_indexdef")
	}
}

func TestIsSystemSchema(t *testing.T) {
	for _, sys := range []string{"pg_catalog", "information_schema", "pg_toast", "pg_toast_temp_1", "pg_temp_3"} {
		if !isSystemSchema(sys) {
			t.Fatalf("schema %q seharusnya dianggap schema sistem", sys)
		}
	}
	for _, userSchema := range []string{"public", "sales", "app_data"} {
		if isSystemSchema(userSchema) {
			t.Fatalf("schema %q seharusnya bukan schema sistem", userSchema)
		}
	}
}

func TestViewerRejectsSystemSchemasBeforeConnecting(t *testing.T) {
	viewer := NewViewer(NewStore(nil))
	if _, err := viewer.GetTableDetails(context.Background(), "db-1", "pg_catalog", "pg_authid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTableDetails pada pg_catalog = %v, mau ErrNotFound", err)
	}
	if _, err := viewer.GetTableRows(context.Background(), "db-1", "information_schema", "tables", 10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTableRows pada information_schema = %v, mau ErrNotFound", err)
	}
}

type fakeTestHarness struct {
	users      map[int64]accesscontrol.User
	tokens     map[string]int64
	csrfTokens map[string]string
	grants     map[int64]map[string]accesscontrol.DatabaseAccess
	groups     []accesscontrol.Group
	failRevoke error
	store      *memoryCatalogStore
	viewer     *stubSchemaViewer
	handler    *HTTPHandler
}

type memoryCatalogStore struct {
	items map[string]storedConnection
}

func newMemoryCatalogStore() *memoryCatalogStore {
	return &memoryCatalogStore{items: make(map[string]storedConnection)}
}

func (m *memoryCatalogStore) ListConnections(_ context.Context) ([]DatabaseConnection, error) {
	out := make([]DatabaseConnection, 0, len(m.items))
	for _, item := range m.items {
		out = append(out, item.DatabaseConnection)
	}
	return out, nil
}

func (m *memoryCatalogStore) GetConnection(_ context.Context, databaseID string) (DatabaseConnection, error) {
	item, ok := m.items[strings.ToLower(strings.TrimSpace(databaseID))]
	if !ok {
		return DatabaseConnection{}, ErrNotFound
	}
	return item.DatabaseConnection, nil
}

func (m *memoryCatalogStore) CreateConnection(_ context.Context, input ConnectionInput) (DatabaseConnection, error) {
	normalized, err := validateConnectionInput(input, true)
	if err != nil {
		return DatabaseConnection{}, err
	}
	if _, exists := m.items[normalized.ID]; exists {
		return DatabaseConnection{}, ErrConflict
	}
	now := time.Now().UTC()
	stored := storedConnection{
		DatabaseConnection: DatabaseConnection{
			ID:           normalized.ID,
			Name:         normalized.Name,
			Host:         normalized.Host,
			Port:         normalized.Port,
			DatabaseName: normalized.DatabaseName,
			Username:     normalized.Username,
			SSLMode:      normalized.SSLMode,
			Description:  normalized.Description,
			HasPassword:  normalized.Password != "",
			CreatedAt:    now,
			UpdatedAt:    now,
		},
		Password: normalized.Password,
	}
	m.items[normalized.ID] = stored
	return stored.DatabaseConnection, nil
}

func (m *memoryCatalogStore) UpdateConnection(_ context.Context, databaseID string, input ConnectionInput) (DatabaseConnection, error) {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	existing, ok := m.items[databaseID]
	if !ok {
		return DatabaseConnection{}, ErrNotFound
	}
	input.ID = databaseID
	normalized, err := validateConnectionInput(input, false)
	if err != nil {
		return DatabaseConnection{}, err
	}
	pwd := existing.Password
	if normalized.Password != "" {
		pwd = normalized.Password
	}
	existing.Name = normalized.Name
	existing.Host = normalized.Host
	existing.Port = normalized.Port
	existing.DatabaseName = normalized.DatabaseName
	existing.Username = normalized.Username
	existing.SSLMode = normalized.SSLMode
	existing.Description = normalized.Description
	existing.Password = pwd
	existing.HasPassword = pwd != ""
	existing.UpdatedAt = time.Now().UTC()
	m.items[databaseID] = existing
	return existing.DatabaseConnection, nil
}

func (m *memoryCatalogStore) DeleteConnection(_ context.Context, databaseID string) error {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if _, ok := m.items[databaseID]; !ok {
		return ErrNotFound
	}
	delete(m.items, databaseID)
	return nil
}

type stubSchemaViewer struct {
	store     *memoryCatalogStore
	failByID  map[string]error
	schemas   map[string][]SchemaSummary
	details   map[string]TableDetails
	rowsPages map[string]TableRowsPage
}

func (v *stubSchemaViewer) PingConnection(_ context.Context, databaseID string) (ConnectionStatus, error) {
	conn, err := v.store.GetConnection(context.Background(), databaseID)
	if err != nil {
		return ConnectionStatus{}, err
	}
	if failErr, ok := v.failByID[conn.ID]; ok {
		return ConnectionStatus{
			DatabaseID: conn.ID,
			Connected:  false,
			Error:      sanitizeConnectionError(failErr, v.store.items[conn.ID].Password),
			CheckedAt:  time.Now().UTC(),
		}, TargetConnectionError{Message: sanitizeConnectionError(failErr, v.store.items[conn.ID].Password)}
	}
	return ConnectionStatus{
		DatabaseID:      conn.ID,
		Connected:       true,
		CurrentDatabase: conn.DatabaseName,
		CurrentUser:     conn.Username,
		ServerVersion:   "PostgreSQL 16.4",
		CheckedAt:       time.Now().UTC(),
	}, nil
}

func (v *stubSchemaViewer) ListSchemas(_ context.Context, databaseID string) ([]SchemaSummary, error) {
	conn, err := v.store.GetConnection(context.Background(), databaseID)
	if err != nil {
		return nil, err
	}
	if failErr, ok := v.failByID[conn.ID]; ok {
		return nil, TargetConnectionError{Message: sanitizeConnectionError(failErr, v.store.items[conn.ID].Password)}
	}
	return v.schemas[conn.ID], nil
}

func (v *stubSchemaViewer) GetTableDetails(_ context.Context, databaseID, schemaName, tableName string) (TableDetails, error) {
	conn, err := v.store.GetConnection(context.Background(), databaseID)
	if err != nil {
		return TableDetails{}, err
	}
	if failErr, ok := v.failByID[conn.ID]; ok {
		return TableDetails{}, TargetConnectionError{Message: sanitizeConnectionError(failErr, v.store.items[conn.ID].Password)}
	}
	key := conn.ID + ":" + schemaName + "." + tableName
	detail, ok := v.details[key]
	if !ok {
		return TableDetails{}, ErrNotFound
	}
	return detail, nil
}

func (v *stubSchemaViewer) GetTableRows(_ context.Context, databaseID, schemaName, tableName string, limit, offset int) (TableRowsPage, error) {
	conn, err := v.store.GetConnection(context.Background(), databaseID)
	if err != nil {
		return TableRowsPage{}, err
	}
	if failErr, ok := v.failByID[conn.ID]; ok {
		return TableRowsPage{}, TargetConnectionError{Message: sanitizeConnectionError(failErr, v.store.items[conn.ID].Password)}
	}
	key := conn.ID + ":" + schemaName + "." + tableName
	page, ok := v.rowsPages[key]
	if !ok {
		return TableRowsPage{}, ErrNotFound
	}
	page.Limit = limit
	page.Offset = offset
	return page, nil
}

func newFakeTestHarness() *fakeTestHarness {
	store := newMemoryCatalogStore()
	viewer := &stubSchemaViewer{
		store:     store,
		failByID:  make(map[string]error),
		schemas:   make(map[string][]SchemaSummary),
		details:   make(map[string]TableDetails),
		rowsPages: make(map[string]TableRowsPage),
	}
	h := &fakeTestHarness{
		users: map[int64]accesscontrol.User{
			1: {ID: 1, Username: "admin_user", PlatformAdmin: true},
			2: {ID: 2, Username: "viewer_user", PlatformAdmin: false},
			3: {ID: 3, Username: "outsider_user", PlatformAdmin: false},
		},
		tokens: map[string]int64{
			"tok-admin":    1,
			"tok-viewer":   2,
			"tok-outsider": 3,
		},
		csrfTokens: map[string]string{
			"tok-admin":    "csrf-admin",
			"tok-viewer":   "csrf-viewer",
			"tok-outsider": "csrf-outsider",
		},
		grants: map[int64]map[string]accesscontrol.DatabaseAccess{
			1: {},
			2: {"orders-db": accesscontrol.AccessViewOnly},
			3: {},
		},
		groups: []accesscontrol.Group{{ID: 10, Name: "analysts"}},
		store:  store,
		viewer: viewer,
	}
	h.handler = NewHTTPHandler(store, viewer, h, h)
	return h
}

type fakeCtxKey uint8

const fakeUserCtxKey fakeCtxKey = 1

func (f *fakeTestHarness) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("if3110_session")
		if err != nil {
			writeError(w, http.StatusUnauthorized, "perlu login")
			return
		}
		uid, ok := f.tokens[c.Value]
		if !ok {
			writeError(w, http.StatusUnauthorized, "sesi tidak berlaku")
			return
		}
		user := f.users[uid]
		ctx := context.WithValue(r.Context(), fakeUserCtxKey, user)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (f *fakeTestHarness) RequirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := f.UserFromRequest(r)
		if !user.PlatformAdmin {
			writeError(w, http.StatusForbidden, "khusus Platform Admin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fakeTestHarness) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("if3110_session")
		if err != nil || r.Header.Get("X-CSRF-Token") != f.csrfTokens[c.Value] {
			writeError(w, http.StatusForbidden, "request CSRF ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (f *fakeTestHarness) RequireDatabaseAccess(databaseID func(*http.Request) string, required accesscontrol.DatabaseAccess, next http.Handler) http.Handler {
	return f.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, _ := f.UserFromRequest(r)
		access, _ := f.EffectiveAccess(r.Context(), user.ID, databaseID(r))
		allowed := required == accesscontrol.AccessViewOnly && access.CanView() ||
			required == accesscontrol.AccessViewEdit && access.CanEdit()
		if !allowed {
			writeError(w, http.StatusForbidden, "akses ke database ditolak")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func (f *fakeTestHarness) UserFromRequest(r *http.Request) (accesscontrol.User, bool) {
	if u, ok := accesscontrol.UserFromContext(r.Context()); ok {
		return u, true
	}
	u, ok := r.Context().Value(fakeUserCtxKey).(accesscontrol.User)
	return u, ok
}

func (f *fakeTestHarness) EffectiveAccess(_ context.Context, userID int64, databaseID string) (accesscontrol.DatabaseAccess, error) {
	userGrants := f.grants[userID]
	if userGrants == nil {
		return accesscontrol.AccessNone, nil
	}
	if acc, ok := userGrants[databaseID]; ok {
		return acc, nil
	}
	return accesscontrol.AccessNone, nil
}

func (f *fakeTestHarness) ListGroups(_ context.Context) ([]accesscontrol.Group, error) {
	if f.failRevoke != nil {
		return nil, f.failRevoke
	}
	return f.groups, nil
}

func (f *fakeTestHarness) RemoveDatabaseAccess(_ context.Context, _ int64, databaseID string) error {
	if f.failRevoke != nil {
		return f.failRevoke
	}
	for uid := range f.grants {
		delete(f.grants[uid], databaseID)
	}
	return nil
}

func TestHTTPDatabaseManagementFlows(t *testing.T) {
	h := newFakeTestHarness()

	// 1. Unauthenticated request is rejected (401)
	req := httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	rec := httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated GET /api/databases status = %d, mau %d", rec.Code, http.StatusUnauthorized)
	}

	// 2. Non-admin user cannot create or list admin databases (403)
	req = httptest.NewRequest(http.MethodGet, "/api/admin/databases", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("regular user GET /api/admin/databases status = %d, mau %d", rec.Code, http.StatusForbidden)
	}

	// 3. Admin mutation without CSRF token is rejected (403)
	createBody := `{"id":"orders-db","name":"Orders DB","host":"localhost","port":5432,"databaseName":"orders","username":"orders_user","password":"secret-db-password","sslMode":"disable","description":"Main orders database"}`
	req = httptest.NewRequest(http.MethodPost, "/api/admin/databases", strings.NewReader(createBody))
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("admin POST without CSRF status = %d, mau %d", rec.Code, http.StatusForbidden)
	}

	// 4. Admin creates database connection with CSRF token (201), response never leaks password
	req = httptest.NewRequest(http.MethodPost, "/api/admin/databases", strings.NewReader(createBody))
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	req.Header.Set("X-CSRF-Token", "csrf-admin")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("admin POST /api/admin/databases status = %d, mau %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-db-password") {
		t.Fatalf("response membocorkan password database: %s", rec.Body.String())
	}

	// 5. Admin updates database connection with empty password -> preserves existing password
	updateBody := `{"name":"Orders Production DB","host":"10.0.0.12","port":5432,"databaseName":"orders","username":"orders_user","password":"","sslMode":"require","description":"Updated"}`
	req = httptest.NewRequest(http.MethodPut, "/api/admin/databases/orders-db", strings.NewReader(updateBody))
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	req.Header.Set("X-CSRF-Token", "csrf-admin")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin PUT /api/admin/databases/orders-db status = %d, mau %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if h.store.items["orders-db"].Password != "secret-db-password" {
		t.Fatalf("password lama tidak dipertahankan saat update kosong: %q", h.store.items["orders-db"].Password)
	}

	// Seed schema, table details, and rows (including formatted NaN/Inf) in stub viewer
	h.viewer.schemas["orders-db"] = []SchemaSummary{
		{
			Name:   "public",
			Tables: []TableSummary{{Name: "customers", Type: "BASE TABLE", EstimatedRows: 2}},
		},
	}
	h.viewer.details["orders-db:public.customers"] = TableDetails{
		Schema: "public",
		Name:   "customers",
		Type:   "BASE TABLE",
		Columns: []ColumnInfo{
			{Name: "id", DataType: "bigint", IsNullable: false, OrdinalPosition: 1, IsPrimaryKey: true},
			{Name: "email", DataType: "text", IsNullable: false, OrdinalPosition: 2, IsPrimaryKey: false},
		},
		Constraints: []ConstraintInfo{
			{Name: "customers_pkey", Type: "PRIMARY KEY", Columns: []string{"id"}, Definition: "PRIMARY KEY (id)"},
		},
		Indexes: []IndexInfo{
			{Name: "customers_pkey", IsUnique: true, IsPrimary: true, Columns: []string{"id"}, Definition: "CREATE UNIQUE INDEX customers_pkey ON public.customers USING btree (id)"},
			{Name: "customers_email_id_idx", IsUnique: false, IsPrimary: false, Columns: []string{"email", "id"}, Definition: "CREATE INDEX customers_email_id_idx ON public.customers USING btree (email, id)"},
		},
	}
	h.viewer.rowsPages["orders-db:public.customers"] = TableRowsPage{
		Schema:  "public",
		Table:   "customers",
		Columns: []DataColumn{{Name: "id", DatabaseType: "INT8"}, {Name: "email", DatabaseType: "TEXT"}, {Name: "score", DatabaseType: "FLOAT8"}},
		Rows: []map[string]any{
			{"id": 1, "email": "a@example.com", "score": formatCellValue(math.NaN())},
			{"id": 2, "email": "b@example.com", "score": formatCellValue(math.Inf(1))},
		},
		TotalRows: 2,
	}

	// 6. Platform Admin WITHOUT group grant cannot view database schema/data (403)
	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("Platform Admin tanpa izin grup seharusnya ditolak (403), dapat %d", rec.Code)
	}

	// 7. Outsider user without group grant sees empty list on GET /api/databases and 403 on direct access
	req = httptest.NewRequest(http.MethodGet, "/api/databases", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-outsider"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("outsider GET /api/databases status = %d, mau 200", rec.Code)
	}
	var outsiderList struct {
		Databases []AccessibleDatabase `json:"databases"`
	}
	if err := json.NewDecoder(bytes.NewReader(rec.Body.Bytes())).Decode(&outsiderList); err != nil {
		t.Fatal(err)
	}
	if len(outsiderList.Databases) != 0 {
		t.Fatalf("outsider seharusnya melihat 0 database, dapat %d", len(outsiderList.Databases))
	}

	// 8. Viewer user with view_only grant sees orders-db and can inspect summary, schemas, table details, and rows
	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("viewer GET /api/databases/orders-db status = %d, mau 200: %s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db/schemas/public/tables/customers/rows?limit=10&offset=0", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"NaN"`) || !strings.Contains(rec.Body.String(), `"Infinity"`) {
		t.Fatalf("viewer GET table rows gagal mengenkode float non-finite: code=%d body=%s", rec.Code, rec.Body.String())
	}

	// 9. Missing table returns 404 Not Found
	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db/schemas/public/tables/missing_table", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing table status = %d, mau %d", rec.Code, http.StatusNotFound)
	}

	// 10. Target database connection failure returns 502 Bad Gateway on BOTH summary and schemas, with password & DSN redacted
	h.viewer.failByID["orders-db"] = errors.New("parse \"postgres://orders_user:secret-db-password@10.0.0.12:5432/orders\": dial refused")
	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("offline summary status = %d, mau %d: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-db-password") || strings.Contains(rec.Body.String(), "postgres://") {
		t.Fatalf("offline summary membocorkan password/DSN: %s", rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("connection failure status = %d, mau %d: %s", rec.Code, http.StatusBadGateway, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "secret-db-password") || strings.Contains(rec.Body.String(), "postgres://") {
		t.Fatalf("error koneksi membocorkan password/DSN: %s", rec.Body.String())
	}
	delete(h.viewer.failByID, "orders-db")

	// 11. If grant cleanup fails during deletion, DELETE returns 500 and keeps the database connection intact
	h.failRevoke = errors.New("db error revoking grants")
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/databases/orders-db", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	req.Header.Set("X-CSRF-Token", "csrf-admin")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("failed grant cleanup DELETE status = %d, mau %d", rec.Code, http.StatusInternalServerError)
	}
	if _, err := h.store.GetConnection(context.Background(), "orders-db"); err != nil {
		t.Fatalf("koneksi seharusnya belum terhapus saat pencabutan grant gagal: %v", err)
	}
	h.failRevoke = nil

	// 12. Admin deletes database connection -> removes grants and connection; recreating same ID does not inherit old access
	req = httptest.NewRequest(http.MethodDelete, "/api/admin/databases/orders-db", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	req.Header.Set("X-CSRF-Token", "csrf-admin")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("admin DELETE /api/admin/databases/orders-db status = %d, mau %d", rec.Code, http.StatusNoContent)
	}

	req = httptest.NewRequest(http.MethodPost, "/api/admin/databases", strings.NewReader(createBody))
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-admin"})
	req.Header.Set("X-CSRF-Token", "csrf-admin")
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("recreate orders-db status = %d, mau 201", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/databases/orders-db/schemas", nil)
	req.AddCookie(&http.Cookie{Name: "if3110_session", Value: "tok-viewer"})
	rec = httptest.NewRecorder()
	h.handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("reused ID seharusnya tidak mewarisi akses lama, dapat status %d", rec.Code)
	}
}
