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
		body       string
		access     accesscontrol.DatabaseAccess
		csrf       bool
		wantStatus int
	}{
		{
			name:       "no access cannot read history",
			method:     http.MethodGet,
			path:       "/api/databases/db1/migrations",
			access:     accesscontrol.AccessNone,
			csrf:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "view only cannot create migration",
			method:     http.MethodPost,
			path:       "/api/databases/db1/migrations",
			body:       `{"name":"m1","upSQL":"SELECT 1","downSQL":"SELECT 1"}`,
			access:     accesscontrol.AccessViewOnly,
			csrf:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "edit still needs csrf",
			method:     http.MethodPost,
			path:       "/api/databases/db1/migrations",
			body:       `{"name":"m1","upSQL":"SELECT 1","downSQL":"SELECT 1"}`,
			access:     accesscontrol.AccessViewEdit,
			csrf:       false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "view only cannot submit review request",
			method:     http.MethodPost,
			path:       "/api/databases/db1/migrations/1/review-requests",
			body:       `{"operation":"apply"}`,
			access:     accesscontrol.AccessViewOnly,
			csrf:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "view only cannot queue review request",
			method:     http.MethodPost,
			path:       "/api/databases/db1/review-requests/1/queue",
			access:     accesscontrol.AccessViewOnly,
			csrf:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "view only leader approval still needs csrf",
			method:     http.MethodPut,
			path:       "/api/databases/db1/review-requests/1/approvals/2",
			body:       `{"decision":"approve"}`,
			access:     accesscontrol.AccessViewOnly,
			csrf:       false,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "no access cannot approve review request",
			method:     http.MethodPut,
			path:       "/api/databases/db1/review-requests/1/approvals/2",
			body:       `{"decision":"approve"}`,
			access:     accesscontrol.AccessNone,
			csrf:       true,
			wantStatus: http.StatusForbidden,
		},
		{
			name:       "legacy direct execute returns 409 conflict",
			method:     http.MethodPost,
			path:       "/api/databases/db1/migrations/1/execute",
			access:     accesscontrol.AccessViewEdit,
			csrf:       true,
			wantStatus: http.StatusConflict,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			handler := NewHTTPHandler(nil, testAuthGuard{access: test.access, csrf: test.csrf})
			req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d (%s)", rec.Code, test.wantStatus, rec.Body.String())
			}
		})
	}
}
