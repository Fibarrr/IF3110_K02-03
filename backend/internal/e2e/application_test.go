package e2e

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/migration"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/web"
	"github.com/lib/pq"
)

// End-to-end test: menguji login, sesi, izin, viewer, dan migration melalui HTTP
// dengan PostgreSQL sungguhan. TEST_DATABASE_URL harus menunjuk ke database lokal.
func TestApplicationUserFlow(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("isi TEST_DATABASE_URL untuk menjalankan end-to-end test PostgreSQL")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	parsedURL, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatalf("TEST_DATABASE_URL tidak valid: %v", err)
	}
	adminDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := adminDB.PingContext(ctx); err != nil {
		adminDB.Close()
		t.Fatalf("gagal terhubung ke TEST_DATABASE_URL: %v", err)
	}

	var suffix string
	var randomBytes [6]byte
	if _, err := rand.Read(randomBytes[:]); err != nil {
		t.Fatal(err)
	}
	suffix = hex.EncodeToString(randomBytes[:])
	platformSchema := "e2e_platform_" + suffix
	targetSchema := "e2e_target_" + suffix
	databaseID := "e2e-" + suffix

	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(platformSchema)); err != nil {
		adminDB.Close()
		t.Fatalf("gagal membuat schema platform sementara: %v", err)
	}
	targetSchemaCreated := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if targetSchemaCreated {
			if _, err := adminDB.ExecContext(cleanupCtx, `DROP SCHEMA `+pq.QuoteIdentifier(targetSchema)+` CASCADE`); err != nil {
				t.Errorf("gagal membersihkan schema target E2E: %v", err)
			}
		}
		if _, err := adminDB.ExecContext(cleanupCtx, `DROP SCHEMA `+pq.QuoteIdentifier(platformSchema)+` CASCADE`); err != nil {
			t.Errorf("gagal membersihkan schema platform E2E: %v", err)
		}
		_ = adminDB.Close()
	})
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(targetSchema)); err != nil {
		t.Fatalf("gagal membuat schema target sementara: %v", err)
	}
	targetSchemaCreated = true

	platformURL := *parsedURL
	query := platformURL.Query()
	query.Set("options", "-c search_path="+platformSchema)
	platformURL.RawQuery = query.Encode()
	db, err := sql.Open("postgres", platformURL.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(4)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		t.Fatal(err)
	}

	accessStore := accesscontrol.NewStore(db)
	dmStore, err := databasemanagement.NewStoreWithCredentialKey(db, "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	migrationStore := migration.NewStore(db, dmStore)
	var server *httptest.Server
	var connectionCreated bool
	t.Cleanup(func() {
		if server != nil {
			server.Close()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cleanupCancel()
		if connectionCreated {
			if err := dmStore.DeleteConnection(cleanupCtx, databaseID); err != nil {
				t.Errorf("gagal membersihkan koneksi E2E: %v", err)
			}
		}
		if err := dmStore.Close(); err != nil {
			t.Errorf("gagal menutup pool database E2E: %v", err)
		}
		_ = db.Close()
	})

	if err := accessStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dmStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := migrationStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := accessStore.BootstrapAdmin(ctx, "e2e-admin", "e2e-admin-password-123"); err != nil {
		t.Fatal(err)
	}

	if _, err := adminDB.ExecContext(ctx, `CREATE TABLE `+pq.QuoteIdentifier(targetSchema)+`.items (id BIGINT PRIMARY KEY, label TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := adminDB.ExecContext(ctx, `INSERT INTO `+pq.QuoteIdentifier(targetSchema)+`.items (id, label) VALUES (1, 'first item')`); err != nil {
		t.Fatal(err)
	}

	acHandler := accesscontrol.NewHTTPHandler(accessStore, accesscontrol.NewSessionManager(db, accesscontrol.DefaultSessionPolicy()), false)
	dmViewer := databasemanagement.NewViewer(dmStore)
	dmHandler := databasemanagement.NewHTTPHandler(dmStore, dmViewer, acHandler, accessStore)
	migrationHandler := migration.NewHTTPHandler(migrationStore, acHandler)
	apiMux := http.NewServeMux()
	dmHandler.RegisterRoutes(apiMux)
	migrationHandler.RegisterRoutes(apiMux)
	apiMux.Handle("/api/", acHandler)
	appMux := http.NewServeMux()
	appMux.Handle("/api/", apiMux)
	webHandler, err := web.NewHandler(acHandler)
	if err != nil {
		t.Fatal(err)
	}
	webHandler.RegisterRoutes(appMux, nil, acHandler)
	server = httptest.NewServer(appMux)

	adminClient := testClient(t)
	adminCSRF := login(t, adminClient, server.URL, "e2e-admin", "e2e-admin-password-123")
	memberCreate := doRequest(t, adminClient, server.URL, http.MethodPost, "/api/admin/users", map[string]string{
		"username": "e2e-member-" + suffix,
		"password": "e2e-member-password-123",
	}, adminCSRF)
	requireStatus(t, memberCreate, http.StatusCreated)
	member := decodeJSON[accesscontrol.User](t, memberCreate.body)

	groupCreate := doRequest(t, adminClient, server.URL, http.MethodPost, "/api/admin/groups", map[string]string{"name": "E2E group " + suffix}, adminCSRF)
	requireStatus(t, groupCreate, http.StatusCreated)
	group := decodeJSON[accesscontrol.Group](t, groupCreate.body)
	requireStatus(t, doRequest(t, adminClient, server.URL, http.MethodPut,
		fmt.Sprintf("/api/admin/groups/%d/members/%d", group.ID, member.ID), nil, adminCSRF), http.StatusNoContent)

	port := 5432
	if parsedURL.Port() != "" {
		parsedPort, parseErr := strconv.Atoi(parsedURL.Port())
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		port = parsedPort
	}
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
	connectionInput := databasemanagement.ConnectionInput{
		ID: databaseID, Name: "E2E Database " + suffix,
		Host: parsedURL.Hostname(), Port: port,
		DatabaseName: strings.TrimPrefix(parsedURL.Path, "/"),
		Username:     username, Password: password, SSLMode: sslMode,
	}
	connectionCreate := doRequest(t, adminClient, server.URL, http.MethodPost, "/api/admin/databases", connectionInput, adminCSRF)
	requireStatus(t, connectionCreate, http.StatusCreated)
	connectionCreated = true
	connectionTest := doRequest(t, adminClient, server.URL, http.MethodPost, "/api/admin/databases/"+databaseID+"/test", nil, adminCSRF)
	requireStatus(t, connectionTest, http.StatusOK)
	requireStatus(t, doRequest(t, adminClient, server.URL, http.MethodPut,
		fmt.Sprintf("/api/admin/groups/%d/databases/%s/access", group.ID, databaseID), map[string]string{"access": string(accesscontrol.AccessViewOnly)}, adminCSRF), http.StatusNoContent)

	memberClient := testClient(t)
	memberCSRF := login(t, memberClient, server.URL, "e2e-member-"+suffix, "e2e-member-password-123")
	adminPage := doRequest(t, memberClient, server.URL, http.MethodGet, "/admin", nil, "")
	requireStatus(t, adminPage, http.StatusForbidden)
	if !strings.Contains(string(adminPage.body), "hanya dapat diakses oleh Platform Admin") {
		t.Fatalf("halaman admin tidak menampilkan pesan akses terbatas: %s", adminPage.body)
	}
	accessibleDBs := doRequest(t, memberClient, server.URL, http.MethodGet, "/api/databases", nil, "")
	requireStatus(t, accessibleDBs, http.StatusOK)
	if !strings.Contains(string(accessibleDBs.body), databaseID) {
		t.Fatalf("database yang diberi grant tidak muncul: %s", accessibleDBs.body)
	}
	schemas := doRequest(t, memberClient, server.URL, http.MethodGet, "/api/databases/"+databaseID+"/schemas", nil, "")
	requireStatus(t, schemas, http.StatusOK)
	if !strings.Contains(string(schemas.body), targetSchema) {
		t.Fatalf("schema target tidak terlihat: %s", schemas.body)
	}
	tableDetailsPath := "/api/databases/" + databaseID + "/schemas/" + targetSchema + "/tables/items"
	details := doRequest(t, memberClient, server.URL, http.MethodGet, tableDetailsPath, nil, "")
	requireStatus(t, details, http.StatusOK)
	if !strings.Contains(string(details.body), `"name":"label"`) {
		t.Fatalf("metadata kolom tabel tidak sesuai: %s", details.body)
	}
	deniedDraft := doRequest(t, memberClient, server.URL, http.MethodPost, "/api/databases/"+databaseID+"/migrations", migration.Input{
		Name: "must be denied", SQL: "SELECT 1",
	}, memberCSRF)
	requireStatus(t, deniedDraft, http.StatusForbidden)

	requireStatus(t, doRequest(t, adminClient, server.URL, http.MethodPut,
		fmt.Sprintf("/api/admin/groups/%d/databases/%s/access", group.ID, databaseID), map[string]string{"access": string(accesscontrol.AccessViewEdit)}, adminCSRF), http.StatusNoContent)
	createdMigration := doRequest(t, memberClient, server.URL, http.MethodPost, "/api/databases/"+databaseID+"/migrations", migration.Input{
		Name: "add note column",
		SQL:  `ALTER TABLE ` + pq.QuoteIdentifier(targetSchema) + `.items ADD COLUMN note TEXT`,
	}, memberCSRF)
	requireStatus(t, createdMigration, http.StatusCreated)
	created := decodeJSON[migration.Migration](t, createdMigration.body)
	if created.Status != "draft" || created.Version != 1 {
		t.Fatalf("draft migration tidak sesuai: %#v", created)
	}
	executed := doRequest(t, memberClient, server.URL, http.MethodPost,
		fmt.Sprintf("/api/databases/%s/migrations/%d/execute", databaseID, created.ID), nil, memberCSRF)
	requireStatus(t, executed, http.StatusOK)
	result := decodeJSON[migration.Migration](t, executed.body)
	if result.Status != "applied" {
		t.Fatalf("migration tidak berstatus applied: %#v", result)
	}
	detailsAfterMigration := doRequest(t, memberClient, server.URL, http.MethodGet, tableDetailsPath, nil, "")
	requireStatus(t, detailsAfterMigration, http.StatusOK)
	updatedDetails := decodeJSON[databasemanagement.TableDetails](t, detailsAfterMigration.body)
	foundNote := false
	for _, column := range updatedDetails.Columns {
		if column.Name == "note" {
			foundNote = true
		}
	}
	if !foundNote {
		t.Fatalf("kolom hasil migration tidak terlihat di viewer: %#v", updatedDetails.Columns)
	}

	requireStatus(t, doRequest(t, memberClient, server.URL, http.MethodPost, "/api/auth/logout", nil, memberCSRF), http.StatusNoContent)
	requireStatus(t, doRequest(t, memberClient, server.URL, http.MethodGet, "/api/auth/me", nil, ""), http.StatusUnauthorized)
}

type httpResult struct {
	status int
	body   []byte
}

func doRequest(t *testing.T, client *http.Client, baseURL, method, path string, body any, csrf string) httpResult {
	t.Helper()
	var requestBody io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		requestBody = strings.NewReader(string(encoded))
	}
	request, err := http.NewRequest(method, baseURL+path, requestBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", baseURL)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if csrf != "" {
		request.Header.Set("X-CSRF-Token", csrf)
	}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("request %s %s gagal: %v", method, path, err)
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return httpResult{status: response.StatusCode, body: responseBody}
}

func login(t *testing.T, client *http.Client, baseURL, username, password string) string {
	t.Helper()
	result := doRequest(t, client, baseURL, http.MethodPost, "/api/auth/login", map[string]string{
		"username": username,
		"password": password,
	}, "")
	if result.status != http.StatusOK {
		t.Fatalf("login %q status=%d body=%s", username, result.status, result.body)
	}
	var payload struct {
		CSRFToken string `json:"csrfToken"`
	}
	if err := json.Unmarshal(result.body, &payload); err != nil || payload.CSRFToken == "" {
		t.Fatalf("respons login tidak memuat CSRF token: body=%s err=%v", result.body, err)
	}
	return payload.CSRFToken
}

func requireStatus(t *testing.T, result httpResult, expected int) {
	t.Helper()
	if result.status != expected {
		t.Fatalf("status=%d, ingin %d, body=%s", result.status, expected, result.body)
	}
}

func decodeJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var result T
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("gagal membaca JSON %q: %v", body, err)
	}
	return result
}

func testClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{Jar: jar, Timeout: 10 * time.Second}
}
