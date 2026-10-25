package migration

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
)

type testAuthGuard struct {
	access accesscontrol.DatabaseAccess
	csrf   bool
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
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		allowed := required == accesscontrol.AccessViewOnly && g.access.CanView() || required == accesscontrol.AccessViewEdit && g.access.CanEdit()
		if !allowed {
			writeError(w, http.StatusForbidden, "akses ditolak")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func TestHTTPMigrationRoutesRequireExpectedAccess(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		access     accesscontrol.DatabaseAccess
		csrf       bool
		wantStatus int
	}{
		{name: "no access cannot read history", method: http.MethodGet, path: "/api/databases/db1/migrations", access: accesscontrol.AccessNone, csrf: true, wantStatus: http.StatusForbidden},
		{name: "view only cannot create migration", method: http.MethodPost, path: "/api/databases/db1/migrations", access: accesscontrol.AccessViewOnly, csrf: true, wantStatus: http.StatusForbidden},
		{name: "edit still needs csrf", method: http.MethodPost, path: "/api/databases/db1/migrations", access: accesscontrol.AccessViewEdit, csrf: false, wantStatus: http.StatusForbidden},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHTTPHandler(nil, testAuthGuard{access: test.access, csrf: test.csrf})
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(`{"name":"m1","sql":"SELECT 1"}`))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, mau %d", rec.Code, test.wantStatus)
			}
		})
	}
}
