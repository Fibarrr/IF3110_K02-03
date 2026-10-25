package migration

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
)

//go:embed schema.sql
var schema embed.FS

const maxSQLBytes = 256 << 10
const maxExecutionDuration = 2 * time.Minute

var (
	ErrNotFound       = errors.New("migration tidak ditemukan")
	ErrNotEditable    = errors.New("migration yang sudah dijalankan tidak bisa diubah")
	ErrOutOfOrder     = errors.New("migration sebelumnya belum sukses")
	ErrAlreadyApplied = errors.New("migration sudah pernah dijalankan")
	ErrAlreadyRunning = errors.New("migration sedang berjalan atau perlu diperiksa")
	ErrExecution      = errors.New("SQL migration gagal dijalankan; perubahan sudah di-rollback")
)

type InputError string

func (e InputError) Error() string { return string(e) }

type Input struct {
	Name string `json:"name"`
	SQL  string `json:"sql"`
}

type Migration struct {
	ID         int64      `json:"id"`
	DatabaseID string     `json:"databaseId"`
	Version    int64      `json:"version"`
	Name       string     `json:"name"`
	SQL        string     `json:"sql"`
	Status     string     `json:"status"`
	CreatedBy  int64      `json:"createdBy"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	ExecutedBy *int64     `json:"executedBy,omitempty"`
	ExecutedAt *time.Time `json:"executedAt,omitempty"`
	LastError  string     `json:"lastError,omitempty"`
}

type TargetStore interface {
	GetConnection(ctx context.Context, databaseID string) (databasemanagement.DatabaseConnection, error)
	OpenTargetDB(ctx context.Context, databaseID string) (*sql.DB, databasemanagement.DatabaseConnection, error)
}

type Store struct {
	db      *sql.DB
	targets TargetStore
}

func NewStore(db *sql.DB, targets TargetStore) *Store {
	return &Store{db: db, targets: targets}
}

func (s *Store) Migrate(ctx context.Context) error {
	contents, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("baca schema migration: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("siapkan tabel migration: %w", err)
	}
	return nil
}

func (s *Store) Create(ctx context.Context, databaseID string, userID int64, input Input) (Migration, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || userID < 1 {
		return Migration{}, InputError("database dan user harus valid")
	}
	input, err := validateInput(input)
	if err != nil {
		return Migration{}, err
	}
	if _, err := s.targets.GetConnection(ctx, databaseID); err != nil {
		return Migration{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 311031))`, databaseID); err != nil {
		return Migration{}, err
	}
	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM mg_migrations WHERE database_id = $1`, databaseID).Scan(&version); err != nil {
		return Migration{}, err
	}
	m, err := scanMigration(tx.QueryRowContext(ctx, `
		INSERT INTO mg_migrations (database_id, version, name, sql_text, status, created_by)
		VALUES ($1, $2, $3, $4, 'draft', $5)
		RETURNING id, database_id, version, name, sql_text, status, created_by, created_at, updated_at, executed_by, executed_at, last_error`,
		databaseID, version, input.Name, input.SQL, userID))
	if err != nil {
		return Migration{}, err
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, err
	}
	return m, nil
}

func (s *Store) Update(ctx context.Context, databaseID string, migrationID, userID int64, input Input) (Migration, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || migrationID < 1 || userID < 1 {
		return Migration{}, InputError("database, migration, dan user harus valid")
	}
	input, err := validateInput(input)
	if err != nil {
		return Migration{}, err
	}
	m, err := scanMigration(s.db.QueryRowContext(ctx, `
		UPDATE mg_migrations
		SET name = $3, sql_text = $4, status = 'draft', updated_at = NOW(),
		    executed_by = NULL, executed_at = NULL, last_error = ''
		WHERE database_id = $1 AND id = $2 AND status IN ('draft', 'failed')
		RETURNING id, database_id, version, name, sql_text, status, created_by, created_at, updated_at, executed_by, executed_at, last_error`,
		databaseID, migrationID, input.Name, input.SQL))
	if errors.Is(err, ErrNotFound) {
		var exists bool
		if checkErr := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM mg_migrations WHERE database_id=$1 AND id=$2)`, databaseID, migrationID).Scan(&exists); checkErr != nil {
			return Migration{}, checkErr
		}
		if !exists {
			return Migration{}, ErrNotFound
		}
		return Migration{}, ErrNotEditable
	}
	return m, err
}

func (s *Store) Get(ctx context.Context, databaseID string, migrationID int64) (Migration, error) {
	if migrationID < 1 {
		return Migration{}, ErrNotFound
	}
	return scanMigration(s.db.QueryRowContext(ctx, `
		SELECT id, database_id, version, name, sql_text, status, created_by, created_at, updated_at, executed_by, executed_at, last_error
		FROM mg_migrations WHERE database_id=$1 AND id=$2`, normalizeDatabaseID(databaseID), migrationID))
}

func (s *Store) List(ctx context.Context, databaseID string) ([]Migration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, database_id, version, name, sql_text, status, created_by, created_at, updated_at, executed_by, executed_at, last_error
		FROM mg_migrations WHERE database_id=$1 ORDER BY version`, normalizeDatabaseID(databaseID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]Migration, 0)
	for rows.Next() {
		m, err := scanMigration(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, m)
	}
	return result, rows.Err()
}

func (s *Store) Execute(ctx context.Context, databaseID string, migrationID, userID int64) (Migration, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || migrationID < 1 || userID < 1 {
		return Migration{}, InputError("database, migration, dan user harus valid")
	}
	metaTx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, err
	}
	defer metaTx.Rollback()
	if _, err := metaTx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 311031))`, databaseID); err != nil {
		return Migration{}, err
	}
	m, err := scanMigration(metaTx.QueryRowContext(ctx, `
		SELECT id, database_id, version, name, sql_text, status, created_by, created_at, updated_at, executed_by, executed_at, last_error
		FROM mg_migrations WHERE database_id=$1 AND id=$2 FOR UPDATE`, databaseID, migrationID))
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if m.Status == "applied" {
		return Migration{}, ErrAlreadyApplied
	}
	if m.Status == "running" {
		return Migration{}, ErrAlreadyRunning
	}
	if m.Status != "draft" && m.Status != "failed" {
		return Migration{}, ErrNotEditable
	}
	var earlierPending bool
	if err := metaTx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mg_migrations WHERE database_id=$1 AND version<$2 AND status<>'applied')`, databaseID, m.Version).Scan(&earlierPending); err != nil {
		return Migration{}, err
	}
	if earlierPending {
		return Migration{}, ErrOutOfOrder
	}
	now := time.Now().UTC()
	m.Status = "running"
	m.ExecutedBy = &userID
	m.ExecutedAt = &now
	m.UpdatedAt = now
	m.LastError = ""
	if _, err := metaTx.ExecContext(ctx, `UPDATE mg_migrations SET status='running', executed_by=$3, executed_at=$4, updated_at=$4, last_error='' WHERE database_id=$1 AND id=$2`, databaseID, migrationID, userID, now); err != nil {
		return Migration{}, err
	}
	if err := metaTx.Commit(); err != nil {
		return Migration{}, err
	}

	targetDB, _, err := s.targets.OpenTargetDB(ctx, databaseID)
	if err != nil {
		m.Status = "failed"
		m.LastError = "gagal terhubung ke database target"
		if updateErr := s.saveExecutionFailure(&m); updateErr != nil {
			return Migration{}, updateErr
		}
		return m, err
	}
	defer targetDB.Close()

	executionCtx, cancel := context.WithTimeout(ctx, maxExecutionDuration)
	defer cancel()
	targetTx, err := targetDB.BeginTx(executionCtx, &sql.TxOptions{})
	if err == nil {
		_, err = targetTx.ExecContext(executionCtx, m.SQL)
	}
	if err != nil {
		if targetTx != nil {
			_ = targetTx.Rollback()
		}
		m.Status = "failed"
		m.LastError = "SQL gagal; perubahan sudah di-rollback"
		if updateErr := s.saveExecutionFailure(&m); updateErr != nil {
			return Migration{}, updateErr
		}
		return m, fmt.Errorf("%w: %v", ErrExecution, err)
	}
	if err := targetTx.Commit(); err != nil {
		m.Status = "failed"
		m.LastError = "transaksi target gagal di-commit"
		if updateErr := s.saveExecutionFailure(&m); updateErr != nil {
			return Migration{}, updateErr
		}
		return m, fmt.Errorf("%w: %v", ErrExecution, err)
	}

	now = time.Now().UTC()
	m.Status = "applied"
	m.LastError = ""
	m.UpdatedAt = now
	if err = s.saveExecutionStatus(&m); err != nil {
		return Migration{}, err
	}
	return m, nil
}

func (s *Store) saveExecutionFailure(m *Migration) error {
	m.UpdatedAt = time.Now().UTC()
	return s.saveExecutionStatus(m)
}

func (s *Store) saveExecutionStatus(m *Migration) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE mg_migrations SET status=$3, updated_at=$4, last_error=$5 WHERE database_id=$1 AND id=$2 AND status='running'`, m.DatabaseID, m.ID, m.Status, m.UpdatedAt, m.LastError)
	return err
}

type rowScanner interface{ Scan(...any) error }

func scanMigration(row rowScanner) (Migration, error) {
	var m Migration
	var executedBy sql.NullInt64
	var executedAt sql.NullTime
	err := row.Scan(&m.ID, &m.DatabaseID, &m.Version, &m.Name, &m.SQL, &m.Status, &m.CreatedBy,
		&m.CreatedAt, &m.UpdatedAt, &executedBy, &executedAt, &m.LastError)
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	if executedBy.Valid {
		m.ExecutedBy = &executedBy.Int64
	}
	if executedAt.Valid {
		t := executedAt.Time
		m.ExecutedAt = &t
	}
	return m, nil
}

func validateInput(input Input) (Input, error) {
	input.Name = strings.TrimSpace(input.Name)
	input.SQL = strings.TrimSpace(input.SQL)
	if input.Name == "" || len(input.Name) > 120 {
		return Input{}, InputError("nama migration harus berisi 1 sampai 120 karakter")
	}
	if input.SQL == "" || len(input.SQL) > maxSQLBytes {
		return Input{}, InputError("SQL migration tidak boleh kosong dan maksimal 256 KiB")
	}
	return input, nil
}

func normalizeDatabaseID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
