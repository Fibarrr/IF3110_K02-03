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
		path        string
		wantStatus  int
		wantSnippet string
	}{
		{path: "/login", wantStatus: http.StatusOK, wantSnippet: "Masuk ke Platform"},
		{path: "/", wantStatus: http.StatusOK, wantSnippet: "Pilih Database"},
		{path: "/databases/orders-db", wantStatus: http.StatusOK, wantSnippet: "orders-db"},
		{path: "/admin", wantStatus: http.StatusServiceUnavailable, wantSnippet: "akses admin belum dikonfigurasi"},
		{path: "/static/styles.css", wantStatus: http.StatusOK, wantSnippet: ":root"},
		{path: "/static/app.js", wantStatus: http.StatusOK, wantSnippet: "X-CSRF-Token"},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus || !strings.Contains(rec.Body.String(), tc.wantSnippet) {
			t.Fatalf("GET %s status=%d (mau %d), snippet %q", tc.path, rec.Code, tc.wantStatus, tc.wantSnippet)
		}
	}
}
