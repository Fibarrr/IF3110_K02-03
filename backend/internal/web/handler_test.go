package web

import (
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebHandlerServesSPAAndBuiltAssets(t *testing.T) {
	handler, err := NewHandler()
	if err != nil {
		t.Fatalf("NewHandler() gagal: %v", err)
	}

	spaRoutes := []string{"/", "/login", "/admin", "/databases/orders-db"}
	for _, route := range spaRoutes {
		req := httptest.NewRequest(http.MethodGet, route, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `<div id="root"></div>`) {
			t.Fatalf("GET %s status=%d, expected SPA index.html", route, rec.Code)
		}
	}

	// Verify at least one built JS asset exists and is served with 200 OK.
	entries, err := fs.ReadDir(handler.distFS, "assets")
	if err != nil || len(entries) == 0 {
		t.Fatalf("expected built SPA assets in dist/assets: %v", err)
	}
	assetPath := "/assets/" + entries[0].Name()
	req := httptest.NewRequest(http.MethodGet, assetPath, nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() == 0 {
		t.Fatalf("GET %s status=%d len=%d", assetPath, rec.Code, rec.Body.Len())
	}

	// Missing asset returns 404 rather than HTML fallback.
	req = httptest.NewRequest(http.MethodGet, "/assets/does-not-exist.js", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET missing asset status=%d, want 404", rec.Code)
	}
}
