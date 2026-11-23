package databasemanagement

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	"github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

//go:embed schema.sql
var schema embed.FS

var (
	ErrNotFound         = errors.New("database tidak ditemukan")
	ErrConflict         = errors.New("database dengan ID atau nama tersebut sudah ada")
	ErrTargetConnection = errors.New("gagal terhubung ke database target")
)

type InputError string

func (e InputError) Error() string { return string(e) }

type TargetConnectionError struct {
	Message string
}

func (e TargetConnectionError) Error() string { return e.Message }
func (e TargetConnectionError) Unwrap() error { return ErrTargetConnection }

// DatabaseConnection menyimpan metadata koneksi PostgreSQL tanpa menyertakan password.
type DatabaseConnection struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Host         string    `json:"host"`
	Port         int       `json:"port"`
	DatabaseName string    `json:"databaseName"`
	Username     string    `json:"username"`
	SSLMode      string    `json:"sslMode"`
	Description  string    `json:"description"`
	HasPassword  bool      `json:"hasPassword"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

type ConnectionInput struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Host         string `json:"host"`
	Port         int    `json:"port"`
	DatabaseName string `json:"databaseName"`
	Username     string `json:"username"`
	Password     string `json:"password"`
	SSLMode      string `json:"sslMode"`
	Description  string `json:"description"`
}

type storedConnection struct {
	DatabaseConnection
	Password string `json:"-"`
}

type Store struct {
	db *sql.DB
}

func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

func (s *Store) Migrate(ctx context.Context) error {
	contents, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read database management schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("apply database management schema: %w", err)
	}
	return nil
}

func (s *Store) CreateConnection(ctx context.Context, input ConnectionInput) (DatabaseConnection, error) {
	normalized, err := validateConnectionInput(input, true)
	if err != nil {
		return DatabaseConnection{}, err
	}
	ctx = telemetry.WithDatabaseID(ctx, normalized.ID)
	ctx, span := telemetry.StartSpan(ctx, "db.management.create_connection")
	defer span.End()

	const query = `
		INSERT INTO dm_databases (id, name, host, port, database_name, username, password, ssl_mode, description)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, name, host, port, database_name, username, (password <> ''), ssl_mode, description, created_at, updated_at`
	var conn DatabaseConnection
	err = telemetry.ObserveSQL(ctx, normalized.ID, query, []any{normalized.ID, normalized.Host, normalized.Port, normalized.DatabaseName}, func() (int64, error) {
		scanErr := s.db.QueryRowContext(ctx, query,
			normalized.ID, normalized.Name, normalized.Host, normalized.Port, normalized.DatabaseName,
			normalized.Username, normalized.Password, normalized.SSLMode, normalized.Description,
		).Scan(&conn.ID, &conn.Name, &conn.Host, &conn.Port, &conn.DatabaseName, &conn.Username,
			&conn.HasPassword, &conn.SSLMode, &conn.Description, &conn.CreatedAt, &conn.UpdatedAt)
		if scanErr != nil {
			return 0, scanErr
		}
		return 1, nil
	})
	if err != nil {
		return DatabaseConnection{}, mapDatabaseError(err)
	}
	return conn, nil
}

func (s *Store) UpdateConnection(ctx context.Context, databaseID string, input ConnectionInput) (DatabaseConnection, error) {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if databaseID == "" {
		return DatabaseConnection{}, ErrNotFound
	}
	input.ID = databaseID
	normalized, err := validateConnectionInput(input, false)
	if err != nil {
		return DatabaseConnection{}, err
	}
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.management.update_connection")
	defer span.End()

	const query = `
		UPDATE dm_databases
		SET name = $2, host = $3, port = $4, database_name = $5, username = $6,
		    password = CASE WHEN $7 = '' THEN password ELSE $7 END,
		    ssl_mode = $8, description = $9, updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, host, port, database_name, username, (password <> ''), ssl_mode, description, created_at, updated_at`
	var conn DatabaseConnection
	err = telemetry.ObserveSQL(ctx, databaseID, query, []any{databaseID, normalized.Host, normalized.Port, normalized.DatabaseName}, func() (int64, error) {
		scanErr := s.db.QueryRowContext(ctx, query,
			databaseID, normalized.Name, normalized.Host, normalized.Port, normalized.DatabaseName,
			normalized.Username, normalized.Password, normalized.SSLMode, normalized.Description,
		).Scan(&conn.ID, &conn.Name, &conn.Host, &conn.Port, &conn.DatabaseName, &conn.Username,
			&conn.HasPassword, &conn.SSLMode, &conn.Description, &conn.CreatedAt, &conn.UpdatedAt)
		if scanErr != nil {
			return 0, scanErr
		}
		return 1, nil
	})
	if errors.Is(err, sql.ErrNoRows) {
		return DatabaseConnection{}, ErrNotFound
	}
	if err != nil {
		return DatabaseConnection{}, mapDatabaseError(err)
	}
	return conn, nil
}

// DeleteConnection menghapus konfigurasi database beserta seluruh grant akses grup dan merotasi sesi terkait dalam satu transaksi.
func (s *Store) DeleteConnection(ctx context.Context, databaseID string) error {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if databaseID == "" {
		return ErrNotFound
	}
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.management.delete_connection")
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return err
	}
	committed := false
	defer func() {
		if !committed {
			rbErr := tx.Rollback()
			if errors.Is(rbErr, sql.ErrTxDone) {
				rbErr = nil
			}
			telemetry.LogSQLRollback(ctx, databaseID, "delete_connection_aborted", rbErr)
		}
	}()

	var deletedCount int64
	if err := telemetry.ObserveSQL(ctx, databaseID, `DELETE FROM dm_databases WHERE id = $1`, []any{databaseID}, func() (int64, error) {
		res, execErr := tx.ExecContext(ctx, `DELETE FROM dm_databases WHERE id = $1`, databaseID)
		if execErr != nil {
			return 0, execErr
		}
		deletedCount, execErr = res.RowsAffected()
		return deletedCount, execErr
	}); err != nil {
		return err
	}
	if deletedCount == 0 {
		return ErrNotFound
	}

	const rotateQuery = `
		UPDATE ac_sessions SET rotated_at = TO_TIMESTAMP(0)
		WHERE user_id IN (
		    SELECT m.user_id FROM ac_group_members m
		    JOIN ac_database_grants g ON g.group_id = m.group_id
		    WHERE g.database_id = $1
		)`
	if err := telemetry.ObserveSQL(ctx, databaseID, rotateQuery, []any{databaseID}, func() (int64, error) {
		res, execErr := tx.ExecContext(ctx, rotateQuery, databaseID)
		if execErr != nil {
			return 0, execErr
		}
		return res.RowsAffected()
	}); err != nil {
		return err
	}
	if err := telemetry.ObserveSQL(ctx, databaseID, `DELETE FROM ac_database_grants WHERE database_id = $1`, []any{databaseID}, func() (int64, error) {
		res, execErr := tx.ExecContext(ctx, `DELETE FROM ac_database_grants WHERE database_id = $1`, databaseID)
		if execErr != nil {
			return 0, execErr
		}
		return res.RowsAffected()
	}); err != nil {
		return err
	}

	commitStart := time.Now()
	err = tx.Commit()
	telemetry.LogSQLCommit(ctx, databaseID, time.Since(commitStart), err)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return err
	}
	committed = true
	return nil
}

func (s *Store) GetConnection(ctx context.Context, databaseID string) (DatabaseConnection, error) {
	stored, err := s.getStoredConnection(ctx, databaseID)
	if err != nil {
		return DatabaseConnection{}, err
	}
	return stored.DatabaseConnection, nil
}

func (s *Store) ListConnections(ctx context.Context) ([]DatabaseConnection, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, host, port, database_name, username, (password <> ''), ssl_mode, description, created_at, updated_at
		FROM dm_databases
		ORDER BY name, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	conns := make([]DatabaseConnection, 0)
	for rows.Next() {
		var conn DatabaseConnection
		if err := rows.Scan(&conn.ID, &conn.Name, &conn.Host, &conn.Port, &conn.DatabaseName,
			&conn.Username, &conn.HasPassword, &conn.SSLMode, &conn.Description, &conn.CreatedAt, &conn.UpdatedAt); err != nil {
			return nil, err
		}
		conns = append(conns, conn)
	}
	return conns, rows.Err()
}

// OpenTargetDB membuka koneksi *sql.DB ke database PostgreSQL target yang terdaftar.
// Caller wajib menutup *sql.DB yang dikembalikan setelah selesai dipakai.
func (s *Store) OpenTargetDB(ctx context.Context, databaseID string) (*sql.DB, DatabaseConnection, error) {
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.management.open_target_db")
	defer span.End()

	stored, err := s.getStoredConnection(ctx, databaseID)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return nil, DatabaseConnection{}, err
	}
	span.SetAttributes(
		attribute.String("db.host", stored.Host),
		attribute.Int("db.port", stored.Port),
		attribute.String("db.name", stored.DatabaseName),
		attribute.String("db.ssl_mode", stored.SSLMode),
	)
	targetDB, err := sql.Open("postgres", buildPostgresDSN(stored))
	if err != nil {
		safeErr := TargetConnectionError{Message: sanitizeConnectionError(err, stored.Password)}
		telemetry.RecordSpanError(span, safeErr)
		return nil, stored.DatabaseConnection, safeErr
	}
	targetDB.SetMaxOpenConns(4)
	targetDB.SetMaxIdleConns(2)
	targetDB.SetConnMaxLifetime(2 * time.Minute)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := targetDB.PingContext(pingCtx); err != nil {
		_ = targetDB.Close()
		safeErr := TargetConnectionError{Message: sanitizeConnectionError(err, stored.Password)}
		telemetry.RecordSpanError(span, safeErr)
		return nil, stored.DatabaseConnection, safeErr
	}
	return targetDB, stored.DatabaseConnection, nil
}

func (s *Store) getStoredConnection(ctx context.Context, databaseID string) (storedConnection, error) {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if databaseID == "" {
		return storedConnection{}, ErrNotFound
	}
	var stored storedConnection
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, host, port, database_name, username, password, (password <> ''), ssl_mode, description, created_at, updated_at
		FROM dm_databases
		WHERE id = $1`, databaseID).
		Scan(&stored.ID, &stored.Name, &stored.Host, &stored.Port, &stored.DatabaseName,
			&stored.Username, &stored.Password, &stored.HasPassword, &stored.SSLMode,
			&stored.Description, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedConnection{}, ErrNotFound
	}
	if err != nil {
		return storedConnection{}, err
	}
	return stored, nil
}

func validateConnectionInput(input ConnectionInput, allowSlugFromName bool) (ConnectionInput, error) {
	input.Name = strings.TrimSpace(input.Name)
	if input.Name == "" || len(input.Name) > 80 {
		return ConnectionInput{}, InputError("nama koneksi harus berisi 1 sampai 80 karakter")
	}
	var err error
	if allowSlugFromName {
		input.ID, err = normalizeDatabaseID(input.ID, input.Name)
	} else {
		input.ID, err = normalizeDatabaseID(input.ID, "")
	}
	if err != nil {
		return ConnectionInput{}, err
	}
	input.Host = strings.TrimSpace(input.Host)
	if !isValidHost(input.Host) {
		return ConnectionInput{}, InputError("host database tidak valid")
	}
	if input.Port == 0 {
		input.Port = 5432
	}
	if input.Port < 1 || input.Port > 65535 {
		return ConnectionInput{}, InputError("port database harus berada di antara 1 sampai 65535")
	}
	input.DatabaseName = strings.TrimSpace(input.DatabaseName)
	if input.DatabaseName == "" || len(input.DatabaseName) > 128 {
		return ConnectionInput{}, InputError("nama database PostgreSQL harus berisi 1 sampai 128 karakter")
	}
	input.Username = strings.TrimSpace(input.Username)
	if input.Username == "" || len(input.Username) > 128 {
		return ConnectionInput{}, InputError("username database harus berisi 1 sampai 128 karakter")
	}
	if len(input.Password) > 512 {
		return ConnectionInput{}, InputError("password database terlalu panjang")
	}
	input.SSLMode = strings.ToLower(strings.TrimSpace(input.SSLMode))
	if input.SSLMode == "" {
		input.SSLMode = "disable"
	}
	switch input.SSLMode {
	case "disable", "require", "verify-ca", "verify-full":
	default:
		return ConnectionInput{}, InputError("sslMode harus salah satu dari: disable, require, verify-ca, verify-full")
	}
	input.Description = strings.TrimSpace(input.Description)
	if len(input.Description) > 300 {
		return ConnectionInput{}, InputError("deskripsi maksimal 300 karakter")
	}
	return input, nil
}

func isValidHost(host string) bool {
	if host == "" || len(host) > 253 {
		return false
	}
	if net.ParseIP(host) != nil {
		return true
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) < 1 || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
			return false
		}
		for _, r := range label {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' {
				return false
			}
		}
	}
	return true
}

func normalizeDatabaseID(id, fallbackName string) (string, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" && fallbackName != "" {
		var b strings.Builder
		lastDash := false
		for _, r := range strings.ToLower(strings.TrimSpace(fallbackName)) {
			if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
				b.WriteRune(r)
				lastDash = false
			} else if !lastDash && b.Len() > 0 {
				b.WriteByte('-')
				lastDash = true
			}
		}
		id = strings.Trim(b.String(), "-")
	}
	if len(id) < 1 || len(id) > 64 {
		return "", InputError("ID database harus berisi 1 sampai 64 karakter")
	}
	for _, r := range id {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') && r != '.' && r != '_' && r != '-' {
			return "", InputError("ID database hanya boleh berisi huruf kecil, angka, titik, garis bawah, atau tanda hubung")
		}
	}
	return id, nil
}

func buildPostgresDSN(conn storedConnection) string {
	u := &url.URL{
		Scheme: "postgres",
		Host:   net.JoinHostPort(conn.Host, strconv.Itoa(conn.Port)),
		Path:   "/" + conn.DatabaseName,
	}
	if conn.Password != "" {
		u.User = url.UserPassword(conn.Username, conn.Password)
	} else {
		u.User = url.User(conn.Username)
	}
	q := url.Values{}
	q.Set("sslmode", conn.SSLMode)
	q.Set("connect_timeout", "5")
	u.RawQuery = q.Encode()
	return u.String()
}

// sanitizeConnectionError mengembalikan pesan aman tanpa pernah menyertakan DSN atau password.
func sanitizeConnectionError(err error, _ string) string {
	if err == nil {
		return ""
	}
	return "gagal terhubung ke database target"
}

func mapDatabaseError(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
