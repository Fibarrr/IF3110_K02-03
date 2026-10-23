package accesscontrol

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPostgresAccessControlFlow(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("isi TEST_DATABASE_URL untuk menjalankan tes PostgreSQL")
	}

	ctx := context.Background()
	adminDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()

	suffix, err := randomToken()
	if err != nil {
		t.Fatal(err)
	}
	schemaName := "ac_test_" + strings.ToLower(strings.ReplaceAll(suffix[:8], "-", "a"))
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA "`+schemaName+`"`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = adminDB.ExecContext(ctx, `DROP SCHEMA "`+schemaName+`" CASCADE`)
	}()

	db, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	defer db.Close()
	if err := db.PingContext(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `SET search_path TO "`+schemaName+`"`); err != nil {
		t.Fatal(err)
	}

	store := NewStore(db)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := store.BootstrapAdmin(ctx, "platform-admin", "admin-password-123"); err != nil {
		t.Fatal(err)
	}
	admin, err := store.VerifyCredentials(ctx, "PLATFORM-ADMIN", "admin-password-123")
	if err != nil || !admin.PlatformAdmin {
		t.Fatalf("Platform Admin login failed: user=%#v err=%v", admin, err)
	}
	if _, err := store.VerifyCredentials(ctx, "platform-admin", "wrong-password-123"); err == nil {
		t.Fatal("wrong password was accepted")
	}

	user, err := store.CreateUser(ctx, "member-1", "member-password-123")
	if err != nil {
		t.Fatal(err)
	}
	viewGroup, err := store.CreateGroup(ctx, "viewers")
	if err != nil {
		t.Fatal(err)
	}
	editGroup, err := store.CreateGroup(ctx, "editors")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.AddMember(ctx, viewGroup.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDatabaseAccess(ctx, viewGroup.ID, "db-1", AccessViewOnly); err != nil {
		t.Fatal(err)
	}
	if got, err := store.EffectiveAccess(ctx, user.ID, "db-1"); err != nil || got != AccessViewOnly {
		t.Fatalf("view access = %q, err=%v", got, err)
	}
	if err := store.AddMember(ctx, editGroup.ID, user.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDatabaseAccess(ctx, editGroup.ID, "db-1", AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	if got, err := store.EffectiveAccess(ctx, user.ID, "db-1"); err != nil || got != AccessViewEdit {
		t.Fatalf("edit access should win, got %q err=%v", got, err)
	}

	policy := DefaultSessionPolicy()
	policy.RotateEvery = -time.Second
	sessions := NewSessionManager(db, policy)
	token, _, _, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	var expiryBefore time.Time
	if err := db.QueryRowContext(ctx, `SELECT expires_at FROM ac_sessions WHERE token_hash = $1`, hashToken(token)).Scan(&expiryBefore); err != nil {
		t.Fatal(err)
	}
	rotated, err := sessions.Load(ctx, token)
	if err != nil || rotated.RotatedToken == "" {
		t.Fatalf("session did not rotate: err=%v", err)
	}
	var expiryAfter time.Time
	if err := db.QueryRowContext(ctx, `SELECT expires_at FROM ac_sessions WHERE token_hash = $1`, hashToken(rotated.RotatedToken)).Scan(&expiryAfter); err != nil {
		t.Fatal(err)
	}
	if !expiryBefore.Equal(expiryAfter) {
		t.Fatal("rotasi memperpanjang batas umur sesi")
	}
	if _, err := sessions.Load(ctx, token); err != nil {
		t.Fatalf("old token should work during grace period: %v", err)
	}
	if _, err := sessions.Load(ctx, rotated.RotatedToken); err != nil {
		t.Fatalf("rotated token should work: %v", err)
	}

	handler := NewHTTPHandler(store, sessions, false)
	request := httptest.NewRequest("GET", "/api/admin/users", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: rotated.RotatedToken})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("regular user admin status = %d, mau %d", response.Code, http.StatusForbidden)
	}

	adminToken, _, _, err := sessions.Create(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("GET", "/api/admin/users", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminToken})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("Platform Admin user list status = %d, mau %d", response.Code, http.StatusOK)
	}

	adminToken, csrfToken, _, err := sessions.Create(ctx, admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest("POST", "http://app.local/api/admin/groups", strings.NewReader(`{"name":"api-group"}`))
	request.Header.Set("Origin", "http://app.local")
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminToken})
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfToken})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("admin mutation without CSRF status = %d, mau %d", response.Code, http.StatusForbidden)
	}

	request = httptest.NewRequest("POST", "http://app.local/api/admin/groups", strings.NewReader(`{"name":"api-group"}`))
	request.Header.Set("Origin", "http://app.local")
	request.Header.Set("X-CSRF-Token", csrfToken)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: adminToken})
	request.AddCookie(&http.Cookie{Name: csrfCookieName, Value: csrfToken})
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("admin mutation with CSRF status = %d, mau %d: %s", response.Code, http.StatusCreated, response.Body.String())
	}

	userToken, _, _, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	protected := handler.RequireDatabaseAccess(func(*http.Request) string { return "db-2" }, AccessViewOnly, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request = httptest.NewRequest("GET", "/protected", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: userToken})
	response = httptest.NewRecorder()
	protected.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("request without database access status = %d, mau %d", response.Code, http.StatusForbidden)
	}

	protected = handler.RequireDatabaseAccess(func(*http.Request) string { return "db-1" }, AccessViewEdit, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request = httptest.NewRequest("GET", "/protected", nil)
	request.AddCookie(&http.Cookie{Name: sessionCookieName, Value: userToken})
	response = httptest.NewRecorder()
	protected.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("request with database access status = %d, mau %d", response.Code, http.StatusNoContent)
	}

	if err := sessions.Revoke(ctx, rotated.RotatedToken); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Load(ctx, rotated.RotatedToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session should be rejected, got %v", err)
	}

	idleToken, _, _, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE ac_sessions SET last_seen_at = NOW() - INTERVAL '31 minutes' WHERE token_hash = $1`, hashToken(idleToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Load(ctx, idleToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("idle session should expire, got %v", err)
	}

	absoluteToken, _, _, err := sessions.Create(ctx, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE ac_sessions SET expires_at = NOW() - INTERVAL '1 second' WHERE token_hash = $1`, hashToken(absoluteToken)); err != nil {
		t.Fatal(err)
	}
	if _, err := sessions.Load(ctx, absoluteToken); !errors.Is(err, ErrNotFound) {
		t.Fatalf("absolute-expired session should expire, got %v", err)
	}

	if err := store.SetUserDisabled(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := store.VerifyCredentials(ctx, "member-1", "member-password-123"); err == nil {
		t.Fatal("disabled user was able to log in")
	}
}
