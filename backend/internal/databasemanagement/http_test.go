package databasemanagement

import (
	"context"
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
			if err != nil || limit != tc.wantLimit || offset != tc.wantOffset {
				t.Fatalf("normalizePagination(%q, %q) = (%d, %d, %v), mau (%d, %d, nil)",
					tc.rawLimit, tc.rawOffset, limit, offset, err, tc.wantLimit, tc.wantOffset)
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
	if got := formatCellValue(math.NaN()); got != "NaN" {
		t.Fatalf("formatCellValue(NaN) = %#v, mau NaN", got)
	}
	if got := formatCellValue(math.Inf(1)); got != "Infinity" {
		t.Fatalf("formatCellValue(+Inf) = %#v, mau Infinity", got)
	}
	if got := formatCellValue(math.Inf(-1)); got != "-Infinity" {
		t.Fatalf("formatCellValue(-Inf) = %#v, mau -Infinity", got)
	}
}

func TestSystemSchemaRejection(t *testing.T) {
	for _, sys := range []string{"pg_catalog", "information_schema", "pg_toast", "pg_temp_3"} {
		if !isSystemSchema(sys) {
			t.Fatalf("schema %q seharusnya dianggap schema sistem", sys)
		}
	}
	viewer := NewViewer(NewStore(nil))
	if _, err := viewer.GetTableDetails(context.Background(), "db-1", "pg_catalog", "pg_authid"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTableDetails pada pg_catalog = %v, mau ErrNotFound", err)
	}
	if _, err := viewer.GetTableRows(context.Background(), "db-1", "information_schema", "tables", 10, 0); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetTableRows pada information_schema = %v, mau ErrNotFound", err)
	}
}

type testAuthGuard struct {
	loggedIn bool
	admin    bool
	csrf     bool
	access   accesscontrol.DatabaseAccess
}

func (g testAuthGuard) RequireUser(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.loggedIn {
			writeError(w, http.StatusUnauthorized, "perlu login")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g testAuthGuard) RequirePlatformAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.admin {
			writeError(w, http.StatusForbidden, "khusus Platform Admin")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g testAuthGuard) RequireCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !g.csrf {
			writeError(w, http.StatusForbidden, "CSRF ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (g testAuthGuard) RequireDatabaseAccess(_ func(*http.Request) string, required accesscontrol.DatabaseAccess, next http.Handler) http.Handler {
	return g.RequireUser(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := required == accesscontrol.AccessViewOnly && g.access.CanView() ||
			required == accesscontrol.AccessViewEdit && g.access.CanEdit()
		if !allowed {
			writeError(w, http.StatusForbidden, "akses ke database ditolak")
			return
		}
		next.ServeHTTP(w, r)
	}))
}

func TestHTTPRoutesGuardAndValidation(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		body       string
		guard      testAuthGuard
		wantStatus int
	}{
		{
			name:       "unauthenticated user rejected",
			method:     http.MethodGet,
			path:       "/api/databases",
			guard:      testAuthGuard{loggedIn: false},
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "regular user cannot access admin databases",
			method:     http.MethodGet,
			path:       "/api/admin/databases",
			guard:      testAuthGuard{loggedIn: true, admin: false},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "admin mutation without CSRF rejected",
			method:     http.MethodPost,
			path:       "/api/admin/databases",
			body:       `{"name":"DB"}`,
			guard:      testAuthGuard{loggedIn: true, admin: true, csrf: false},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "admin mutation rejects invalid JSON body",
			method:     http.MethodPost,
			path:       "/api/admin/databases",
			body:       `{"unknownField":123}`,
			guard:      testAuthGuard{loggedIn: true, admin: true, csrf: true},
			wantStatus: http.StatusBadRequest,
		},
		{
			name:       "platform admin without database grant cannot view schema",
			method:     http.MethodGet,
			path:       "/api/databases/orders-db/schemas",
			guard:      testAuthGuard{loggedIn: true, admin: true, access: accesscontrol.AccessNone},
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "viewer with invalid pagination query receives 400",
			method:     http.MethodGet,
			path:       "/api/databases/orders-db/schemas/public/tables/items/rows?limit=0",
			guard:      testAuthGuard{loggedIn: true, access: accesscontrol.AccessViewOnly},
			wantStatus: http.StatusBadRequest,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			handler := NewHTTPHandler(NewStore(nil), NewViewer(NewStore(nil)), tc.guard, nil)
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, mau %d (%s)", tc.method, tc.path, rec.Code, tc.wantStatus, rec.Body.String())
			}
		})
	}
}
