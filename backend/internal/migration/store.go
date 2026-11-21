package migration

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
)

//go:embed schema.sql
var schema embed.FS

type ActorInfo struct {
	UserID   int64
	Username string
}

type Store struct {
	db      *sql.DB
	targets TargetStore
	access  AccessChecker
}

type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func NewStore(db *sql.DB, targets TargetStore, access AccessChecker) *Store {
	return &Store{db: db, targets: targets, access: access}
}

func (s *Store) Migrate(ctx context.Context) error {
	contents, err := schema.ReadFile("schema.sql")
	if err != nil {
		return fmt.Errorf("baca schema migration: %w", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mulai transaksi schema migration: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, string(contents)); err != nil {
		return fmt.Errorf("siapkan tabel migration: %w", err)
	}
	return tx.Commit()
}

func (s *Store) ListDatabaseApprovers(ctx context.Context, databaseID string) ([]accesscontrol.DatabaseApprover, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" {
		return nil, InputError("databaseID harus valid")
	}
	if s.access == nil {
		return nil, errors.New("access checker belum dikonfigurasi")
	}
	return s.access.ListDatabaseApprovers(ctx, databaseID)
}

func (s *Store) Create(ctx context.Context, databaseID string, userID int64, input Input) (Migration, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || userID < 1 {
		return Migration{}, InputError("database dan user harus valid")
	}
	norm, err := validateInput(input)
	if err != nil {
		return Migration{}, err
	}
	if _, err := s.targets.GetConnection(ctx, databaseID); err != nil {
		return Migration{}, err
	}

	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "migration.create")
	defer span.End()

	batchJSON, err := marshalBatchConfig(norm.BatchConfig)
	if err != nil {
		return Migration{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return Migration{}, err
	}

	var version int64
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) + 1 FROM mg_migrations WHERE database_id = $1`, databaseID).Scan(&version); err != nil {
		return Migration{}, err
	}

	m, err := scanMigration(tx.QueryRowContext(ctx, `
		INSERT INTO mg_migrations (
			database_id, version, name, sql_text, up_sql, down_sql,
			reversible, irreversible_reason, execution_mode, batch_config,
			current_revision, has_executed, status, created_by
		)
		VALUES ($1, $2, $3, $4, $4, $5, $6, $7, $8, $9::jsonb, 1, FALSE, 'draft', $10)
		RETURNING `+migrationSelectCols,
		databaseID, version, norm.Name, norm.UpSQL, norm.DownSQL,
		norm.Reversible, norm.IrreversibleReason, norm.ExecutionMode, string(batchJSON), userID))
	if err != nil {
		return Migration{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mg_migration_revisions (
			migration_id, revision, name, up_sql, down_sql,
			reversible, irreversible_reason, execution_mode, batch_config, created_by
		)
		VALUES ($1, 1, $2, $3, $4, $5, $6, $7, $8::jsonb, $9)`,
		m.ID, m.Name, m.UpSQL, m.DownSQL, m.Reversible, m.IrreversibleReason, m.ExecutionMode, string(batchJSON), userID); err != nil {
		return Migration{}, err
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.created", "ok",
		slog.Int64("migration_id", m.ID), slog.Int64("version", m.Version), slog.String("mode", m.ExecutionMode))
	return m, nil
}

func (s *Store) Update(ctx context.Context, databaseID string, migrationID, userID int64, input Input) (Migration, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || migrationID < 1 || userID < 1 {
		return Migration{}, InputError("database, migration, dan user harus valid")
	}
	norm, err := validateInput(input)
	if err != nil {
		return Migration{}, err
	}

	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "migration.update", attribute.Int64("migration.id", migrationID))
	defer span.End()

	batchJSON, err := marshalBatchConfig(norm.BatchConfig)
	if err != nil {
		return Migration{}, err
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return Migration{}, err
	}

	cur, err := scanMigration(tx.QueryRowContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, migrationID))
	if err != nil {
		return Migration{}, err
	}
	if cur.HasExecuted || cur.Status == JobRunning || cur.Status == JobQueued || cur.Status == "applied" {
		return Migration{}, ErrNotEditable
	}

	nextRev := cur.CurrentRevision + 1
	updated, err := scanMigration(tx.QueryRowContext(ctx, `
		UPDATE mg_migrations
		SET name = $3, sql_text = $4, up_sql = $4, down_sql = $5,
		    reversible = $6, irreversible_reason = $7, execution_mode = $8, batch_config = $9::jsonb,
		    current_revision = $10, status = 'draft', last_error = '', updated_at = NOW()
		WHERE database_id = $1 AND id = $2
		RETURNING `+migrationSelectCols,
		databaseID, migrationID, norm.Name, norm.UpSQL, norm.DownSQL,
		norm.Reversible, norm.IrreversibleReason, norm.ExecutionMode, string(batchJSON), nextRev))
	if err != nil {
		return Migration{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mg_migration_revisions (
			migration_id, revision, name, up_sql, down_sql,
			reversible, irreversible_reason, execution_mode, batch_config, created_by
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)`,
		updated.ID, nextRev, updated.Name, updated.UpSQL, updated.DownSQL,
		updated.Reversible, updated.IrreversibleReason, updated.ExecutionMode, string(batchJSON), userID); err != nil {
		return Migration{}, err
	}

	reason := fmt.Sprintf("revisi diperbarui menjadi revisi %d", nextRev)
	rows, err := tx.QueryContext(ctx, `
		UPDATE mg_review_requests
		SET status = 'invalidated', invalidation_reason = $3, updated_at = NOW()
		WHERE migration_id = $1 AND revision < $2 AND status IN ('pending_review', 'approved')
		RETURNING id`, migrationID, nextRev, reason)
	if err != nil {
		return Migration{}, err
	}
	var invalidatedIDs []int64
	for rows.Next() {
		var rid int64
		if err := rows.Scan(&rid); err != nil {
			rows.Close()
			return Migration{}, err
		}
		invalidatedIDs = append(invalidatedIDs, rid)
	}
	rows.Close()
	for _, rid := range invalidatedIDs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mg_review_audit_events (request_id, database_id, actor_user_id, event_type, comment)
			VALUES ($1, $2, $3, 'invalidated', $4)`, rid, databaseID, userID, reason); err != nil {
			return Migration{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Migration{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.updated", "ok",
		slog.Int64("migration_id", updated.ID), slog.Int("revision", nextRev), slog.Int("invalidated_reviews", len(invalidatedIDs)))
	return updated, nil
}

func (s *Store) Get(ctx context.Context, databaseID string, migrationID int64) (Migration, error) {
	if migrationID < 1 {
		return Migration{}, ErrNotFound
	}
	return scanMigration(s.db.QueryRowContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE database_id = $1 AND id = $2`,
		normalizeDatabaseID(databaseID), migrationID))
}

func (s *Store) List(ctx context.Context, databaseID string) ([]Migration, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE database_id = $1 ORDER BY version ASC`,
		normalizeDatabaseID(databaseID))
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

func (s *Store) ListRevisions(ctx context.Context, databaseID string, migrationID int64) ([]MigrationRevision, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.migration_id, r.revision, r.name, r.up_sql, r.down_sql,
		       r.reversible, r.irreversible_reason, r.execution_mode, r.batch_config, r.created_by, r.created_at
		FROM mg_migration_revisions r
		JOIN mg_migrations m ON m.id = r.migration_id
		WHERE m.database_id = $1 AND r.migration_id = $2
		ORDER BY r.revision DESC`, normalizeDatabaseID(databaseID), migrationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	revs := make([]MigrationRevision, 0)
	for rows.Next() {
		rev, err := scanRevision(rows)
		if err != nil {
			return nil, err
		}
		revs = append(revs, rev)
	}
	return revs, rows.Err()
}

func (s *Store) GetRevision(ctx context.Context, migrationID int64, revision int) (MigrationRevision, error) {
	return scanRevision(s.db.QueryRowContext(ctx, `
		SELECT id, migration_id, revision, name, up_sql, down_sql,
		       reversible, irreversible_reason, execution_mode, batch_config, created_by, created_at
		FROM mg_migration_revisions WHERE migration_id = $1 AND revision = $2`, migrationID, revision))
}

func (s *Store) ListHistory(ctx context.Context, databaseID string, hideRollback bool) ([]ExecutionHistoryEntry, error) {
	return s.listHistory(ctx, databaseID, 0, hideRollback)
}

func (s *Store) ListMigrationHistory(ctx context.Context, databaseID string, migrationID int64) ([]ExecutionHistoryEntry, error) {
	return s.listHistory(ctx, databaseID, migrationID, false)
}

func (s *Store) listHistory(ctx context.Context, databaseID string, migrationID int64, hideRollback bool) ([]ExecutionHistoryEntry, error) {
	const base = `
		SELECT id, database_id, migration_id, job_id, review_request_id,
		       migration_version, migration_name, revision, operation, target_execution_id,
		       attempt, executed_by, executed_by_username, sql_executed, status,
		       committed_batches, committed_rows, error_summary, started_at, finished_at
		FROM mg_executions WHERE database_id = $1`
	var rows *sql.Rows
	var err error
	if migrationID > 0 {
		rows, err = s.db.QueryContext(ctx, base+` AND migration_id = $2 ORDER BY id ASC`, normalizeDatabaseID(databaseID), migrationID)
	} else {
		rows, err = s.db.QueryContext(ctx, base+` ORDER BY id ASC`, normalizeDatabaseID(databaseID))
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	history := make([]ExecutionHistoryEntry, 0)
	for rows.Next() {
		entry, err := scanExecution(rows)
		if err != nil {
			return nil, err
		}
		history = append(history, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if hideRollback {
		return FilterHideRollback(history), nil
	}
	return history, nil
}

func (s *Store) CreateReviewRequest(ctx context.Context, databaseID string, migrationID int64, actor ActorInfo, input ReviewRequestInput) (ReviewRequest, error) {
	databaseID = normalizeDatabaseID(databaseID)
	op := strings.ToLower(strings.TrimSpace(input.Operation))
	if databaseID == "" || migrationID < 1 || actor.UserID < 1 {
		return ReviewRequest{}, InputError("database, migration, dan pengguna harus valid")
	}
	switch op {
	case OpApply, OpRollback, OpForward, OpResume, OpCancelBatch:
	default:
		return ReviewRequest{}, InputError("operation harus salah satu dari: apply, rollback, forward, resume, cancel_batch")
	}
	comment := strings.TrimSpace(input.Comment)
	if len(comment) > 500 {
		return ReviewRequest{}, InputError("komentar review maksimal 500 karakter")
	}

	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "migration.create_review_request",
		attribute.Int64("migration.id", migrationID), attribute.String("migration.operation", op))
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return ReviewRequest{}, err
	}

	approvers, err := s.ListDatabaseApprovers(ctx, databaseID)
	if err != nil {
		return ReviewRequest{}, err
	}
	if len(approvers) == 0 {
		return ReviewRequest{}, ErrNoApproverGroups
	}
	var missingLeaders []string
	for _, a := range approvers {
		if a.LeaderUserID == nil {
			missingLeaders = append(missingLeaders, a.GroupName)
		}
	}
	if len(missingLeaders) > 0 {
		return ReviewRequest{}, fmt.Errorf("%w: %s", ErrMissingLeader, strings.Join(missingLeaders, ", "))
	}

	m, err := scanMigration(tx.QueryRowContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, migrationID))
	if err != nil {
		return ReviewRequest{}, err
	}
	revision := input.Revision
	if revision == 0 {
		revision = m.CurrentRevision
	}
	if revision != m.CurrentRevision {
		return ReviewRequest{}, InputError(fmt.Sprintf("revisi %d tidak cocok dengan revisi terbaru (%d)", revision, m.CurrentRevision))
	}

	targetExecID, targetJobID, err := s.validateOperationStateTx(ctx, tx, databaseID, m, op, input.TargetExecutionID, input.TargetJobID, 0)
	if err != nil {
		return ReviewRequest{}, err
	}

	var req ReviewRequest
	err = tx.QueryRowContext(ctx, `
		INSERT INTO mg_review_requests (
			database_id, migration_id, revision, operation,
			target_execution_id, target_job_id, requested_by, requested_by_username, status, comment
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, 'pending_review', $9)
		RETURNING id, database_id, migration_id, revision, operation,
		          target_execution_id, target_job_id, requested_by, requested_by_username,
		          status, comment, invalidation_reason, created_at, updated_at`,
		databaseID, migrationID, revision, op,
		targetExecID, targetJobID, actor.UserID, actor.Username, comment,
	).Scan(
		&req.ID, &req.DatabaseID, &req.MigrationID, &req.Revision, &req.Operation,
		&req.TargetExecutionID, &req.TargetJobID, &req.RequestedBy, &req.RequestedByUsername,
		&req.Status, &req.Comment, &req.InvalidationReason, &req.CreatedAt, &req.UpdatedAt,
	)
	if err != nil {
		return ReviewRequest{}, err
	}
	req.MigrationVersion = m.Version
	req.MigrationName = m.Name
	req.Approvals = make([]ReviewApproval, 0, len(approvers))

	for _, a := range approvers {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO mg_review_approvals (
				request_id, group_id, group_name, access_level, leader_user_id, leader_username, decision
			) VALUES ($1, $2, $3, $4, $5, $6, 'pending')`,
			req.ID, a.GroupID, a.GroupName, string(a.Access), *a.LeaderUserID, a.LeaderUsername); err != nil {
			return ReviewRequest{}, err
		}
		req.Approvals = append(req.Approvals, ReviewApproval{
			RequestID: req.ID, GroupID: a.GroupID, GroupName: a.GroupName,
			Access: a.Access, LeaderUserID: *a.LeaderUserID, LeaderUsername: a.LeaderUsername, Decision: "pending",
		})
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mg_review_audit_events (request_id, database_id, actor_user_id, actor_username, event_type, comment)
		VALUES ($1, $2, $3, $4, 'submitted', $5)`,
		req.ID, databaseID, actor.UserID, actor.Username, comment); err != nil {
		return ReviewRequest{}, err
	}
	if op == OpApply && m.Status == "draft" {
		if _, err := tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'pending_review', updated_at = NOW() WHERE id = $1`, m.ID); err != nil {
			return ReviewRequest{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.review_requested", ReviewPending,
		slog.Int64("migration_id", migrationID), slog.Int64("review_request_id", req.ID), slog.String("operation", op))
	return req, nil
}

// validateOperationStateTx enforces linear ordering, reversibility, duplicate-job, and DB-blocking rules.
// Pass excludeJobID = 0 from API submit/queue endpoints and excludeJobID = job.ID during worker revalidation.
func (s *Store) validateOperationStateTx(
	ctx context.Context,
	tx *sql.Tx,
	databaseID string,
	m Migration,
	op string,
	reqTargetExecID *int64,
	reqTargetJobID *int64,
	excludeJobID int64,
) (*int64, *int64, error) {
	var uncertainExists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mg_jobs WHERE database_id = $1 AND status = 'uncertain' AND id <> $2)`,
		databaseID, excludeJobID).Scan(&uncertainExists); err != nil {
		return nil, nil, err
	}
	if uncertainExists {
		return nil, nil, ErrDatabaseBlocked
	}

	// Prevent queueing a duplicate active job for the SAME migration (other migrations on the DB can still queue and serialize via lock).
	var duplicateMigJob bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM mg_jobs
			WHERE migration_id = $1 AND status IN ('queued', 'running') AND id <> $2
		)`, m.ID, excludeJobID).Scan(&duplicateMigJob); err != nil {
		return nil, nil, err
	}
	if duplicateMigJob {
		return nil, nil, ErrAlreadyRunning
	}

	var blockingPartialJobID sql.NullInt64
	err := tx.QueryRowContext(ctx, `
		SELECT id FROM mg_jobs
		WHERE database_id = $1 AND status IN ('partial', 'partially_reverted') AND id <> $2
		ORDER BY id DESC LIMIT 1`, databaseID, excludeJobID).Scan(&blockingPartialJobID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	if blockingPartialJobID.Valid && op != OpResume && op != OpCancelBatch {
		return nil, nil, ErrDatabaseBlocked
	}

	switch op {
	case OpApply:
		if m.Status == "applied" {
			return nil, nil, ErrAlreadyApplied
		}
		if excludeJobID == 0 && (m.Status == JobRunning || m.Status == JobQueued) {
			return nil, nil, ErrAlreadyRunning
		}
		if m.Status == "rolled_back" {
			return nil, nil, InputError("migration ini sudah pernah diterapkan lalu di-rollback; gunakan operasi 'forward'")
		}
		var earlierDraft bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mg_migrations
				WHERE database_id = $1 AND version < $2
				  AND status IN ('draft', 'pending_review', 'approved', 'queued', 'running')
			)`, databaseID, m.Version).Scan(&earlierDraft); err != nil {
			return nil, nil, err
		}
		if earlierDraft {
			return nil, nil, ErrOutOfOrder
		}
		return nil, nil, nil

	case OpRollback:
		if !m.Reversible || strings.TrimSpace(m.DownSQL) == "" {
			return nil, nil, ErrIrreversible
		}
		if m.Status != "applied" || m.ActiveExecutionID == nil {
			return nil, nil, InputError("hanya migration yang sedang aktif diterapkan yang dapat di-rollback")
		}
		var laterActive bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(SELECT 1 FROM mg_migrations WHERE database_id = $1 AND version > $2 AND status = 'applied')`,
			databaseID, m.Version).Scan(&laterActive); err != nil {
			return nil, nil, err
		}
		if laterActive {
			return nil, nil, fmt.Errorf("%w: rollback hanya dapat dilakukan pada migration aktif terakhir", ErrOutOfOrder)
		}
		targetExec := *m.ActiveExecutionID
		if reqTargetExecID != nil && *reqTargetExecID != targetExec {
			return nil, nil, InputError(fmt.Sprintf("targetExecutionId (%d) harus merujuk ke eksekusi aktif (%d)", *reqTargetExecID, targetExec))
		}
		return &targetExec, nil, nil

	case OpForward:
		if m.Status == "applied" {
			return nil, nil, ErrAlreadyApplied
		}
		if m.Status != "rolled_back" && m.Status != JobFailed {
			return nil, nil, InputError("forward hanya berlaku untuk migration yang pernah di-rollback atau gagal dieksekusi")
		}
		var earlierBlocked bool
		if err := tx.QueryRowContext(ctx, `
			SELECT EXISTS(
				SELECT 1 FROM mg_migrations
				WHERE database_id = $1 AND version < $2
				  AND status IN ('draft', 'pending_review', 'approved', 'queued', 'running', 'rolled_back')
			)`, databaseID, m.Version).Scan(&earlierBlocked); err != nil {
			return nil, nil, err
		}
		if earlierBlocked {
			return nil, nil, fmt.Errorf("%w: forward harus mengikuti urutan linear tanpa melewati migration sebelumnya", ErrOutOfOrder)
		}
		return reqTargetExecID, nil, nil

	case OpResume, OpCancelBatch:
		if m.ExecutionMode != ModeBatch {
			return nil, nil, InputError("operasi resume/cancel_batch hanya berlaku untuk migration mode batch")
		}
		if op == OpCancelBatch && (!m.Reversible || strings.TrimSpace(m.DownSQL) == "") {
			return nil, nil, fmt.Errorf("%w: pembatalan batch tidak aman tanpa SQL down; buat migration perbaikan baru", ErrIrreversible)
		}
		var targetJob int64
		var targetJobStatus string
		if reqTargetJobID != nil && *reqTargetJobID > 0 {
			err := tx.QueryRowContext(ctx, `
				SELECT id, status FROM mg_jobs WHERE database_id = $1 AND migration_id = $2 AND id = $3`,
				databaseID, m.ID, *reqTargetJobID).Scan(&targetJob, &targetJobStatus)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, ErrNotFound
			}
			if err != nil {
				return nil, nil, err
			}
		} else {
			err := tx.QueryRowContext(ctx, `
				SELECT id, status FROM mg_jobs
				WHERE database_id = $1 AND migration_id = $2 AND status IN ('partial', 'partially_reverted')
				ORDER BY id DESC LIMIT 1`, databaseID, m.ID).Scan(&targetJob, &targetJobStatus)
			if errors.Is(err, sql.ErrNoRows) {
				return nil, nil, InputError("tidak ada batch job berstatus partial atau partially_reverted untuk migration ini")
			}
			if err != nil {
				return nil, nil, err
			}
		}
		if op == OpResume && targetJobStatus != JobPartial {
			return nil, nil, InputError("resume hanya berlaku untuk batch job berstatus partial")
		}
		if op == OpCancelBatch && targetJobStatus != JobPartial && targetJobStatus != JobPartiallyReverted {
			return nil, nil, InputError("cancel_batch hanya berlaku untuk batch job berstatus partial atau partially_reverted")
		}
		var execID sql.NullInt64
		_ = tx.QueryRowContext(ctx, `SELECT id FROM mg_executions WHERE job_id = $1 ORDER BY id DESC LIMIT 1`, targetJob).Scan(&execID)
		var targetExec *int64
		if execID.Valid {
			v := execID.Int64
			targetExec = &v
		}
		return targetExec, &targetJob, nil
	}
	return nil, nil, InputError("operasi tidak dikenal")
}

func (s *Store) GetReviewRequest(ctx context.Context, databaseID string, requestID int64) (ReviewRequest, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if databaseID == "" || requestID < 1 {
		return ReviewRequest{}, ErrNotFound
	}
	var req ReviewRequest
	err := s.db.QueryRowContext(ctx, `
		SELECT r.id, r.database_id, r.migration_id, m.version, m.name, r.revision, r.operation,
		       r.target_execution_id, r.target_job_id, r.requested_by, r.requested_by_username,
		       r.status, r.comment, r.invalidation_reason, r.created_at, r.updated_at
		FROM mg_review_requests r
		JOIN mg_migrations m ON m.id = r.migration_id
		WHERE r.database_id = $1 AND r.id = $2`, databaseID, requestID).Scan(
		&req.ID, &req.DatabaseID, &req.MigrationID, &req.MigrationVersion, &req.MigrationName,
		&req.Revision, &req.Operation, &req.TargetExecutionID, &req.TargetJobID,
		&req.RequestedBy, &req.RequestedByUsername, &req.Status, &req.Comment,
		&req.InvalidationReason, &req.CreatedAt, &req.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, err
	}
	if req.Approvals, err = listApprovals(ctx, s.db, req.ID); err != nil {
		return ReviewRequest{}, err
	}
	if req.AuditEvents, err = s.listAuditEvents(ctx, req.ID); err != nil {
		return ReviewRequest{}, err
	}
	return req, nil
}

func (s *Store) ListReviewRequests(ctx context.Context, databaseID string) ([]ReviewRequest, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.database_id, r.migration_id, m.version, m.name, r.revision, r.operation,
		       r.target_execution_id, r.target_job_id, r.requested_by, r.requested_by_username,
		       r.status, r.comment, r.invalidation_reason, r.created_at, r.updated_at
		FROM mg_review_requests r
		JOIN mg_migrations m ON m.id = r.migration_id
		WHERE r.database_id = $1
		ORDER BY r.id DESC`, normalizeDatabaseID(databaseID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reqs := make([]ReviewRequest, 0)
	for rows.Next() {
		var req ReviewRequest
		if err := rows.Scan(
			&req.ID, &req.DatabaseID, &req.MigrationID, &req.MigrationVersion, &req.MigrationName,
			&req.Revision, &req.Operation, &req.TargetExecutionID, &req.TargetJobID,
			&req.RequestedBy, &req.RequestedByUsername, &req.Status, &req.Comment,
			&req.InvalidationReason, &req.CreatedAt, &req.UpdatedAt,
		); err != nil {
			return nil, err
		}
		reqs = append(reqs, req)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range reqs {
		if reqs[i].Approvals, err = listApprovals(ctx, s.db, reqs[i].ID); err != nil {
			return nil, err
		}
	}
	return reqs, nil
}

func (s *Store) RecordApproval(ctx context.Context, databaseID string, requestID, groupID int64, actor ActorInfo, input ApprovalInput) (ReviewRequest, error) {
	databaseID = normalizeDatabaseID(databaseID)
	decision := strings.ToLower(strings.TrimSpace(input.Decision))
	if decision != "approve" && decision != "reject" {
		return ReviewRequest{}, InputError("decision harus 'approve' atau 'reject'")
	}
	comment := strings.TrimSpace(input.Comment)
	if len(comment) > 500 {
		return ReviewRequest{}, InputError("komentar maksimal 500 karakter")
	}

	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "migration.record_approval",
		attribute.Int64("review.request_id", requestID), attribute.Int64("review.group_id", groupID))
	defer span.End()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ReviewRequest{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return ReviewRequest{}, err
	}

	liveApprovers, err := s.ListDatabaseApprovers(ctx, databaseID)
	if err != nil {
		return ReviewRequest{}, err
	}

	var status, operation string
	var migrationID int64
	var revision int
	err = tx.QueryRowContext(ctx, `
		SELECT status, migration_id, revision, operation
		FROM mg_review_requests WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, requestID).
		Scan(&status, &migrationID, &revision, &operation)
	if errors.Is(err, sql.ErrNoRows) {
		return ReviewRequest{}, ErrNotFound
	}
	if err != nil {
		return ReviewRequest{}, err
	}
	if status != ReviewPending && status != ReviewApproved && status != ReviewRejected {
		return ReviewRequest{}, InputError(fmt.Sprintf("review request berstatus %q tidak dapat diubah lagi", status))
	}

	snapshot, err := listApprovals(ctx, tx, requestID)
	if err != nil {
		return ReviewRequest{}, err
	}
	var currentRev int
	var hasExecuted bool
	if err := tx.QueryRowContext(ctx, `SELECT current_revision, has_executed FROM mg_migrations WHERE id = $1`, migrationID).
		Scan(&currentRev, &hasExecuted); err != nil {
		return ReviewRequest{}, err
	}
	if currentRev != revision {
		_ = invalidateReviewTx(ctx, tx, requestID, migrationID, operation, hasExecuted, databaseID, actor, "revisi migration telah berubah")
		_ = tx.Commit()
		return ReviewRequest{}, ErrApprovalInvalid
	}
	if ok, reason := approverSnapshotsMatch(snapshot, liveApprovers); !ok {
		_ = invalidateReviewTx(ctx, tx, requestID, migrationID, operation, hasExecuted, databaseID, actor, reason)
		_ = tx.Commit()
		return ReviewRequest{}, fmt.Errorf("%w: %s", ErrApprovalInvalid, reason)
	}

	isLeader := false
	for _, a := range snapshot {
		if a.GroupID == groupID && a.LeaderUserID == actor.UserID {
			isLeader = true
			break
		}
	}
	if !isLeader {
		return ReviewRequest{}, ErrNotGroupLeader
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE mg_review_approvals SET decision = $3, comment = $4, decided_by = $5, decided_at = NOW()
		WHERE request_id = $1 AND group_id = $2`, requestID, groupID, decision, comment, actor.UserID); err != nil {
		return ReviewRequest{}, err
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mg_review_audit_events (
			request_id, database_id, group_id, actor_user_id, actor_username, event_type, decision, comment
		) VALUES ($1, $2, $3, $4, $5, 'decision', $6, $7)`,
		requestID, databaseID, groupID, actor.UserID, actor.Username, decision, comment); err != nil {
		return ReviewRequest{}, err
	}

	updatedApprovals, err := listApprovals(ctx, tx, requestID)
	if err != nil {
		return ReviewRequest{}, err
	}
	newStatus := ReviewApproved
	for _, a := range updatedApprovals {
		if a.Decision == "reject" {
			newStatus = ReviewRejected
			break
		}
		if a.Decision != "approve" {
			newStatus = ReviewPending
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE mg_review_requests SET status = $2, updated_at = NOW() WHERE id = $1`, requestID, newStatus); err != nil {
		return ReviewRequest{}, err
	}
	if operation == OpApply && !hasExecuted {
		switch newStatus {
		case ReviewApproved:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'approved', updated_at = NOW() WHERE id = $1 AND status IN ('draft','pending_review')`, migrationID)
		case ReviewRejected:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'draft', updated_at = NOW() WHERE id = $1 AND status IN ('pending_review','approved')`, migrationID)
		}
	}
	if err := tx.Commit(); err != nil {
		return ReviewRequest{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.approval_recorded", newStatus,
		slog.Int64("review_request_id", requestID), slog.Int64("group_id", groupID), slog.String("decision", decision))
	return s.GetReviewRequest(ctx, databaseID, requestID)
}

func (s *Store) EnqueueReviewRequest(ctx context.Context, databaseID string, requestID int64, actor ActorInfo) (Job, error) {
	databaseID = normalizeDatabaseID(databaseID)
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "migration.queue.enqueue", attribute.Int64("review.request_id", requestID))
	defer span.End()

	traceJSON, err := json.Marshal(telemetry.InjectTraceCarrier(ctx))
	if err != nil {
		traceJSON = []byte("{}")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return Job{}, err
	}

	liveApprovers, err := s.ListDatabaseApprovers(ctx, databaseID)
	if err != nil {
		return Job{}, err
	}

	var req ReviewRequest
	err = tx.QueryRowContext(ctx, `
		SELECT id, database_id, migration_id, revision, operation,
		       target_execution_id, target_job_id, requested_by, requested_by_username,
		       status, comment, invalidation_reason, created_at, updated_at
		FROM mg_review_requests WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, requestID).Scan(
		&req.ID, &req.DatabaseID, &req.MigrationID, &req.Revision, &req.Operation,
		&req.TargetExecutionID, &req.TargetJobID, &req.RequestedBy, &req.RequestedByUsername,
		&req.Status, &req.Comment, &req.InvalidationReason, &req.CreatedAt, &req.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if req.Status == ReviewQueued || req.Status == ReviewCompleted {
		if existing, getErr := scanJob(tx.QueryRowContext(ctx, `
			SELECT `+jobSelectCols+` FROM mg_jobs WHERE review_request_id = $1 ORDER BY id DESC LIMIT 1`, req.ID), false); getErr == nil {
			return existing, nil
		}
	}
	if req.Status != ReviewApproved {
		return Job{}, ErrNotApproved
	}

	m, err := scanMigration(tx.QueryRowContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE id = $1 FOR UPDATE`, req.MigrationID))
	if err != nil {
		return Job{}, err
	}
	snapshot, err := listApprovals(ctx, tx, req.ID)
	if err != nil {
		return Job{}, err
	}
	if ok, reason := approverSnapshotsMatch(snapshot, liveApprovers); !ok {
		_ = invalidateReviewTx(ctx, tx, req.ID, m.ID, req.Operation, m.HasExecuted, databaseID, actor, reason)
		_ = tx.Commit()
		return Job{}, fmt.Errorf("%w: %s", ErrApprovalInvalid, reason)
	}
	for _, a := range snapshot {
		if a.Decision != "approve" {
			return Job{}, ErrNotApproved
		}
	}
	if m.CurrentRevision != req.Revision {
		_ = invalidateReviewTx(ctx, tx, req.ID, m.ID, req.Operation, m.HasExecuted, databaseID, actor, "revisi migration sudah berubah")
		_ = tx.Commit()
		return Job{}, ErrApprovalInvalid
	}
	if _, _, err := s.validateOperationStateTx(ctx, tx, databaseID, m, req.Operation, req.TargetExecutionID, req.TargetJobID, 0); err != nil {
		return Job{}, err
	}

	// Derive explicit root_job_id so multi-hop resume/cancel_batch/rollback share one checkpoint lineage.
	// OpApply and OpForward always start a fresh root_job_id = job.ID.
	var inheritedRoot sql.NullInt64
	switch req.Operation {
	case OpResume, OpCancelBatch:
		if req.TargetJobID != nil {
			_ = tx.QueryRowContext(ctx, `SELECT COALESCE(root_job_id, id) FROM mg_jobs WHERE id = $1`, *req.TargetJobID).Scan(&inheritedRoot)
		}
	case OpRollback:
		if req.TargetExecutionID != nil {
			_ = tx.QueryRowContext(ctx, `
				SELECT COALESCE(j.root_job_id, j.id)
				FROM mg_executions e JOIN mg_jobs j ON j.id = e.job_id
				WHERE e.id = $1`, *req.TargetExecutionID).Scan(&inheritedRoot)
		}
	}

	job, err := scanJob(tx.QueryRowContext(ctx, `
		INSERT INTO mg_jobs (
			database_id, migration_id, review_request_id, revision, operation,
			target_execution_id, parent_job_id, root_job_id, requested_by, requested_by_username,
			status, idempotency_key, max_attempts, trace_context
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, 'queued', $11, $12, $13::jsonb)
		RETURNING `+jobSelectCols,
		databaseID, req.MigrationID, req.ID, req.Revision, req.Operation,
		req.TargetExecutionID, req.TargetJobID, inheritedRoot, actor.UserID, actor.Username,
		fmt.Sprintf("review-request:%d", req.ID), defaultMaxAttempts, string(traceJSON)), false)
	if err != nil {
		return Job{}, err
	}
	if job.RootJobID == 0 {
		job.RootJobID = job.ID
		if _, err := tx.ExecContext(ctx, `UPDATE mg_jobs SET root_job_id = id WHERE id = $1`, job.ID); err != nil {
			return Job{}, err
		}
	}
	job.MigrationVersion = m.Version
	job.MigrationName = m.Name

	if _, err := tx.ExecContext(ctx, `UPDATE mg_review_requests SET status = 'queued', updated_at = NOW() WHERE id = $1`, req.ID); err != nil {
		return Job{}, err
	}
	if req.Operation == OpApply {
		_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'queued', updated_at = NOW() WHERE id = $1`, m.ID)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mg_review_audit_events (request_id, database_id, actor_user_id, actor_username, event_type, comment)
		VALUES ($1, $2, $3, $4, 'queued', $5)`,
		req.ID, databaseID, actor.UserID, actor.Username, fmt.Sprintf("job #%d masuk queue", job.ID)); err != nil {
		return Job{}, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.job_enqueued", JobQueued,
		slog.Int64("migration_id", m.ID), slog.Int64("job_id", job.ID), slog.String("operation", job.Operation))
	return job, nil
}

func (s *Store) EnqueueResumeJob(ctx context.Context, databaseID string, targetJobID, reviewRequestID int64, actor ActorInfo) (Job, error) {
	databaseID = normalizeDatabaseID(databaseID)
	if reviewRequestID < 1 {
		err := s.db.QueryRowContext(ctx, `
			SELECT id FROM mg_review_requests
			WHERE database_id = $1 AND target_job_id = $2 AND operation = 'resume' AND status = 'approved'
			ORDER BY id DESC LIMIT 1`, databaseID, targetJobID).Scan(&reviewRequestID)
		if errors.Is(err, sql.ErrNoRows) {
			return Job{}, ErrNotApproved
		}
		if err != nil {
			return Job{}, err
		}
	}
	req, err := s.GetReviewRequest(ctx, databaseID, reviewRequestID)
	if err != nil {
		return Job{}, err
	}
	if req.Operation != OpResume || req.TargetJobID == nil || *req.TargetJobID != targetJobID {
		return Job{}, InputError("reviewRequestId tidak cocok dengan operasi resume untuk job tersebut")
	}
	return s.EnqueueReviewRequest(ctx, databaseID, reviewRequestID, actor)
}

func (s *Store) GetJob(ctx context.Context, databaseID string, jobID int64) (Job, error) {
	if jobID < 1 {
		return Job{}, ErrNotFound
	}
	return scanJob(s.db.QueryRowContext(ctx, `
		SELECT `+jobSelectColsAliased+`, m.version, m.name
		FROM mg_jobs j JOIN mg_migrations m ON m.id = j.migration_id
		WHERE j.database_id = $1 AND j.id = $2`, normalizeDatabaseID(databaseID), jobID), true)
}

func (s *Store) ListJobs(ctx context.Context, databaseID string) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+jobSelectColsAliased+`, m.version, m.name
		FROM mg_jobs j JOIN mg_migrations m ON m.id = j.migration_id
		WHERE j.database_id = $1 ORDER BY j.id DESC`, normalizeDatabaseID(databaseID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	jobs := make([]Job, 0)
	for rows.Next() {
		j, err := scanJob(rows, true)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

func (s *Store) ListJobCheckpoints(ctx context.Context, jobID int64) ([]BatchCheckpoint, error) {
	var rootJobID int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(root_job_id, id) FROM mg_jobs WHERE id = $1`, jobID).Scan(&rootJobID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []BatchCheckpoint{}, nil
		}
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT job_id, execution_id, batch_no, direction, start_cursor, end_cursor, rows_affected, status, committed_at
		FROM mg_batch_checkpoints WHERE job_id = $1
		ORDER BY direction ASC, batch_no ASC`, rootJobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BatchCheckpoint, 0)
	for rows.Next() {
		var cp BatchCheckpoint
		if err := rows.Scan(&cp.JobID, &cp.ExecutionID, &cp.BatchNo, &cp.Direction,
			&cp.StartCursor, &cp.EndCursor, &cp.RowsAffected, &cp.Status, &cp.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func (s *Store) ResolveUncertainJob(ctx context.Context, databaseID string, jobID int64, actor ActorInfo, resolvedStatus, note string) (Job, error) {
	databaseID = normalizeDatabaseID(databaseID)
	resolvedStatus = strings.ToLower(strings.TrimSpace(resolvedStatus))
	if resolvedStatus != JobSucceeded && resolvedStatus != JobFailed {
		return Job{}, InputError("status resolusi harus 'succeeded' atau 'failed'")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, databaseID); err != nil {
		return Job{}, err
	}

	job, err := scanJob(tx.QueryRowContext(ctx, `
		SELECT `+jobSelectCols+` FROM mg_jobs WHERE database_id = $1 AND id = $2 FOR UPDATE`, databaseID, jobID), false)
	if err != nil {
		return Job{}, err
	}
	if job.Status != JobUncertain {
		return Job{}, InputError("hanya job berstatus uncertain yang dapat diresolusi")
	}

	summary := strings.TrimSpace(note)
	if summary == "" {
		summary = fmt.Sprintf("diresolusi manual oleh %s sebagai %s", actor.Username, resolvedStatus)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mg_jobs SET status = $2, error_summary = $3, finished_at = NOW(), updated_at = NOW() WHERE id = $1`,
		jobID, resolvedStatus, summary); err != nil {
		return Job{}, err
	}
	var latestExecID sql.NullInt64
	_ = tx.QueryRowContext(ctx, `SELECT id FROM mg_executions WHERE job_id = $1 ORDER BY id DESC LIMIT 1`, jobID).Scan(&latestExecID)
	if latestExecID.Valid {
		_, _ = tx.ExecContext(ctx, `
			UPDATE mg_executions SET status = $2, error_summary = $3, finished_at = NOW() WHERE id = $1`,
			latestExecID.Int64, resolvedStatus, summary)
	}

	isRevertOp := job.Operation == OpRollback || job.Operation == OpCancelBatch
	if resolvedStatus == JobSucceeded {
		_, _ = tx.ExecContext(ctx, `UPDATE mg_review_requests SET status = 'completed', updated_at = NOW() WHERE id = $1`, job.ReviewRequestID)
		if job.RootJobID > 0 {
			_, _ = tx.ExecContext(ctx, `
				UPDATE mg_jobs SET status = 'succeeded', updated_at = NOW()
				WHERE root_job_id = $1 AND id <> $2 AND status IN ('partial', 'partially_reverted')`, job.RootJobID, jobID)
		}
		if isRevertOp {
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'rolled_back', active_execution_id = NULL, last_error = '', updated_at = NOW() WHERE id = $1`, job.MigrationID)
		} else {
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'applied', active_execution_id = $2, last_error = '', updated_at = NOW() WHERE id = $1`, job.MigrationID, latestExecID)
		}
	} else {
		switch job.Operation {
		case OpRollback:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'applied', last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, summary)
		case OpCancelBatch:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'partially_reverted', last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, summary)
		case OpResume:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'partial', last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, summary)
		default:
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'failed', last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, summary)
		}
	}
	if err := tx.Commit(); err != nil {
		return Job{}, err
	}

	telemetry.LogEvent(ctx, slog.LevelInfo, "migration.uncertain_resolved", resolvedStatus, slog.Int64("job_id", jobID))
	return s.GetJob(ctx, databaseID, jobID)
}

func (s *Store) RenewDBLock(ctx context.Context, databaseID, workerID string, jobID int64, leaseTTL time.Duration) (bool, error) {
	res, err := s.db.ExecContext(ctx, `
		UPDATE mg_db_locks
		SET heartbeat_at = NOW(), lease_until = NOW() + ($4 * INTERVAL '1 millisecond')
		WHERE database_id = $1 AND owner_worker_id = $2 AND job_id = $3`,
		normalizeDatabaseID(databaseID), workerID, jobID, leaseTTL.Milliseconds())
	if err != nil {
		return false, err
	}
	affected, err := res.RowsAffected()
	return affected > 0, err
}

func (s *Store) ReleaseDBLock(ctx context.Context, databaseID, workerID string, jobID int64) error {
	_, err := s.db.ExecContext(ctx, `
		DELETE FROM mg_db_locks WHERE database_id = $1 AND owner_worker_id = $2 AND job_id = $3`,
		normalizeDatabaseID(databaseID), workerID, jobID)
	return err
}

// RecoverExpiredLocks releases locks from crashed workers and moves running jobs safely to partial, partially_reverted, or uncertain.
func (s *Store) RecoverExpiredLocks(ctx context.Context) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT l.database_id, l.job_id, l.owner_worker_id, j.status, j.operation, m.execution_mode, j.committed_batches, j.migration_id
		FROM mg_db_locks l
		JOIN mg_jobs j ON j.id = l.job_id
		JOIN mg_migrations m ON m.id = j.migration_id
		WHERE l.lease_until < NOW()
		FOR UPDATE OF l SKIP LOCKED`)
	if err != nil {
		return 0, err
	}
	type staleLock struct {
		dbID, workerID, jobStatus, operation, mode string
		jobID, migrationID                         int64
		committedBatches                           int
	}
	var stale []staleLock
	for rows.Next() {
		var item staleLock
		if err := rows.Scan(&item.dbID, &item.jobID, &item.workerID, &item.jobStatus, &item.operation, &item.mode, &item.committedBatches, &item.migrationID); err != nil {
			rows.Close()
			return 0, err
		}
		stale = append(stale, item)
	}
	rows.Close()

	for _, item := range stale {
		if item.jobStatus == JobRunning {
			nextStatus := JobUncertain
			msg := fmt.Sprintf("worker %s berhenti sebelum selesai; status commit perlu diverifikasi", item.workerID)
			if item.mode == ModeBatch && item.committedBatches > 0 {
				if item.operation == OpRollback || item.operation == OpCancelBatch {
					nextStatus = JobPartiallyReverted
					msg = fmt.Sprintf("worker %s berhenti setelah %d batch dikompensasi; lanjutkan pembatalan batch", item.workerID, item.committedBatches)
				} else {
					nextStatus = JobPartial
					msg = fmt.Sprintf("worker %s berhenti setelah %d batch committed; dapat dilanjutkan dari checkpoint", item.workerID, item.committedBatches)
				}
			}
			_, _ = tx.ExecContext(ctx, `UPDATE mg_jobs SET status = $2, error_summary = $3, updated_at = NOW() WHERE id = $1`, item.jobID, nextStatus, msg)
			_, _ = tx.ExecContext(ctx, `UPDATE mg_executions SET status = $2, error_summary = $3, finished_at = NOW() WHERE job_id = $1 AND status = 'running'`, item.jobID, nextStatus, msg)
			_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = $2, last_error = $3, updated_at = NOW() WHERE id = $1`, item.migrationID, nextStatus, msg)
			telemetry.LogEvent(ctx, slog.LevelWarn, "migration.lock_recovered", nextStatus,
				slog.String("database", item.dbID), slog.Int64("job_id", item.jobID), slog.String("stale_worker", item.workerID))
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM mg_db_locks WHERE database_id = $1 AND job_id = $2`, item.dbID, item.jobID); err != nil {
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(stale), nil
}

func lockDatabaseTx(ctx context.Context, tx *sql.Tx, databaseID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 311031))`, databaseID)
	return err
}

func invalidateReviewTx(ctx context.Context, tx *sql.Tx, requestID, migrationID int64, operation string, hasExecuted bool, databaseID string, actor ActorInfo, reason string) error {
	if _, err := tx.ExecContext(ctx, `
		UPDATE mg_review_requests SET status = 'invalidated', invalidation_reason = $2, updated_at = NOW() WHERE id = $1`,
		requestID, reason); err != nil {
		return err
	}
	if operation == OpApply && !hasExecuted {
		_, _ = tx.ExecContext(ctx, `UPDATE mg_migrations SET status = 'draft', updated_at = NOW() WHERE id = $1 AND status IN ('pending_review','approved','queued')`, migrationID)
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO mg_review_audit_events (request_id, database_id, actor_user_id, actor_username, event_type, comment)
		VALUES ($1, $2, $3, $4, 'invalidated', $5)`,
		requestID, databaseID, actor.UserID, actor.Username, reason)
	return err
}

func listApprovals(ctx context.Context, q queryer, requestID int64) ([]ReviewApproval, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT request_id, group_id, group_name, access_level, leader_user_id, leader_username,
		       decision, comment, decided_by, decided_at
		FROM mg_review_approvals WHERE request_id = $1 ORDER BY group_id ASC`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ReviewApproval, 0)
	for rows.Next() {
		var a ReviewApproval
		var accessStr string
		var decidedBy sql.NullInt64
		var decidedAt sql.NullTime
		if err := rows.Scan(&a.RequestID, &a.GroupID, &a.GroupName, &accessStr,
			&a.LeaderUserID, &a.LeaderUsername, &a.Decision, &a.Comment, &decidedBy, &decidedAt); err != nil {
			return nil, err
		}
		a.Access = accesscontrol.DatabaseAccess(accessStr)
		if decidedBy.Valid {
			v := decidedBy.Int64
			a.DecidedBy = &v
		}
		if decidedAt.Valid {
			t := decidedAt.Time
			a.DecidedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) listAuditEvents(ctx context.Context, requestID int64) ([]ReviewAuditEvent, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, request_id, database_id, group_id, actor_user_id, actor_username, event_type, decision, comment, created_at
		FROM mg_review_audit_events WHERE request_id = $1 ORDER BY id ASC`, requestID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ReviewAuditEvent, 0)
	for rows.Next() {
		var e ReviewAuditEvent
		var groupID sql.NullInt64
		if err := rows.Scan(&e.ID, &e.RequestID, &e.DatabaseID, &groupID, &e.ActorUserID,
			&e.ActorUsername, &e.EventType, &e.Decision, &e.Comment, &e.CreatedAt); err != nil {
			return nil, err
		}
		if groupID.Valid {
			v := groupID.Int64
			e.GroupID = &v
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

const migrationSelectCols = `
	id, database_id, version, name, up_sql, down_sql,
	reversible, irreversible_reason, execution_mode, batch_config,
	current_revision, has_executed, active_execution_id, status,
	created_by, created_at, updated_at, executed_by, executed_at, last_error`

const jobSelectCols = `
	id, database_id, migration_id, review_request_id, revision, operation,
	target_execution_id, parent_job_id, COALESCE(root_job_id, id), requested_by, requested_by_username,
	status, attempt, max_attempts, next_run_at, worker_id,
	total_rows_estimated, committed_batches, committed_rows, last_cursor, max_cursor,
	error_summary, created_at, started_at, finished_at, updated_at`

const jobSelectColsAliased = `
	j.id, j.database_id, j.migration_id, j.review_request_id, j.revision, j.operation,
	j.target_execution_id, j.parent_job_id, COALESCE(j.root_job_id, j.id), j.requested_by, j.requested_by_username,
	j.status, j.attempt, j.max_attempts, j.next_run_at, j.worker_id,
	j.total_rows_estimated, j.committed_batches, j.committed_rows, j.last_cursor, j.max_cursor,
	j.error_summary, j.created_at, j.started_at, j.finished_at, j.updated_at`

type rowScanner interface{ Scan(...any) error }

func scanMigration(row rowScanner) (Migration, error) {
	var m Migration
	var batchBytes []byte
	var activeExecID, executedBy sql.NullInt64
	var executedAt sql.NullTime
	err := row.Scan(
		&m.ID, &m.DatabaseID, &m.Version, &m.Name, &m.UpSQL, &m.DownSQL,
		&m.Reversible, &m.IrreversibleReason, &m.ExecutionMode, &batchBytes,
		&m.CurrentRevision, &m.HasExecuted, &activeExecID, &m.Status,
		&m.CreatedBy, &m.CreatedAt, &m.UpdatedAt, &executedBy, &executedAt, &m.LastError,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return Migration{}, ErrNotFound
	}
	if err != nil {
		return Migration{}, err
	}
	m.SQL = m.UpSQL
	m.BatchConfig = unmarshalBatchConfig(batchBytes)
	if activeExecID.Valid {
		v := activeExecID.Int64
		m.ActiveExecutionID = &v
	}
	if executedBy.Valid {
		v := executedBy.Int64
		m.ExecutedBy = &v
	}
	if executedAt.Valid {
		t := executedAt.Time
		m.ExecutedAt = &t
	}
	return m, nil
}

func scanRevision(row rowScanner) (MigrationRevision, error) {
	var r MigrationRevision
	var batchBytes []byte
	err := row.Scan(&r.ID, &r.MigrationID, &r.Revision, &r.Name, &r.UpSQL, &r.DownSQL,
		&r.Reversible, &r.IrreversibleReason, &r.ExecutionMode, &batchBytes, &r.CreatedBy, &r.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return MigrationRevision{}, ErrNotFound
	}
	if err != nil {
		return MigrationRevision{}, err
	}
	r.BatchConfig = unmarshalBatchConfig(batchBytes)
	return r, nil
}

func scanJob(row rowScanner, withMigration bool) (Job, error) {
	var j Job
	var targetExecID, parentJobID sql.NullInt64
	var startedAt, finishedAt sql.NullTime
	dest := []any{
		&j.ID, &j.DatabaseID, &j.MigrationID, &j.ReviewRequestID, &j.Revision, &j.Operation,
		&targetExecID, &parentJobID, &j.RootJobID, &j.RequestedBy, &j.RequestedByUsername,
		&j.Status, &j.Attempt, &j.MaxAttempts, &j.NextRunAt, &j.WorkerID,
		&j.TotalRowsEstimated, &j.CommittedBatches, &j.CommittedRows, &j.LastCursor, &j.MaxCursor,
		&j.ErrorSummary, &j.CreatedAt, &startedAt, &finishedAt, &j.UpdatedAt,
	}
	if withMigration {
		dest = append(dest, &j.MigrationVersion, &j.MigrationName)
	}
	err := row.Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, ErrNotFound
	}
	if err != nil {
		return Job{}, err
	}
	if targetExecID.Valid {
		v := targetExecID.Int64
		j.TargetExecutionID = &v
	}
	if parentJobID.Valid {
		v := parentJobID.Int64
		j.ParentJobID = &v
	}
	if startedAt.Valid {
		t := startedAt.Time
		j.StartedAt = &t
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		j.FinishedAt = &t
	}
	return j, nil
}

func scanExecution(row rowScanner) (ExecutionHistoryEntry, error) {
	var e ExecutionHistoryEntry
	var jobID, reqID, targetExecID sql.NullInt64
	var finishedAt sql.NullTime
	err := row.Scan(
		&e.ExecutionID, &e.DatabaseID, &e.MigrationID, &jobID, &reqID,
		&e.MigrationVersion, &e.MigrationName, &e.Revision, &e.Operation, &targetExecID,
		&e.Attempt, &e.ExecutedBy, &e.ExecutedByUsername, &e.SQLExecuted, &e.Status,
		&e.CommittedBatches, &e.CommittedRows, &e.ErrorSummary, &e.StartedAt, &finishedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return ExecutionHistoryEntry{}, ErrNotFound
	}
	if err != nil {
		return ExecutionHistoryEntry{}, err
	}
	if jobID.Valid {
		v := jobID.Int64
		e.JobID = &v
	}
	if reqID.Valid {
		v := reqID.Int64
		e.ReviewRequestID = &v
	}
	if targetExecID.Valid {
		v := targetExecID.Int64
		e.TargetExecutionID = &v
	}
	if finishedAt.Valid {
		t := finishedAt.Time
		e.FinishedAt = &t
	}
	return e, nil
}

func marshalBatchConfig(cfg *BatchConfig) ([]byte, error) {
	if cfg == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(cfg)
}

func unmarshalBatchConfig(raw []byte) *BatchConfig {
	if len(raw) == 0 || string(raw) == "{}" || string(raw) == "null" {
		return nil
	}
	var cfg BatchConfig
	if err := json.Unmarshal(raw, &cfg); err != nil || cfg.TableName == "" {
		return nil
	}
	return &cfg
}
