package main

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
)

func TestBuildServerMuxRoutesAPIAndWeb(t *testing.T) {
	acHandler := accesscontrol.NewHTTPHandler(nil, nil, false)
	dmHandler := databasemanagement.NewHTTPHandler(nil, nil, acHandler, nil)

	apiMux := http.NewServeMux()
	dmHandler.RegisterRoutes(apiMux)
	apiMux.Handle("/api/", acHandler)

	mux, err := buildServerMux(apiMux, acHandler)
	if err != nil {
		t.Fatalf("buildServerMux() gagal: %v", err)
	}

	tests := []struct {
		path       string
		wantStatus int
	}{
		{path: "/health", wantStatus: http.StatusOK},
		{path: "/login", wantStatus: http.StatusOK},
		{path: "/", wantStatus: http.StatusOK},
		{path: "/admin", wantStatus: http.StatusUnauthorized},
		{path: "/static/styles.css", wantStatus: http.StatusOK},
		{path: "/api/admin/databases", wantStatus: http.StatusUnauthorized},
		{path: "/api/databases", wantStatus: http.StatusUnauthorized},
	}

	for _, tc := range tests {
		req := httptest.NewRequest(http.MethodGet, tc.path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != tc.wantStatus {
			t.Fatalf("GET %s status = %d, mau %d", tc.path, rec.Code, tc.wantStatus)
		}
	}
}
