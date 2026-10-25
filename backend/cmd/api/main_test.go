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
		method     string
		path       string
		wantStatus int
	}{
		{method: http.MethodGet, path: "/health", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/login", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/databases/orders-db", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/admin", wantStatus: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/static/styles.css", wantStatus: http.StatusOK},
		{method: http.MethodGet, path: "/api/admin/users", wantStatus: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/api/admin/databases", wantStatus: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/api/databases", wantStatus: http.StatusUnauthorized},
		{method: http.MethodGet, path: "/api/databases/orders-db/schemas", wantStatus: http.StatusUnauthorized},
	}

	for _, tc := range tests {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			rec := httptest.NewRecorder()
			mux.ServeHTTP(rec, req)
			if rec.Code != tc.wantStatus {
				t.Fatalf("%s %s status = %d, mau %d", tc.method, tc.path, rec.Code, tc.wantStatus)
			}
		})
	}
}
