package migration

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	"github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

type WorkerConfig struct {
	WorkerID     string
	PollInterval time.Duration
	LockLeaseTTL time.Duration
	Concurrency  int
}

type Worker struct {
	store *Store
	cfg   WorkerConfig
}

type jobOutcome struct {
	status           string
	committedBatches int
	committedRows    int64
	lastCursor       int64
	maxCursor        int64
	errorSummary     string
	preExecution     bool
}

func NewWorker(store *Store, cfg WorkerConfig) *Worker {
	if cfg.WorkerID == "" {
		cfg.WorkerID = fmt.Sprintf("worker-%d", time.Now().UTC().UnixNano())
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = 400 * time.Millisecond
	}
	if cfg.LockLeaseTTL <= 0 {
		cfg.LockLeaseTTL = 30 * time.Second
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 2
	}
	return &Worker{store: store, cfg: cfg}
}

// Run starts concurrent worker loops and periodic expired-lock recovery until ctx is cancelled.
func (w *Worker) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(w.cfg.LockLeaseTTL / 2)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				_, _ = w.store.RecoverExpiredLocks(ctx)
			}
		}
	}()

	for i := 0; i < w.cfg.Concurrency; i++ {
		workerName := fmt.Sprintf("%s-%d", w.cfg.WorkerID, i+1)
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				default:
				}
				processed, err := w.processNextJobWithID(ctx, id)
				if err != nil && ctx.Err() == nil {
					telemetry.LogEvent(ctx, slog.LevelError, "worker.poll_error", "error",
						slog.String("worker_id", id), slog.String("error", telemetry.RedactString(err.Error())))
				}
				if !processed {
					select {
					case <-ctx.Done():
						return
					case <-time.After(w.cfg.PollInterval):
					}
				}
			}
		}(workerName)
	}
	wg.Wait()
}

// ProcessNextJob claims and runs at most one queued job synchronously (useful in integration tests).
func (w *Worker) ProcessNextJob(ctx context.Context) (bool, error) {
	return w.processNextJobWithID(ctx, w.cfg.WorkerID)
}

func (w *Worker) processNextJobWithID(ctx context.Context, workerID string) (bool, error) {
	job, traceMap, ok, err := w.claimNextJob(ctx, workerID)
	if err != nil || !ok {
		return false, err
	}

	baseCtx := telemetry.ExtractTraceCarrier(ctx, traceMap)
	baseCtx = telemetry.WithDatabaseID(baseCtx, job.DatabaseID)
	baseCtx = telemetry.WithActor(baseCtx, telemetry.Actor{UserID: job.RequestedBy, Username: job.RequestedByUsername})
	baseCtx = telemetry.WithMigrationMeta(baseCtx, telemetry.MigrationMeta{MigrationID: job.MigrationID, JobID: job.ID, Attempt: job.Attempt})

	jobCtx, cancelJob := context.WithCancel(baseCtx)
	defer cancelJob()

	jobCtx, jobSpan := telemetry.StartSpan(jobCtx, "worker.job",
		attribute.Int64("job.id", job.ID),
		attribute.String("job.operation", job.Operation),
		attribute.Int("job.attempt", job.Attempt),
		attribute.String("worker.id", workerID),
	)
	defer jobSpan.End()

	// Renew distributed DB lock continuously; if lease is lost, cancel jobCtx immediately to prevent split-brain execution.
	hbStop := make(chan struct{})
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		interval := w.cfg.LockLeaseTTL / 3
		if interval < 100*time.Millisecond {
			interval = 100 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-hbStop:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				renewed, rErr := w.store.RenewDBLock(renewCtx, job.DatabaseID, workerID, job.ID, w.cfg.LockLeaseTTL)
				cancel()
				if rErr != nil || !renewed {
					telemetry.LogEvent(jobCtx, slog.LevelError, "worker.lock_lost", "cancelled",
						slog.String("worker_id", workerID), slog.Int64("job_id", job.ID))
					cancelJob()
					return
				}
			}
		}
	}()

	defer func() {
		close(hbStop)
		<-hbDone
		relCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = w.store.ReleaseDBLock(relCtx, job.DatabaseID, workerID, job.ID)
	}()

	if err := w.executeClaimedJob(jobCtx, job); err != nil {
		telemetry.RecordSpanError(jobSpan, err)
	}
	return true, nil
}

func (w *Worker) claimNextJob(ctx context.Context, workerID string) (Job, map[string]string, bool, error) {
	tx, err := w.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Job{}, nil, false, err
	}
	defer tx.Rollback()

	var jobID int64
	var dbID string
	var traceRaw []byte
	err = tx.QueryRowContext(ctx, `
		SELECT j.id, j.database_id, j.trace_context
		FROM mg_jobs j
		WHERE j.status = 'queued'
		  AND j.next_run_at <= NOW()
		  AND NOT EXISTS (
		      SELECT 1 FROM mg_db_locks l WHERE l.database_id = j.database_id AND l.lease_until >= NOW()
		  )
		ORDER BY j.id ASC
		FOR UPDATE OF j SKIP LOCKED
		LIMIT 1`).Scan(&jobID, &dbID, &traceRaw)
	if errors.Is(err, sql.ErrNoRows) {
		return Job{}, nil, false, nil
	}
	if err != nil {
		return Job{}, nil, false, err
	}

	// Use PostgreSQL NOW() consistently (no Go vs DB clock skew).
	res, err := tx.ExecContext(ctx, `
		INSERT INTO mg_db_locks (database_id, owner_worker_id, job_id, acquired_at, heartbeat_at, lease_until)
		VALUES ($1, $2, $3, NOW(), NOW(), NOW() + ($4 * INTERVAL '1 millisecond'))
		ON CONFLICT (database_id) DO UPDATE
		SET owner_worker_id = EXCLUDED.owner_worker_id,
		    job_id = EXCLUDED.job_id,
		    acquired_at = NOW(),
		    heartbeat_at = NOW(),
		    lease_until = NOW() + ($4 * INTERVAL '1 millisecond')
		WHERE mg_db_locks.lease_until < NOW()`,
		dbID, workerID, jobID, w.cfg.LockLeaseTTL.Milliseconds())
	if err != nil {
		return Job{}, nil, false, err
	}
	if affected, err := res.RowsAffected(); err != nil || affected == 0 {
		return Job{}, nil, false, nil
	}

	job, err := scanJob(tx.QueryRowContext(ctx, `
		UPDATE mg_jobs
		SET status = 'running', attempt = attempt + 1, worker_id = $2,
		    started_at = COALESCE(started_at, NOW()), updated_at = NOW()
		WHERE id = $1
		RETURNING `+jobSelectCols, jobID, workerID), false)
	if err != nil {
		return Job{}, nil, false, err
	}
	if err := tx.Commit(); err != nil {
		return Job{}, nil, false, err
	}

	traceMap := make(map[string]string)
	if len(traceRaw) > 0 {
		_ = json.Unmarshal(traceRaw, &traceMap)
	}
	return job, traceMap, true, nil
}

func (w *Worker) executeClaimedJob(ctx context.Context, job Job) error {
	revalCtx, revalSpan := telemetry.StartSpan(ctx, "worker.revalidate")
	m, rev, err := w.revalidateJob(revalCtx, job)
	if err != nil {
		telemetry.RecordSpanError(revalSpan, err)
		revalSpan.End()
		w.finishJob(ctx, job, m, 0, jobOutcome{
			status:       JobFailed,
			errorSummary: fmt.Sprintf("revalidasi gagal: %v", err),
			preExecution: true,
		})
		return err
	}
	revalSpan.End()

	if rev.ExecutionMode == ModeBatch || job.Operation == OpResume || job.Operation == OpCancelBatch {
		return w.executeBatchJob(ctx, job, m, rev)
	}
	return w.executeStandardJob(ctx, job, m, rev)
}

func (w *Worker) revalidateJob(ctx context.Context, job Job) (Migration, MigrationRevision, error) {
	tx, err := w.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Migration{}, MigrationRevision{}, err
	}
	defer tx.Rollback()
	if err := lockDatabaseTx(ctx, tx, job.DatabaseID); err != nil {
		return Migration{}, MigrationRevision{}, err
	}

	m, err := scanMigration(tx.QueryRowContext(ctx, `
		SELECT `+migrationSelectCols+` FROM mg_migrations WHERE database_id = $1 AND id = $2 FOR UPDATE`,
		job.DatabaseID, job.MigrationID))
	if err != nil {
		return Migration{}, MigrationRevision{}, err
	}

	if w.store.access != nil {
		access, err := w.store.access.EffectiveAccess(ctx, job.RequestedBy, job.DatabaseID)
		if err != nil {
			return m, MigrationRevision{}, err
		}
		if !access.CanEdit() {
			return m, MigrationRevision{}, errors.New("pengguna tidak lagi memiliki izin view_edit")
		}
		liveApprovers, err := w.store.access.ListDatabaseApprovers(ctx, job.DatabaseID)
		if err != nil {
			return m, MigrationRevision{}, err
		}
		snapshot, err := listApprovals(ctx, tx, job.ReviewRequestID)
		if err != nil {
			return m, MigrationRevision{}, err
		}
		if ok, reason := approverSnapshotsMatch(snapshot, liveApprovers); !ok {
			_ = invalidateReviewTx(ctx, tx, job.ReviewRequestID, m.ID, job.Operation, m.HasExecuted, job.DatabaseID,
				ActorInfo{UserID: job.RequestedBy, Username: job.RequestedByUsername}, reason)
			_ = tx.Commit()
			return m, MigrationRevision{}, fmt.Errorf("%w: %s", ErrApprovalInvalid, reason)
		}
		for _, a := range snapshot {
			if a.Decision != "approve" {
				return m, MigrationRevision{}, ErrNotApproved
			}
		}
	}

	if m.CurrentRevision != job.Revision {
		return m, MigrationRevision{}, ErrApprovalInvalid
	}
	if _, _, err := w.store.validateOperationStateTx(ctx, tx, job.DatabaseID, m, job.Operation, job.TargetExecutionID, job.ParentJobID, job.ID); err != nil {
		return m, MigrationRevision{}, err
	}
	rev, err := w.store.GetRevision(ctx, job.MigrationID, job.Revision)
	if err != nil {
		return m, MigrationRevision{}, err
	}
	return m, rev, tx.Commit()
}

func (w *Worker) executeStandardJob(ctx context.Context, job Job, m Migration, rev MigrationRevision) error {
	sqlToRun := rev.UpSQL
	if job.Operation == OpRollback {
		sqlToRun = rev.DownSQL
	}

	execID, err := w.insertExecution(ctx, job, m, sqlToRun)
	if err != nil {
		w.finishJob(ctx, job, m, 0, jobOutcome{status: JobFailed, errorSummary: "gagal mencatat riwayat eksekusi", preExecution: true})
		return err
	}
	ctx = telemetry.WithMigrationMeta(ctx, telemetry.MigrationMeta{MigrationID: m.ID, JobID: job.ID, ExecutionID: execID, Attempt: job.Attempt})
	ctx, execSpan := telemetry.StartSpan(ctx, "worker.execute_standard", attribute.Int64("execution.id", execID))
	defer execSpan.End()

	_, _ = w.store.db.ExecContext(ctx, `UPDATE mg_migrations SET has_executed = TRUE, executed_by = $2, executed_at = NOW(), updated_at = NOW() WHERE id = $1`, m.ID, job.RequestedBy)

	targetDB, _, err := w.store.targets.OpenTargetDB(ctx, job.DatabaseID)
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	defer targetDB.Close()

	execCtx, cancel := context.WithTimeout(ctx, maxExecutionDuration)
	defer cancel()

	if err := ensureTargetTrackingTables(execCtx, targetDB); err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	if ok, checkErr := targetStandardJobCommitted(execCtx, targetDB, job.ID); checkErr == nil && ok {
		telemetry.LogEvent(ctx, slog.LevelWarn, "migration.duplicate_skipped", "already_committed", slog.Int64("job_id", job.ID))
		w.finishJob(ctx, job, m, execID, jobOutcome{status: JobSucceeded})
		return nil
	}

	tx, err := targetDB.BeginTx(execCtx, &sql.TxOptions{})
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	committed := false
	defer func() {
		if !committed {
			rbErr := tx.Rollback()
			if errors.Is(rbErr, sql.ErrTxDone) {
				rbErr = nil
			}
			telemetry.LogSQLRollback(ctx, job.DatabaseID, "standard_migration_rollback", rbErr)
		}
	}()

	if err := telemetry.ObserveSQL(ctx, job.DatabaseID, sqlToRun, nil, func() (int64, error) {
		res, eErr := tx.ExecContext(execCtx, sqlToRun)
		if eErr != nil {
			return 0, eErr
		}
		if res != nil {
			if n, rErr := res.RowsAffected(); rErr == nil {
				return n, nil
			}
		}
		return -1, nil
	}); err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	if _, err := tx.ExecContext(execCtx, `INSERT INTO _mg_execution_markers (job_id, execution_id) VALUES ($1, $2) ON CONFLICT (job_id) DO NOTHING`, job.ID, execID); err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}

	commitStart := time.Now()
	err = tx.Commit()
	telemetry.LogSQLCommit(ctx, job.DatabaseID, time.Since(commitStart), err)
	if err != nil {
		telemetry.RecordSpanError(execSpan, err)
		if verified, ok := w.probeStandardCommit(job.DatabaseID, job.ID); ok {
			if verified {
				w.finishJob(ctx, job, m, execID, jobOutcome{status: JobSucceeded})
				return nil
			}
			w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
			return err
		}
		w.finishJob(ctx, job, m, execID, jobOutcome{
			status:       JobUncertain,
			errorSummary: "hasil commit tidak pasti setelah koneksi terputus; verifikasi database target sebelum melanjutkan",
		})
		return err
	}
	committed = true
	w.finishJob(ctx, job, m, execID, jobOutcome{status: JobSucceeded})
	return nil
}

func (w *Worker) executeBatchJob(ctx context.Context, job Job, m Migration, rev MigrationRevision) error {
	if rev.BatchConfig == nil {
		err := errors.New("konfigurasi batch tidak ditemukan pada revisi migration")
		w.finishJob(ctx, job, m, 0, jobOutcome{status: JobFailed, errorSummary: err.Error(), preExecution: true})
		return err
	}
	cfg := *rev.BatchConfig
	isCompensate := job.Operation == OpCancelBatch || job.Operation == OpRollback
	sqlToRun := rev.UpSQL
	if isCompensate {
		if !rev.Reversible || rev.DownSQL == "" {
			w.finishJob(ctx, job, m, 0, jobOutcome{status: JobFailed, errorSummary: ErrIrreversible.Error(), preExecution: true})
			return ErrIrreversible
		}
		sqlToRun = rev.DownSQL
	}

	execID, err := w.insertExecution(ctx, job, m, sqlToRun)
	if err != nil {
		w.finishJob(ctx, job, m, 0, jobOutcome{status: JobFailed, errorSummary: "gagal mencatat riwayat eksekusi batch", preExecution: true})
		return err
	}
	ctx = telemetry.WithMigrationMeta(ctx, telemetry.MigrationMeta{MigrationID: m.ID, JobID: job.ID, ExecutionID: execID, Attempt: job.Attempt})
	ctx, span := telemetry.StartSpan(ctx, "worker.execute_batch",
		attribute.Int64("execution.id", execID),
		attribute.String("batch.table", cfg.TableSchema+"."+cfg.TableName),
		attribute.Int("batch.size", cfg.BatchSize),
	)
	defer span.End()

	_, _ = w.store.db.ExecContext(ctx, `UPDATE mg_migrations SET has_executed = TRUE, executed_by = $2, executed_at = NOW(), updated_at = NOW() WHERE id = $1`, m.ID, job.RequestedBy)

	targetDB, _, err := w.store.targets.OpenTargetDB(ctx, job.DatabaseID)
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	defer targetDB.Close()

	if err := ensureTargetTrackingTables(ctx, targetDB); err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}

	rootJobID := job.RootJobID
	if rootJobID == 0 {
		rootJobID = job.ID
	}
	if isCompensate {
		return w.runReverseBatchCompensation(ctx, targetDB, job, rootJobID, m, sqlToRun, execID)
	}
	return w.runForwardBatches(ctx, targetDB, job, rootJobID, m, cfg, sqlToRun, execID)
}

func (w *Worker) runForwardBatches(ctx context.Context, targetDB *sql.DB, job Job, rootJobID int64, m Migration, cfg BatchConfig, upSQL string, execID int64) error {
	qualifiedTable := pq.QuoteIdentifier(cfg.TableSchema) + "." + pq.QuoteIdentifier(cfg.TableName)
	quotedKey := pq.QuoteIdentifier(cfg.KeyColumn)

	committedCPs, err := loadTargetBatchCheckpoints(ctx, targetDB, rootJobID, "up")
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}

	var lastCursor, maxCursor, totalEstimated, committedRows int64
	committedBatches := len(committedCPs)
	nextBatchNo := 1
	for _, cp := range committedCPs {
		committedRows += cp.RowsAffected
		if nextBatchNo == 1 || cp.EndCursor > lastCursor {
			lastCursor = cp.EndCursor
		}
		if cp.BatchNo >= nextBatchNo {
			nextBatchNo = cp.BatchNo + 1
		}
		_ = w.upsertPlatformCheckpoint(ctx, rootJobID, execID, "up", cp.BatchNo, cp.StartCursor, cp.EndCursor, cp.RowsAffected, "committed")
	}

	hasRows := false
	if committedBatches > 0 {
		_ = w.store.db.QueryRowContext(ctx, `SELECT max_cursor, total_rows_estimated FROM mg_jobs WHERE id = $1`, rootJobID).Scan(&maxCursor, &totalEstimated)
		hasRows = totalEstimated > 0
	}
	if !hasRows {
		scopeQuery := fmt.Sprintf("SELECT MIN(%s), MAX(%s), COUNT(*) FROM %s", quotedKey, quotedKey, qualifiedTable)
		var minVal, maxVal sql.NullInt64
		if err := telemetry.ObserveSQL(ctx, job.DatabaseID, scopeQuery, nil, func() (int64, error) {
			return 1, targetDB.QueryRowContext(ctx, scopeQuery).Scan(&minVal, &maxVal, &totalEstimated)
		}); err != nil {
			w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
			return err
		}
		if minVal.Valid && maxVal.Valid && totalEstimated > 0 {
			hasRows = true
			maxCursor = maxVal.Int64
			if committedBatches == 0 {
				lastCursor = minVal.Int64 - 1
			}
		}
	}

	dbCtx, cancel := detachedWriteCtx(ctx)
	_, _ = w.store.db.ExecContext(dbCtx, `
		UPDATE mg_jobs SET total_rows_estimated = $2, max_cursor = $3, last_cursor = $4, committed_batches = $5, committed_rows = $6, updated_at = NOW()
		WHERE id IN ($1, $7)`, job.ID, totalEstimated, maxCursor, lastCursor, committedBatches, committedRows, rootJobID)
	cancel()

	for hasRows && lastCursor < maxCursor {
		if ctx.Err() != nil {
			w.failOrRetry(ctx, job, m, execID, jobOutcome{
				status: JobPartial, committedBatches: committedBatches, committedRows: committedRows,
				lastCursor: lastCursor, maxCursor: maxCursor, errorSummary: formatSafeErrorSummary(ctx.Err()),
			}, ctx.Err())
			return ctx.Err()
		}

		batchCtx := telemetry.WithMigrationMeta(ctx, telemetry.MigrationMeta{
			MigrationID: m.ID, JobID: job.ID, ExecutionID: execID, Attempt: job.Attempt, BatchNo: nextBatchNo,
		})
		batchCtx, batchSpan := telemetry.StartSpan(batchCtx, "worker.batch.tx",
			attribute.Int("batch.no", nextBatchNo), attribute.Int64("batch.start_cursor", lastCursor))

		endCursorQuery := fmt.Sprintf(`
			SELECT MAX(k) FROM (
				SELECT %s AS k FROM %s WHERE %s > $1 AND %s <= $2 ORDER BY %s ASC LIMIT $3
			) AS batch_keys`, quotedKey, qualifiedTable, quotedKey, quotedKey, quotedKey)
		var endCursorNull sql.NullInt64
		if err := telemetry.ObserveSQL(batchCtx, job.DatabaseID, endCursorQuery, []any{lastCursor, maxCursor, cfg.BatchSize}, func() (int64, error) {
			return 1, targetDB.QueryRowContext(batchCtx, endCursorQuery, lastCursor, maxCursor, cfg.BatchSize).Scan(&endCursorNull)
		}); err != nil {
			batchSpan.End()
			w.failOrRetry(batchCtx, job, m, execID, jobOutcome{
				status: JobPartial, committedBatches: committedBatches, committedRows: committedRows,
				lastCursor: lastCursor, maxCursor: maxCursor, errorSummary: formatSafeErrorSummary(err),
			}, err)
			return err
		}
		if !endCursorNull.Valid || endCursorNull.Int64 <= lastCursor {
			batchSpan.End()
			break
		}
		endCursor := endCursorNull.Int64

		rowsAffected, commitErr, uncertain := w.execBatchTx(batchCtx, targetDB, job.DatabaseID, rootJobID, execID, "up", nextBatchNo, lastCursor, endCursor, upSQL)
		batchSpan.SetAttributes(attribute.Int64("batch.end_cursor", endCursor), attribute.Int64("batch.rows_affected", rowsAffected))
		batchSpan.End()

		if uncertain {
			w.finishJob(batchCtx, job, m, execID, jobOutcome{
				status: JobUncertain, committedBatches: committedBatches, committedRows: committedRows,
				lastCursor: lastCursor, maxCursor: maxCursor,
				errorSummary: fmt.Sprintf("hasil commit batch #%d belum pasti; verifikasi target DB", nextBatchNo),
			})
			return commitErr
		}
		if commitErr != nil {
			w.failOrRetry(batchCtx, job, m, execID, jobOutcome{
				status: JobPartial, committedBatches: committedBatches, committedRows: committedRows,
				lastCursor: lastCursor, maxCursor: maxCursor, errorSummary: formatSafeErrorSummary(commitErr),
			}, commitErr)
			return commitErr
		}

		_ = w.upsertPlatformCheckpoint(batchCtx, rootJobID, execID, "up", nextBatchNo, lastCursor, endCursor, rowsAffected, "committed")
		committedBatches++
		committedRows += rowsAffected
		lastCursor = endCursor
		nextBatchNo++
		w.updateProgress(batchCtx, job.ID, execID, committedBatches, committedRows, lastCursor)
	}

	w.finishJob(ctx, job, m, execID, jobOutcome{
		status: JobSucceeded, committedBatches: committedBatches, committedRows: committedRows,
		lastCursor: lastCursor, maxCursor: maxCursor,
	})
	return nil
}

func (w *Worker) runReverseBatchCompensation(ctx context.Context, targetDB *sql.DB, job Job, rootJobID int64, m Migration, downSQL string, execID int64) error {
	upCPs, err := loadTargetBatchCheckpoints(ctx, targetDB, rootJobID, "up")
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	downCPs, err := loadTargetBatchCheckpoints(ctx, targetDB, rootJobID, "down")
	if err != nil {
		w.failOrRetry(ctx, job, m, execID, jobOutcome{errorSummary: formatSafeErrorSummary(err)}, err)
		return err
	}
	revertedSet := make(map[int]bool, len(downCPs))
	revertedBatches := len(downCPs)
	var revertedRows int64
	for _, d := range downCPs {
		revertedSet[d.BatchNo] = true
		revertedRows += d.RowsAffected
	}

	for i := len(upCPs) - 1; i >= 0; i-- {
		cp := upCPs[i]
		if revertedSet[cp.BatchNo] {
			continue
		}
		batchCtx := telemetry.WithMigrationMeta(ctx, telemetry.MigrationMeta{
			MigrationID: m.ID, JobID: job.ID, ExecutionID: execID, Attempt: job.Attempt, BatchNo: cp.BatchNo,
		})
		batchCtx, batchSpan := telemetry.StartSpan(batchCtx, "worker.batch.compensate_tx",
			attribute.Int("batch.no", cp.BatchNo), attribute.Int64("batch.start_cursor", cp.StartCursor), attribute.Int64("batch.end_cursor", cp.EndCursor))

		rowsAffected, commitErr, uncertain := w.execBatchTx(batchCtx, targetDB, job.DatabaseID, rootJobID, execID, "down", cp.BatchNo, cp.StartCursor, cp.EndCursor, downSQL)
		batchSpan.SetAttributes(attribute.Int64("batch.rows_affected", rowsAffected))
		batchSpan.End()

		if uncertain {
			w.finishJob(batchCtx, job, m, execID, jobOutcome{
				status: JobUncertain, committedBatches: revertedBatches, committedRows: revertedRows,
				errorSummary: fmt.Sprintf("hasil kompensasi batch #%d belum pasti; verifikasi target DB", cp.BatchNo),
			})
			return commitErr
		}
		if commitErr != nil {
			failStatus := JobPartiallyReverted
			if revertedBatches == 0 {
				failStatus = JobFailed
			}
			w.failOrRetry(batchCtx, job, m, execID, jobOutcome{
				status: failStatus, committedBatches: revertedBatches, committedRows: revertedRows,
				errorSummary: formatSafeErrorSummary(commitErr),
			}, commitErr)
			return commitErr
		}

		_ = w.upsertPlatformCheckpoint(batchCtx, rootJobID, execID, "down", cp.BatchNo, cp.StartCursor, cp.EndCursor, rowsAffected, "reverted")
		pCtx, pCancel := detachedWriteCtx(batchCtx)
		_, _ = w.store.db.ExecContext(pCtx, `UPDATE mg_batch_checkpoints SET status = 'reverted' WHERE job_id = $1 AND direction = 'up' AND batch_no = $2`, rootJobID, cp.BatchNo)
		pCancel()

		revertedBatches++
		revertedRows += rowsAffected
		w.updateProgress(batchCtx, job.ID, execID, revertedBatches, revertedRows, cp.StartCursor)
	}

	w.finishJob(ctx, job, m, execID, jobOutcome{
		status: JobSucceeded, committedBatches: revertedBatches, committedRows: revertedRows,
	})
	return nil
}

// execBatchTx executes one batch SQL + checkpoint insert atomically, probing on a fresh connection if commit is ambiguous.
func (w *Worker) execBatchTx(ctx context.Context, targetDB *sql.DB, databaseID string, rootJobID, execID int64, direction string, batchNo int, startCursor, endCursor int64, query string) (int64, error, bool) {
	tx, err := targetDB.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return 0, err, false
	}
	var rowsAffected int64
	if err := telemetry.ObserveSQL(ctx, databaseID, query, []any{startCursor, endCursor}, func() (int64, error) {
		res, eErr := tx.ExecContext(ctx, query, startCursor, endCursor)
		if eErr != nil {
			return 0, eErr
		}
		if res != nil {
			rowsAffected, _ = res.RowsAffected()
		}
		return rowsAffected, nil
	}); err != nil {
		_ = tx.Rollback()
		telemetry.LogSQLRollback(ctx, databaseID, "batch_tx_failed", nil)
		return 0, err, false
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO _mg_batch_checkpoints (job_id, direction, batch_no, execution_id, start_cursor, end_cursor, rows_affected)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		rootJobID, direction, batchNo, execID, startCursor, endCursor, rowsAffected); err != nil {
		_ = tx.Rollback()
		telemetry.LogSQLRollback(ctx, databaseID, "batch_checkpoint_failed", nil)
		return 0, err, false
	}
	commitStart := time.Now()
	err = tx.Commit()
	telemetry.LogSQLCommit(ctx, databaseID, time.Since(commitStart), err)
	if err != nil {
		if exists, ok := w.probeBatchCheckpoint(databaseID, rootJobID, direction, batchNo); ok {
			if exists {
				return rowsAffected, nil, false
			}
			return 0, err, false
		}
		return 0, err, true
	}
	return rowsAffected, nil, false
}

func (w *Worker) probeStandardCommit(databaseID string, jobID int64) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, _, err := w.store.targets.OpenTargetDB(ctx, databaseID)
	if err != nil {
		return false, false
	}
	defer db.Close()
	exists, err := targetStandardJobCommitted(ctx, db, jobID)
	return exists, err == nil
}

func (w *Worker) probeBatchCheckpoint(databaseID string, rootJobID int64, direction string, batchNo int) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	db, _, err := w.store.targets.OpenTargetDB(ctx, databaseID)
	if err != nil {
		return false, false
	}
	defer db.Close()
	var exists bool
	err = db.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM _mg_batch_checkpoints WHERE job_id = $1 AND direction = $2 AND batch_no = $3)`,
		rootJobID, direction, batchNo).Scan(&exists)
	return exists, err == nil
}

func (w *Worker) failOrRetry(ctx context.Context, job Job, m Migration, execID int64, out jobOutcome, runErr error) {
	if out.status == "" {
		out.status = JobFailed
	}
	if out.status == JobPartial && out.committedBatches == 0 {
		out.status = JobFailed
	}
	// If transient error and attempts remain (and context wasn't cancelled by lost lock), schedule bounded retry.
	if ctx.Err() == nil && IsTransientError(runErr) && job.Attempt < job.MaxAttempts {
		dbCtx, cancel := detachedWriteCtx(ctx)
		defer cancel()
		backoff := RetryBackoff(job.Attempt)
		if execID > 0 {
			_, _ = w.store.db.ExecContext(dbCtx, `
				UPDATE mg_executions SET status = 'failed', committed_batches = $2, committed_rows = $3, error_summary = $4, finished_at = NOW()
				WHERE id = $1`, execID, out.committedBatches, out.committedRows, out.errorSummary)
		}
		_, _ = w.store.db.ExecContext(dbCtx, `
			UPDATE mg_jobs
			SET status = 'queued', next_run_at = NOW() + ($2 * INTERVAL '1 millisecond'),
			    committed_batches = $3, committed_rows = $4, last_cursor = $5, max_cursor = $6,
			    error_summary = $7, updated_at = NOW()
			WHERE id = $1 AND status = 'running' AND worker_id = $8`,
			job.ID, backoff.Milliseconds(), out.committedBatches, out.committedRows, out.lastCursor, out.maxCursor,
			fmt.Sprintf("retry %d/%d dijadwalkan: %s", job.Attempt, job.MaxAttempts, out.errorSummary), job.WorkerID)
		telemetry.LogEvent(ctx, slog.LevelWarn, "migration.retry_scheduled", "retrying",
			slog.Int64("job_id", job.ID), slog.Int("attempt", job.Attempt), slog.String("backoff", backoff.String()))
		return
	}
	w.finishJob(ctx, job, m, execID, out)
}

// finishJob is the single unified terminal state transition for all standard & batch jobs.
// Uses context.WithoutCancel + worker_id guard so cancelled/lease-lost workers never corrupt recovered state.
func (w *Worker) finishJob(ctx context.Context, job Job, m Migration, execID int64, out jobOutcome) {
	dbCtx, cancel := detachedWriteCtx(ctx)
	defer cancel()

	res, err := w.store.db.ExecContext(dbCtx, `
		UPDATE mg_jobs
		SET status = $2, committed_batches = $3, committed_rows = $4, last_cursor = $5, max_cursor = $6,
		    error_summary = $7, finished_at = NOW(), updated_at = NOW()
		WHERE id = $1 AND status = 'running' AND worker_id = $8`,
		job.ID, out.status, out.committedBatches, out.committedRows, out.lastCursor, out.maxCursor, out.errorSummary, job.WorkerID)
	if err != nil {
		return
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// Another recovery sweep already transitioned this job after lock lease expiry.
		return
	}

	if execID > 0 {
		_, _ = w.store.db.ExecContext(dbCtx, `
			UPDATE mg_executions
			SET status = $2, committed_batches = $3, committed_rows = $4, error_summary = $5, finished_at = NOW()
			WHERE id = $1`, execID, out.status, out.committedBatches, out.committedRows, out.errorSummary)
	}

	rootJobID := job.RootJobID
	if rootJobID == 0 {
		rootJobID = job.ID
	}
	isRevertOp := job.Operation == OpRollback || job.Operation == OpCancelBatch

	switch out.status {
	case JobSucceeded:
		_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_review_requests SET status = 'completed', updated_at = NOW() WHERE id = $1`, job.ReviewRequestID)
		// Clear ALL prior partial/partially_reverted jobs in this batch lineage so DB is cleanly unblocked.
		if isRevertOp {
			_, _ = w.store.db.ExecContext(dbCtx, `
				UPDATE mg_jobs SET status = 'failed', error_summary = 'dibatalkan melalui kompensasi batch', updated_at = NOW()
				WHERE root_job_id = $1 AND id <> $2 AND status IN ('partial', 'partially_reverted')`, rootJobID, job.ID)
			_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET status = 'rolled_back', active_execution_id = NULL, last_error = '', updated_at = NOW() WHERE id = $1`, job.MigrationID)
		} else {
			_, _ = w.store.db.ExecContext(dbCtx, `
				UPDATE mg_jobs SET status = 'succeeded', updated_at = NOW()
				WHERE root_job_id = $1 AND id <> $2 AND status IN ('partial', 'partially_reverted')`, rootJobID, job.ID)
			_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET status = 'applied', active_execution_id = $2, last_error = '', updated_at = NOW() WHERE id = $1`, job.MigrationID, execID)
		}

	case JobPartial, JobPartiallyReverted, JobUncertain:
		_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET status = $2, last_error = $3, updated_at = NOW() WHERE id = $1`, job.MigrationID, out.status, out.errorSummary)

	case JobFailed:
		if out.preExecution {
			// Revalidation failure before SQL ran: never leave OpApply stuck in 'queued'; leave other ops' status intact.
			if job.Operation == OpApply {
				nextMigStatus := "draft"
				if m.HasExecuted {
					nextMigStatus = JobFailed
				}
				_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET status = $2, last_error = $3, updated_at = NOW() WHERE id = $1`, job.MigrationID, nextMigStatus, out.errorSummary)
			} else {
				_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, out.errorSummary)
			}
		} else if isRevertOp {
			// Failed rollback/cancel_batch on batch 0 MUST NOT alter active/partial position.
			_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, out.errorSummary)
		} else {
			_, _ = w.store.db.ExecContext(dbCtx, `UPDATE mg_migrations SET status = 'failed', last_error = $2, updated_at = NOW() WHERE id = $1`, job.MigrationID, out.errorSummary)
		}
	}

	level := slog.LevelInfo
	if out.status != JobSucceeded {
		level = slog.LevelWarn
	}
	telemetry.LogEvent(ctx, level, "migration.job_finished", out.status,
		slog.Int64("migration_id", job.MigrationID),
		slog.Int64("job_id", job.ID),
		slog.Int64("execution_id", execID),
		slog.Int("committed_batches", out.committedBatches),
		slog.Int64("committed_rows", out.committedRows),
	)
}

func (w *Worker) insertExecution(ctx context.Context, job Job, m Migration, sqlExecuted string) (int64, error) {
	var execID int64
	err := w.store.db.QueryRowContext(ctx, `
		INSERT INTO mg_executions (
			database_id, migration_id, job_id, review_request_id,
			migration_version, migration_name, revision, operation, target_execution_id,
			attempt, executed_by, executed_by_username, sql_executed, status
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, 'running')
		RETURNING id`,
		job.DatabaseID, m.ID, job.ID, job.ReviewRequestID,
		m.Version, m.Name, job.Revision, job.Operation, job.TargetExecutionID,
		job.Attempt, job.RequestedBy, job.RequestedByUsername, sqlExecuted,
	).Scan(&execID)
	return execID, err
}

func (w *Worker) updateProgress(ctx context.Context, jobID, execID int64, batches int, rows, cursor int64) {
	dbCtx, cancel := detachedWriteCtx(ctx)
	defer cancel()
	_, _ = w.store.db.ExecContext(dbCtx, `
		UPDATE mg_jobs SET committed_batches = $2, committed_rows = $3, last_cursor = $4, updated_at = NOW() WHERE id = $1`,
		jobID, batches, rows, cursor)
	_, _ = w.store.db.ExecContext(dbCtx, `
		UPDATE mg_executions SET committed_batches = $2, committed_rows = $3 WHERE id = $1`, execID, batches, rows)
}

func (w *Worker) upsertPlatformCheckpoint(ctx context.Context, rootJobID, execID int64, direction string, batchNo int, startCursor, endCursor, rowsAffected int64, status string) error {
	dbCtx, cancel := detachedWriteCtx(ctx)
	defer cancel()
	_, err := w.store.db.ExecContext(dbCtx, `
		INSERT INTO mg_batch_checkpoints (job_id, execution_id, batch_no, direction, start_cursor, end_cursor, rows_affected, status)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (job_id, direction, batch_no) DO UPDATE
		SET status = EXCLUDED.status, rows_affected = EXCLUDED.rows_affected`,
		rootJobID, execID, batchNo, direction, startCursor, endCursor, rowsAffected, status)
	return err
}

func detachedWriteCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
}

func ensureTargetTrackingTables(ctx context.Context, targetDB *sql.DB) error {
	_, err := targetDB.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS _mg_execution_markers (
			job_id BIGINT PRIMARY KEY,
			execution_id BIGINT NOT NULL,
			committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
		CREATE TABLE IF NOT EXISTS _mg_batch_checkpoints (
			job_id BIGINT NOT NULL,
			direction TEXT NOT NULL,
			batch_no INT NOT NULL,
			execution_id BIGINT NOT NULL,
			start_cursor BIGINT NOT NULL,
			end_cursor BIGINT NOT NULL,
			rows_affected BIGINT NOT NULL DEFAULT 0,
			committed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (job_id, direction, batch_no)
		);`)
	return err
}

func targetStandardJobCommitted(ctx context.Context, targetDB *sql.DB, jobID int64) (bool, error) {
	var exists bool
	err := targetDB.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM _mg_execution_markers WHERE job_id = $1)`, jobID).Scan(&exists)
	return exists, err
}

func loadTargetBatchCheckpoints(ctx context.Context, targetDB *sql.DB, rootJobID int64, direction string) ([]BatchCheckpoint, error) {
	rows, err := targetDB.QueryContext(ctx, `
		SELECT job_id, execution_id, batch_no, direction, start_cursor, end_cursor, rows_affected, committed_at
		FROM _mg_batch_checkpoints WHERE job_id = $1 AND direction = $2 ORDER BY batch_no ASC`, rootJobID, direction)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]BatchCheckpoint, 0)
	for rows.Next() {
		var cp BatchCheckpoint
		cp.Status = "committed"
		if err := rows.Scan(&cp.JobID, &cp.ExecutionID, &cp.BatchNo, &cp.Direction, &cp.StartCursor, &cp.EndCursor, &cp.RowsAffected, &cp.CommittedAt); err != nil {
			return nil, err
		}
		out = append(out, cp)
	}
	return out, rows.Err()
}

func formatSafeErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return telemetry.TruncateRunes(telemetry.RedactString(fmt.Sprintf("PostgreSQL error (%s): %s", pqErr.Code, pqErr.Message)), 300)
	}
	return telemetry.TruncateRunes(telemetry.RedactString(err.Error()), 300)
}
