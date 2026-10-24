package databasemanagement

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestNormalizeDatabaseID(t *testing.T) {
	tests := []struct {
		name    string
		id      string
		dbName  string
		want    string
		wantErr bool
	}{
		{name: "explicit valid id", id: "  DB-Prod_01.main ", want: "db-prod_01.main"},
		{name: "derived slug from display name", id: "", dbName: "Orders Production DB!", want: "orders-production-db"},
		{name: "invalid characters in explicit id", id: "db prod/1", wantErr: true},
		{name: "empty id and name", id: "", dbName: "   ", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizeDatabaseID(tc.id, tc.dbName)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("normalizeDatabaseID(%q, %q) = %q, seharusnya gagal", tc.id, tc.dbName, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizeDatabaseID(%q, %q) gagal: %v", tc.id, tc.dbName, err)
			}
			if got != tc.want {
				t.Fatalf("normalizeDatabaseID(%q, %q) = %q, mau %q", tc.id, tc.dbName, got, tc.want)
			}
		})
	}
}

func TestValidateConnectionInput(t *testing.T) {
	valid := ConnectionInput{
		ID:           "analytics-db",
		Name:         "Analytics DB",
		Host:         "localhost",
		Port:         5432,
		DatabaseName: "analytics",
		Username:     "readonly_user",
		Password:     "rahasia-sangat-kuat",
		SSLMode:      "",
	}
	normalized, err := validateConnectionInput(valid, true)
	if err != nil {
		t.Fatalf("validateConnectionInput() gagal pada input valid: %v", err)
	}
	if normalized.SSLMode != "disable" {
		t.Fatalf("SSLMode default = %q, mau %q", normalized.SSLMode, "disable")
	}

	ipv6Valid := valid
	ipv6Valid.Host = "::1"
	if _, err := validateConnectionInput(ipv6Valid, true); err != nil {
		t.Fatalf("IPv6 valid ::1 seharusnya diterima: %v", err)
	}

	invalidCases := []struct {
		name   string
		mutate func(*ConnectionInput)
	}{
		{name: "missing name", mutate: func(in *ConnectionInput) { in.Name = " " }},
		{name: "missing host", mutate: func(in *ConnectionInput) { in.Host = "" }},
		{name: "malformed bracket host", mutate: func(in *ConnectionInput) { in.Host = "[" }},
		{name: "malformed host with semicolon", mutate: func(in *ConnectionInput) { in.Host = "db;drop" }},
		{name: "invalid port high", mutate: func(in *ConnectionInput) { in.Port = 70000 }},
		{name: "invalid port negative", mutate: func(in *ConnectionInput) { in.Port = -1 }},
		{name: "missing database name", mutate: func(in *ConnectionInput) { in.DatabaseName = "" }},
		{name: "missing username", mutate: func(in *ConnectionInput) { in.Username = "" }},
		{name: "invalid ssl mode", mutate: func(in *ConnectionInput) { in.SSLMode = "prefer" }},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			in := valid
			tc.mutate(&in)
			if _, err := validateConnectionInput(in, true); err == nil {
				t.Fatalf("kasus %s seharusnya ditolak", tc.name)
			}
		})
	}
}

func TestBuildPostgresDSNEscapesCredentialsAndRedactsPasswordAndDSN(t *testing.T) {
	cfg := storedConnection{
		DatabaseConnection: DatabaseConnection{
			ID:           "db-1",
			Name:         "Primary DB",
			Host:         "db.internal",
			Port:         5432,
			DatabaseName: "app_db",
			Username:     "app_user",
			SSLMode:      "require",
			HasPassword:  true,
		},
		Password: "p@ss:w/rd?secret#1",
	}

	dsn := buildPostgresDSN(cfg)
	if !strings.Contains(dsn, "sslmode=require") || !strings.Contains(dsn, "connect_timeout=5") {
		t.Fatalf("DSN tidak memuat parameter yang diharapkan: %s", dsn)
	}

	rawJSON, err := json.Marshal(cfg.DatabaseConnection)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(rawJSON), cfg.Password) || strings.Contains(string(rawJSON), "password") {
		t.Fatalf("JSON DatabaseConnection membocorkan password: %s", string(rawJSON))
	}

	rawErr := errors.New("parse \"" + dsn + "\": invalid URL escape with password " + cfg.Password)
	sanitized := sanitizeConnectionError(rawErr, cfg.Password)
	if strings.Contains(sanitized, cfg.Password) || strings.Contains(sanitized, "p%40ss") || strings.Contains(sanitized, "postgres://") || strings.Contains(sanitized, "app_user") {
		t.Fatalf("sanitizeConnectionError masih membocorkan detail DSN/password: %s", sanitized)
	}
}

func TestTargetSlotBoundsConcurrency(t *testing.T) {
	store := NewStore(nil)
	for i := 0; i < maxConcurrentTargetOps; i++ {
		release, err := store.acquireTargetSlot(context.Background())
		if err != nil {
			t.Fatalf("acquire slot %d gagal: %v", i, err)
		}
		defer release()
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := store.acquireTargetSlot(ctx); err == nil {
		t.Fatal("acquireTargetSlot seharusnya gagal saat batas konkurensi penuh")
	}
}
