package databasemanagement

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"embed"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq"
)

//go:embed schema.sql
var schema embed.FS

var (
	ErrNotFound         = errors.New("database tidak ditemukan")
	ErrConflict         = errors.New("database dengan ID atau nama tersebut sudah ada")
	ErrMigrationRunning = errors.New("koneksi tidak bisa dihapus saat migration sedang berjalan")
	ErrTargetConnection = errors.New("gagal terhubung ke database target")
)

const maxConcurrentTargetOps = 8

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
	db             *sql.DB
	credentialAEAD cipher.AEAD
	targetSlots    chan struct{}
	targetMu       sync.Mutex
	targetPools    map[string]targetPool
}

type targetPool struct {
	db          *sql.DB
	fingerprint [32]byte
}

func NewStore(db *sql.DB) *Store {
	return newStore(db, nil)
}

// NewStoreWithCredentialKey creates a store that encrypts registered database passwords.
// key must be a base64-encoded 32-byte AES key.
func NewStoreWithCredentialKey(db *sql.DB, encodedKey string) (*Store, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encodedKey))
	if err != nil || len(key) != 32 {
		return nil, errors.New("DATABASE_CREDENTIALS_KEY harus berupa base64 dari 32 byte acak")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return newStore(db, aead), nil
}

func newStore(db *sql.DB, credentialAEAD cipher.AEAD) *Store {
	return &Store{
		db:             db,
		credentialAEAD: credentialAEAD,
		targetSlots:    make(chan struct{}, maxConcurrentTargetOps),
		targetPools:    make(map[string]targetPool),
	}
}

// Close menutup pool koneksi target yang disimpan Store.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.targetMu.Lock()
	pools := s.targetPools
	s.targetPools = make(map[string]targetPool)
	s.targetMu.Unlock()
	var closeErr error
	for _, pool := range pools {
		if err := pool.db.Close(); err != nil && closeErr == nil {
			closeErr = err
		}
	}
	return closeErr
}

func (s *Store) invalidateTargetPool(databaseID string) {
	s.targetMu.Lock()
	pool, ok := s.targetPools[databaseID]
	delete(s.targetPools, databaseID)
	s.targetMu.Unlock()
	if ok {
		_ = pool.db.Close()
	}
}

// AcquireTargetSlot membatasi jumlah operasi bersamaan ke semua database target.
func (s *Store) AcquireTargetSlot(ctx context.Context) (func(), error) {
	if s == nil || s.targetSlots == nil {
		return func() {}, nil
	}
	select {
	case s.targetSlots <- struct{}{}:
		return func() { <-s.targetSlots }, nil
	case <-ctx.Done():
		return nil, TargetConnectionError{Message: "antrean operasi database target dibatalkan atau melewati batas waktu"}
	}
}

func (s *Store) Migrate(ctx context.Context) error {
	contents, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("read database management schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("apply database management schema: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE dm_databases ADD COLUMN IF NOT EXISTS password_encrypted BYTEA NOT NULL DEFAULT ''::bytea`); err != nil {
		return fmt.Errorf("prepare encrypted database credentials: %w", err)
	}
	if s.credentialAEAD != nil {
		if err := s.encryptLegacyPasswords(ctx); err != nil {
			return fmt.Errorf("migrate stored database credentials: %w", err)
		}
		if err := s.verifyEncryptedPasswords(ctx); err != nil {
			return fmt.Errorf("verify stored database credentials: %w", err)
		}
	}
	return nil
}

func (s *Store) verifyEncryptedPasswords(ctx context.Context) error {
	rows, err := s.db.QueryContext(ctx, `SELECT id, password_encrypted FROM dm_databases WHERE octet_length(password_encrypted) > 0`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var encrypted []byte
		if err := rows.Scan(&id, &encrypted); err != nil {
			return err
		}
		if _, err := s.decryptPassword(encrypted); err != nil {
			return fmt.Errorf("koneksi database %q: %w", id, err)
		}
	}
	return rows.Err()
}

func (s *Store) encryptLegacyPasswords(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id, password FROM dm_databases WHERE password <> '' AND octet_length(password_encrypted) = 0 ORDER BY id`)
	if err != nil {
		return err
	}
	type legacyPassword struct {
		id       string
		password string
	}
	legacy := make([]legacyPassword, 0)
	for rows.Next() {
		var item legacyPassword
		if err := rows.Scan(&item.id, &item.password); err != nil {
			_ = rows.Close()
			return err
		}
		legacy = append(legacy, item)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, item := range legacy {
		encrypted, err := s.encryptPassword(item.password)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE dm_databases SET password_encrypted = $2, password = '' WHERE id = $1 AND password = $3 AND octet_length(password_encrypted) = 0`, item.id, encrypted, item.password)
		if err != nil {
			return err
		}
		updated, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if updated != 1 {
			return fmt.Errorf("credential database %q berubah saat migrasi", item.id)
		}
	}
	return tx.Commit()
}

func (s *Store) encryptPassword(password string) ([]byte, error) {
	if password == "" {
		return nil, nil
	}
	if s.credentialAEAD == nil {
		return nil, errors.New("kunci enkripsi credential database belum dikonfigurasi")
	}
	nonce := make([]byte, s.credentialAEAD.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	ciphertext := s.credentialAEAD.Seal(nil, nonce, []byte(password), []byte("dm_databases.password:v1"))
	result := make([]byte, 1, 1+len(nonce)+len(ciphertext))
	result[0] = 1
	result = append(result, nonce...)
	result = append(result, ciphertext...)
	return result, nil
}

func (s *Store) decryptPassword(encrypted []byte) (string, error) {
	if len(encrypted) == 0 {
		return "", nil
	}
	if s.credentialAEAD == nil {
		return "", errors.New("kunci enkripsi credential database belum dikonfigurasi")
	}
	if encrypted[0] != 1 {
		return "", errors.New("versi enkripsi credential database tidak dikenal")
	}
	start := 1
	end := start + s.credentialAEAD.NonceSize()
	if len(encrypted) < end+s.credentialAEAD.Overhead() {
		return "", errors.New("credential database terenkripsi tidak valid")
	}
	plaintext, err := s.credentialAEAD.Open(nil, encrypted[start:end], encrypted[end:], []byte("dm_databases.password:v1"))
	if err != nil {
		return "", errors.New("gagal mendekripsi credential database; periksa DATABASE_CREDENTIALS_KEY")
	}
	return string(plaintext), nil
}

func (s *Store) CreateConnection(ctx context.Context, input ConnectionInput) (DatabaseConnection, error) {
	normalized, err := validateConnectionInput(input, true)
	if err != nil {
		return DatabaseConnection{}, err
	}
	encryptedPassword, err := s.encryptPassword(normalized.Password)
	if err != nil {
		return DatabaseConnection{}, err
	}
	var conn DatabaseConnection
	err = s.db.QueryRowContext(ctx, `
		INSERT INTO dm_databases (id, name, host, port, database_name, username, password, password_encrypted, ssl_mode, description)
		VALUES ($1, $2, $3, $4, $5, $6, '', $7, $8, $9)
		RETURNING id, name, host, port, database_name, username, (password <> '' OR octet_length(password_encrypted) > 0), ssl_mode, description, created_at, updated_at`,
		normalized.ID, normalized.Name, normalized.Host, normalized.Port, normalized.DatabaseName,
		normalized.Username, encryptedPassword, normalized.SSLMode, normalized.Description,
	).Scan(&conn.ID, &conn.Name, &conn.Host, &conn.Port, &conn.DatabaseName, &conn.Username,
		&conn.HasPassword, &conn.SSLMode, &conn.Description, &conn.CreatedAt, &conn.UpdatedAt)
	if err != nil {
		return DatabaseConnection{}, mapDatabaseError(err)
	}
	s.invalidateTargetPool(normalized.ID)
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
	encryptedPassword, err := s.encryptPassword(normalized.Password)
	if err != nil {
		return DatabaseConnection{}, err
	}
	var conn DatabaseConnection
	err = s.db.QueryRowContext(ctx, `
		UPDATE dm_databases
		SET name = $2,
		    host = $3,
		    port = $4,
		    database_name = $5,
		    username = $6,
		    password = '',
		    password_encrypted = CASE WHEN $7::bytea IS NULL THEN password_encrypted ELSE $7 END,
		    ssl_mode = $8,
		    description = $9,
		    updated_at = NOW()
		WHERE id = $1
		RETURNING id, name, host, port, database_name, username, (password <> '' OR octet_length(password_encrypted) > 0), ssl_mode, description, created_at, updated_at`,
		databaseID, normalized.Name, normalized.Host, normalized.Port, normalized.DatabaseName,
		normalized.Username, encryptedPassword, normalized.SSLMode, normalized.Description,
	).Scan(&conn.ID, &conn.Name, &conn.Host, &conn.Port, &conn.DatabaseName, &conn.Username,
		&conn.HasPassword, &conn.SSLMode, &conn.Description, &conn.CreatedAt, &conn.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return DatabaseConnection{}, ErrNotFound
	}
	if err != nil {
		return DatabaseConnection{}, mapDatabaseError(err)
	}
	s.invalidateTargetPool(databaseID)
	return conn, nil
}

// DeleteConnection menghapus konfigurasi database beserta seluruh grant akses grup dan merotasi sesi terkait dalam satu transaksi.
func (s *Store) DeleteConnection(ctx context.Context, databaseID string) error {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if databaseID == "" {
		return ErrNotFound
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 311031))`, databaseID); err != nil {
		return err
	}
	var migrationTable sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT to_regclass('mg_migrations')::text`).Scan(&migrationTable); err != nil {
		return err
	}
	var hasRunningMigration bool
	if migrationTable.Valid {
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM mg_migrations WHERE database_id = $1 AND status = 'running')`, databaseID).Scan(&hasRunningMigration); err != nil {
			return err
		}
	}
	if hasRunningMigration {
		return ErrMigrationRunning
	}

	result, err := tx.ExecContext(ctx, `DELETE FROM dm_databases WHERE id = $1`, databaseID)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrNotFound
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE ac_sessions
		SET rotated_at = TO_TIMESTAMP(0)
		WHERE user_id IN (
		    SELECT m.user_id
		    FROM ac_group_members m
		    JOIN ac_database_grants g ON g.group_id = m.group_id
		    WHERE g.database_id = $1
		)`, databaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM ac_database_grants WHERE database_id = $1`, databaseID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.invalidateTargetPool(databaseID)
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
		SELECT id, name, host, port, database_name, username,
		       (password <> '' OR octet_length(password_encrypted) > 0), ssl_mode, description, created_at, updated_at
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

// OpenTargetDB mengambil pool yang digunakan ulang untuk database target terdaftar.
// Store menutup pool melalui Close atau saat konfigurasi koneksi berubah.
func (s *Store) OpenTargetDB(ctx context.Context, databaseID string) (*sql.DB, DatabaseConnection, error) {
	stored, err := s.getStoredConnection(ctx, databaseID)
	if err != nil {
		return nil, DatabaseConnection{}, err
	}
	dsn := buildPostgresDSN(stored)
	fingerprint := sha256.Sum256([]byte(dsn))
	s.targetMu.Lock()
	pool, ok := s.targetPools[stored.ID]
	if !ok || pool.fingerprint != fingerprint {
		if ok {
			_ = pool.db.Close()
		}
		targetDB, openErr := sql.Open("postgres", dsn)
		if openErr != nil {
			s.targetMu.Unlock()
			return nil, stored.DatabaseConnection, TargetConnectionError{Message: sanitizeConnectionError(openErr, stored.Password)}
		}
		targetDB.SetMaxOpenConns(2)
		targetDB.SetMaxIdleConns(1)
		targetDB.SetConnMaxIdleTime(time.Minute)
		targetDB.SetConnMaxLifetime(2 * time.Minute)
		pool = targetPool{db: targetDB, fingerprint: fingerprint}
		s.targetPools[stored.ID] = pool
	}
	s.targetMu.Unlock()
	targetDB := pool.db

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := targetDB.PingContext(pingCtx); err != nil {
		return nil, stored.DatabaseConnection, TargetConnectionError{Message: sanitizeConnectionError(err, stored.Password)}
	}
	return targetDB, stored.DatabaseConnection, nil
}

func (s *Store) getStoredConnection(ctx context.Context, databaseID string) (storedConnection, error) {
	databaseID = strings.ToLower(strings.TrimSpace(databaseID))
	if databaseID == "" {
		return storedConnection{}, ErrNotFound
	}
	var stored storedConnection
	var legacyPassword string
	var encryptedPassword []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT id, name, host, port, database_name, username, password, password_encrypted,
		       (password <> '' OR octet_length(password_encrypted) > 0), ssl_mode, description, created_at, updated_at
		FROM dm_databases
		WHERE id = $1`, databaseID).
		Scan(&stored.ID, &stored.Name, &stored.Host, &stored.Port, &stored.DatabaseName,
			&stored.Username, &legacyPassword, &encryptedPassword, &stored.HasPassword, &stored.SSLMode,
			&stored.Description, &stored.CreatedAt, &stored.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return storedConnection{}, ErrNotFound
	}
	if err != nil {
		return storedConnection{}, err
	}
	if len(encryptedPassword) > 0 {
		stored.Password, err = s.decryptPassword(encryptedPassword)
		if err != nil {
			return storedConnection{}, err
		}
	} else {
		stored.Password = legacyPassword
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
	if errors.Is(err, context.DeadlineExceeded) {
		return "waktu koneksi habis; periksa host, port, dan apakah PostgreSQL target sedang berjalan"
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return "nama host database tidak ditemukan; periksa nilai host"
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		if opErr.Timeout() {
			return "waktu koneksi habis; periksa host, port, dan firewall database target"
		}
		return "server database tidak dapat dijangkau; periksa host dan port"
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		switch string(pqErr.Code) {
		case "28P01":
			return "autentikasi PostgreSQL ditolak; periksa username dan password"
		case "3D000":
			return "database target tidak ditemukan; periksa nama database"
		case "28000":
			return "aturan autentikasi PostgreSQL menolak koneksi untuk user atau host ini"
		case "42501":
			return "user PostgreSQL tidak memiliki izin yang diperlukan"
		default:
			return "PostgreSQL menolak koneksi (SQLSTATE " + string(pqErr.Code) + "); periksa konfigurasi target"
		}
	}
	return "gagal terhubung ke database target; periksa host, port, nama database, kredensial, dan SSL mode"
}

func mapDatabaseError(err error) error {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrConflict
	}
	return err
}
