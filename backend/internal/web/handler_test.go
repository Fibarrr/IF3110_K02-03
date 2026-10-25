package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebHandlerRendersTemplatesAndStaticAssets(t *testing.T) {
	handler, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler() gagal: %v", err)
	}

	tests := []struct {
		name        string
		path        string
		wantStatus  int
		wantType    string
		wantSnippet string
	}{
		{
			name:        "login page",
			path:        "/login",
			wantStatus:  http.StatusOK,
			wantType:    "text/html",
			wantSnippet: "Masuk ke Platform",
		},
		{
			name:        "workspace home page",
			path:        "/",
			wantStatus:  http.StatusOK,
			wantType:    "text/html",
			wantSnippet: "Pilih Database",
		},
		{
			name:        "database explorer page",
			path:        "/databases/orders-db",
			wantStatus:  http.StatusOK,
			wantType:    "text/html",
			wantSnippet: "orders-db",
		},
		{
			name:        "admin console page",
			path:        "/admin",
			wantStatus:  http.StatusServiceUnavailable,
			wantType:    "text/plain",
			wantSnippet: "akses admin belum dikonfigurasi",
		},
		{
			name:        "stylesheet asset",
			path:        "/static/styles.css",
			wantStatus:  http.StatusOK,
			wantType:    "text/css",
			wantSnippet: ":root",
		},
		{
			name:        "javascript asset",
			path:        "/static/app.js",
			wantStatus:  http.StatusOK,
			wantType:    "javascript",
			wantSnippet: "X-CSRF-Token",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Fatalf("GET %s status = %d, mau %d", tc.path, rec.Code, tc.wantStatus)
			}
			contentType := rec.Header().Get("Content-Type")
			if !strings.Contains(strings.ToLower(contentType), tc.wantType) {
				t.Fatalf("GET %s Content-Type = %q, harus memuat %q", tc.path, contentType, tc.wantType)
			}
			if !strings.Contains(rec.Body.String(), tc.wantSnippet) {
				t.Fatalf("GET %s body tidak memuat %q", tc.path, tc.wantSnippet)
			}
		})
	}
}
