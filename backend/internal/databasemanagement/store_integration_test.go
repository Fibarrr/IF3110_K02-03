package databasemanagement

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/lib/pq"
)

func TestPostgresDatabaseManagementIntegration(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("isi TEST_DATABASE_URL untuk menjalankan tes integrasi PostgreSQL")
	}

	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	host := parsedURL.Hostname()
	port := 5432
	if p := parsedURL.Port(); p != "" {
		if parsedPort, err := strconv.Atoi(p); err == nil {
			port = parsedPort
		}
	}
	dbName := strings.TrimPrefix(parsedURL.Path, "/")
	username := ""
	password := ""
	if parsedURL.User != nil {
		username = parsedURL.User.Username()
		password, _ = parsedURL.User.Password()
	}
	sslMode := parsedURL.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "disable"
	}

	ctx := context.Background()
	adminDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()

	var randBytes [4]byte
	if _, err := rand.Read(randBytes[:]); err != nil {
		t.Fatal(err)
	}
	suffix := hex.EncodeToString(randBytes[:])
	schemaName := "dm_test_" + suffix
	connID := "pg-test-" + suffix

	if _, err := adminDB.ExecContext(ctx, "CREATE SCHEMA "+pq.QuoteIdentifier(schemaName)); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminDB.ExecContext(ctx, "DROP SCHEMA "+pq.QuoteIdentifier(schemaName)+" CASCADE")
	}()

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()

	if _, err := db.ExecContext(ctx, "SET search_path TO "+pq.QuoteIdentifier(schemaName)); err != nil {
		t.Fatal(err)
	}

	acStore := accesscontrol.NewStore(db)
	if err := acStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	dmStore := NewStore(db)
	if err := dmStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	viewer := NewViewer(dmStore)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE sample_items (
		    id SERIAL PRIMARY KEY,
		    label TEXT NOT NULL UNIQUE,
		    ratio DOUBLE PRECISION NOT NULL DEFAULT 'NaN'
		)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE INDEX sample_items_label_id_idx ON sample_items (label, id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO sample_items (label, ratio) VALUES ('item-a', 'NaN')`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE sample_links (item_id INTEGER REFERENCES sample_items(id), note TEXT)`); err != nil {
		t.Fatal(err)
	}

	created, err := dmStore.CreateConnection(ctx, ConnectionInput{
		ID:           connID,
		Name:         "Isolated PG Test " + suffix,
		Host:         host,
		Port:         port,
		DatabaseName: dbName,
		Username:     username,
		Password:     password,
		SSLMode:      sslMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.ID != connID {
		t.Fatalf("created.ID = %q, mau %q", created.ID, connID)
	}

	status, err := viewer.PingConnection(ctx, connID)
	if err != nil || !status.Connected {
		t.Fatalf("PingConnection gagal: status=%#v err=%v", status, err)
	}

	schemas, err := viewer.ListSchemas(ctx, connID)
	if err != nil || len(schemas) == 0 {
		t.Fatalf("ListSchemas gagal: schemas=%#v err=%v", schemas, err)
	}

	details, err := viewer.GetTableDetails(ctx, connID, schemaName, "sample_items")
	if err != nil || len(details.Columns) != 3 || len(details.Constraints) < 2 || len(details.Indexes) < 2 {
		t.Fatalf("GetTableDetails tidak lengkap: details=%#v err=%v", details, err)
	}
	linkDetails, err := viewer.GetTableDetails(ctx, connID, schemaName, "sample_links")
	if err != nil || len(linkDetails.Constraints) != 1 || linkDetails.Constraints[0].ReferencedSchema != schemaName || linkDetails.Constraints[0].ReferencedTable != "sample_items" || !reflect.DeepEqual(linkDetails.Constraints[0].ReferencedColumns, []string{"id"}) {
		t.Fatalf("metadata foreign key untuk visualizer salah: details=%#v err=%v", linkDetails, err)
	}

	var foundPrimaryCols, foundCompositeCols []string
	for _, idx := range details.Indexes {
		if idx.Name == "sample_items_pkey" {
			foundPrimaryCols = idx.Columns
		}
		if idx.Name == "sample_items_label_id_idx" {
			foundCompositeCols = idx.Columns
		}
	}
	if !reflect.DeepEqual(foundPrimaryCols, []string{"id"}) {
		t.Fatalf("kolom index tunggal = %#v, mau [id]", foundPrimaryCols)
	}
	if !reflect.DeepEqual(foundCompositeCols, []string{"label", "id"}) {
		t.Fatalf("kolom index komposit = %#v, mau [label id]", foundCompositeCols)
	}

	rowsPage, err := viewer.GetTableRows(ctx, connID, schemaName, "sample_items", 10, 0)
	if err != nil || rowsPage.TotalRows != 1 || len(rowsPage.Rows) != 1 || rowsPage.Rows[0]["ratio"] != "NaN" {
		t.Fatalf("GetTableRows gagal: page=%#v err=%v", rowsPage, err)
	}

	// Verify that DeleteConnection atomically removes grants in PostgreSQL
	user, err := acStore.CreateUser(ctx, "viewer_"+suffix, "password-kuat-123")
	if err != nil {
		t.Fatal(err)
	}
	group, err := acStore.CreateGroup(ctx, "grp_"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if err := acStore.AddMember(ctx, group.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := acStore.SetDatabaseAccess(ctx, group.ID, connID, accesscontrol.AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if err := dmStore.DeleteConnection(ctx, connID); err != nil {
		t.Fatal(err)
	}
	if access, err := acStore.EffectiveAccess(ctx, user.ID, connID); err != nil || access != accesscontrol.AccessNone {
		t.Fatalf("grant seharusnya terhapus bersama koneksi, dapat access=%q err=%v", access, err)
	}

	sessions := accesscontrol.NewSessionManager(db, accesscontrol.DefaultSessionPolicy())
	acHandler := accesscontrol.NewHTTPHandler(acStore, sessions, false)
	dmHandler := NewHTTPHandler(dmStore, viewer, acHandler, acStore)

	mux := http.NewServeMux()
	dmHandler.RegisterRoutes(mux)
	mux.Handle("/api/", acHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/databases/"+connID+"/schemas", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("tanpa login status = %d, mau %d", rec.Code, http.StatusUnauthorized)
	}
}
