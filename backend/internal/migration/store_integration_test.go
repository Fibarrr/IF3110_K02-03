package migration

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/lib/pq"
)

func TestPostgresMigrationLifecycle(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("isi TEST_DATABASE_URL untuk menjalankan tes integrasi PostgreSQL")
	}

	parsed, err := url.Parse(databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	port := 5432
	if parsed.Port() != "" {
		port, err = strconv.Atoi(parsed.Port())
		if err != nil {
			t.Fatal(err)
		}
	}
	databaseName := strings.TrimPrefix(parsed.Path, "/")
	username := ""
	password := ""
	if parsed.User != nil {
		username = parsed.User.Username()
		password, _ = parsed.User.Password()
	}
	sslMode := parsed.Query().Get("sslmode")
	if sslMode == "" {
		sslMode = "disable"
	}

	ctx := context.Background()
	adminDB, err := sql.Open("postgres", databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer adminDB.Close()
	if err := adminDB.PingContext(ctx); err != nil {
		t.Fatal(err)
	}

	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", random[:])
	schemaName := "mg_test_" + suffix
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(schemaName)); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = adminDB.ExecContext(ctx, `DROP SCHEMA `+pq.QuoteIdentifier(schemaName)+` CASCADE`) }()
	platformURL := *parsed
	platformQuery := platformURL.Query()
	platformQuery.Set("options", "-c search_path="+schemaName)
	platformURL.RawQuery = platformQuery.Encode()
	db, err := sql.Open("postgres", platformURL.String())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	dmStore := databasemanagement.NewStore(db)
	if err := dmStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	store := NewStore(db, dmStore)
	if err := store.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	databaseID := "mg-test-" + suffix
	_, err = dmStore.CreateConnection(ctx, databasemanagement.ConnectionInput{
		ID:           databaseID,
		Name:         "Migration test " + suffix,
		Host:         parsed.Hostname(),
		Port:         port,
		DatabaseName: databaseName,
		Username:     username,
		Password:     password,
		SSLMode:      sslMode,
	})
	if err != nil {
		t.Fatal(err)
	}
	tableName := "mg_result_" + suffix
	rollbackTable := "mg_rollback_" + suffix
	defer func() {
		target, _, openErr := dmStore.OpenTargetDB(ctx, databaseID)
		if openErr == nil {
			defer target.Close()
			_, _ = target.ExecContext(ctx, `DROP TABLE IF EXISTS public.`+pq.QuoteIdentifier(tableName)+`, public.`+pq.QuoteIdentifier(rollbackTable))
		}
		_ = dmStore.DeleteConnection(ctx, databaseID)
	}()

	first, err := store.Create(ctx, databaseID, 101, Input{Name: "create test table", SQL: `CREATE TABLE public.` + pq.QuoteIdentifier(tableName) + ` (id INT PRIMARY KEY)`})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Create(ctx, databaseID, 102, Input{Name: "broken migration", SQL: `CREATE TABLE public.` + pq.QuoteIdentifier(rollbackTable) + ` (id INT); SELECT no_such_migration_function()`})
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || second.Version != 2 {
		t.Fatalf("versions = %d, %d; mau 1, 2", first.Version, second.Version)
	}
	if _, err := store.Execute(ctx, databaseID, second.ID, 102); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("eksekusi versi kedua sebelum pertama error = %v", err)
	}
	if applied, err := store.Execute(ctx, databaseID, first.ID, 101); err != nil || applied.Status != "applied" || applied.ExecutedBy == nil || *applied.ExecutedBy != 101 {
		t.Fatalf("migration pertama gagal: migration=%#v err=%v", applied, err)
	}
	failed, err := store.Execute(ctx, databaseID, second.ID, 102)
	if !errors.Is(err, ErrExecution) || failed.Status != "failed" || failed.ExecutedBy == nil || *failed.ExecutedBy != 102 || failed.ExecutedAt == nil {
		t.Fatalf("migration rusak status=%q err=%v", failed.Status, err)
	}
	target, _, err := dmStore.OpenTargetDB(ctx, databaseID)
	if err != nil {
		t.Fatal(err)
	}
	var rolledBack sql.NullString
	err = target.QueryRowContext(ctx, `SELECT to_regclass($1)`, "public."+rollbackTable).Scan(&rolledBack)
	if err != nil || rolledBack.Valid {
		_ = target.Close()
		t.Fatalf("tabel dari transaksi gagal seharusnya tidak ada: table=%v err=%v", rolledBack, err)
	}
	_ = target.Close()

	updated, err := store.Update(ctx, databaseID, second.ID, 102, Input{Name: "fixed migration", SQL: `ALTER TABLE public.` + pq.QuoteIdentifier(tableName) + ` ADD COLUMN label TEXT`})
	if err != nil || updated.Status != "draft" || updated.Version != 2 {
		t.Fatalf("update migration gagal: migration=%#v err=%v", updated, err)
	}
	if applied, err := store.Execute(ctx, databaseID, second.ID, 102); err != nil || applied.Status != "applied" {
		t.Fatalf("migration kedua gagal: migration=%#v err=%v", applied, err)
	}
	history, err := store.List(ctx, databaseID)
	if err != nil || len(history) != 2 || history[0].Status != "applied" || history[1].Status != "applied" {
		t.Fatalf("riwayat tidak sesuai: history=%#v err=%v", history, err)
	}
}
