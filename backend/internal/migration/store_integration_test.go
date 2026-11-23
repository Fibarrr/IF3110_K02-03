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
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/envfile"
	"github.com/lib/pq"
)

type pgTestHarness struct {
	ctx      context.Context
	db       *sql.DB
	adminDB  *sql.DB
	acStore  *accesscontrol.Store
	dmStore  *databasemanagement.Store
	mgStore  *Store
	worker   *Worker
	dbID     string
	suffix   string
	cleanup  func()
}

func setupPostgresMigrationHarness(t *testing.T) *pgTestHarness {
	t.Helper()
	envfile.Load(".env", "../.env", "../../.env", "../../../.env")
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
	username, password := "", ""
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
	if err := adminDB.PingContext(ctx); err != nil {
		adminDB.Close()
		t.Fatal(err)
	}

	var random [4]byte
	if _, err := rand.Read(random[:]); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprintf("%x", random[:])
	schemaName := "mg2_test_" + suffix
	if _, err := adminDB.ExecContext(ctx, `CREATE SCHEMA `+pq.QuoteIdentifier(schemaName)); err != nil {
		adminDB.Close()
		t.Fatal(err)
	}

	platformURL := *parsed
	q := platformURL.Query()
	q.Set("options", "-c search_path="+schemaName)
	platformURL.RawQuery = q.Encode()
	db, err := sql.Open("postgres", platformURL.String())
	if err != nil {
		_, _ = adminDB.ExecContext(ctx, `DROP SCHEMA `+pq.QuoteIdentifier(schemaName)+` CASCADE`)
		adminDB.Close()
		t.Fatal(err)
	}

	acStore := accesscontrol.NewStore(db)
	dmStore := databasemanagement.NewStore(db)
	mgStore := NewStore(db, dmStore, acStore)
	if err := acStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := dmStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	if err := mgStore.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	// Platform tables use a fresh schema per harness, while the target database is shared.
	// Give each harness a unique job ID range so stale target-side checkpoints from a previous
	// run cannot be mistaken for checkpoints belonging to this harness.
	suffixValue, err := strconv.ParseInt(suffix, 16, 64)
	if err != nil {
		t.Fatal(err)
	}
	jobIDSeed := (int64(1) << 50) + suffixValue
	if _, err := db.ExecContext(ctx, `SELECT setval(pg_get_serial_sequence('mg_jobs', 'id')::regclass, $1, false)`, jobIDSeed); err != nil {
		t.Fatal(err)
	}

	dbID := "mg2-db-" + suffix
	_, err = dmStore.CreateConnection(ctx, databasemanagement.ConnectionInput{
		ID:           dbID,
		Name:         "Migration M2 Test " + suffix,
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

	worker := NewWorker(mgStore, WorkerConfig{
		WorkerID:     "test-worker-" + suffix,
		PollInterval: 50 * time.Millisecond,
		LockLeaseTTL: 10 * time.Second,
		Concurrency:  1,
	})

	cleanup := func() {
		_ = dmStore.DeleteConnection(ctx, dbID)
		_ = db.Close()
		_, _ = adminDB.ExecContext(ctx, `DROP SCHEMA `+pq.QuoteIdentifier(schemaName)+` CASCADE`)
		_ = adminDB.Close()
	}

	return &pgTestHarness{
		ctx:     ctx,
		db:      db,
		adminDB: adminDB,
		acStore: acStore,
		dmStore: dmStore,
		mgStore: mgStore,
		worker:  worker,
		dbID:    dbID,
		suffix:  suffix,
		cleanup: cleanup,
	}
}

func TestPostgresMigrationM2WorkflowApprovalRollbackForwardAndHistory(t *testing.T) {
	h := setupPostgresMigrationHarness(t)
	defer h.cleanup()

	tableA := "mg2_tbl_a_" + h.suffix
	tableB := "mg2_tbl_b_" + h.suffix
	defer func() {
		target, _, err := h.dmStore.OpenTargetDB(h.ctx, h.dbID)
		if err == nil {
			defer target.Close()
			_, _ = target.ExecContext(h.ctx, `DROP TABLE IF EXISTS public.`+pq.QuoteIdentifier(tableA)+`, public.`+pq.QuoteIdentifier(tableB))
		}
	}()

	// Create 2 groups (backend with view_edit, analytics with view_only) + 2 leaders + 1 regular editor.
	editor, err := h.acStore.CreateUser(h.ctx, "editor_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	leadEdit, err := h.acStore.CreateUser(h.ctx, "lead_edit_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	leadView, err := h.acStore.CreateUser(h.ctx, "lead_view_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	grpEdit, err := h.acStore.CreateGroup(h.ctx, "backend_"+h.suffix)
	if err != nil {
		t.Fatal(err)
	}
	grpView, err := h.acStore.CreateGroup(h.ctx, "analytics_"+h.suffix)
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]int64{{grpEdit.ID, editor.ID}, {grpEdit.ID, leadEdit.ID}, {grpView.ID, leadView.ID}} {
		if err := h.acStore.AddMember(h.ctx, pair[0], pair[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := h.acStore.SetDatabaseAccess(h.ctx, grpEdit.ID, h.dbID, accesscontrol.AccessViewEdit); err != nil {
		t.Fatal(err)
	}
	if err := h.acStore.SetDatabaseAccess(h.ctx, grpView.ID, h.dbID, accesscontrol.AccessViewOnly); err != nil {
		t.Fatal(err)
	}

	// Create migration A & B.
	migA, err := h.mgStore.Create(h.ctx, h.dbID, editor.ID, Input{
		Name:    "Create Table A",
		UpSQL:   `CREATE TABLE public.` + pq.QuoteIdentifier(tableA) + ` (id INT PRIMARY KEY, label TEXT)`,
		DownSQL: `DROP TABLE public.` + pq.QuoteIdentifier(tableA),
	})
	if err != nil {
		t.Fatal(err)
	}
	migB, err := h.mgStore.Create(h.ctx, h.dbID, editor.ID, Input{
		Name:    "Create Table B",
		UpSQL:   `CREATE TABLE public.` + pq.QuoteIdentifier(tableB) + ` (id INT PRIMARY KEY)`,
		DownSQL: `DROP TABLE public.` + pq.QuoteIdentifier(tableB),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Review request must fail when group leaders are not assigned yet.
	if _, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, migA.ID, ActorInfo{UserID: editor.ID, Username: editor.Username}, ReviewRequestInput{Operation: OpApply}); !errors.Is(err, ErrMissingLeader) {
		t.Fatalf("expected ErrMissingLeader, got %v", err)
	}

	// Assign active leaders to BOTH groups (including view_only group).
	if err := h.acStore.SetGroupLeader(h.ctx, grpEdit.ID, leadEdit.ID); err != nil {
		t.Fatal(err)
	}
	if err := h.acStore.SetGroupLeader(h.ctx, grpView.ID, leadView.ID); err != nil {
		t.Fatal(err)
	}

	approveAndRun := func(migID int64, op string, targetExecID *int64) Job {
		t.Helper()
		req, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, migID, ActorInfo{UserID: editor.ID, Username: editor.Username}, ReviewRequestInput{
			Operation:         op,
			TargetExecutionID: targetExecID,
		})
		if err != nil {
			t.Fatalf("CreateReviewRequest(%s): %v", op, err)
		}
		// Non-leader cannot approve.
		if _, err := h.mgStore.RecordApproval(h.ctx, h.dbID, req.ID, grpEdit.ID, ActorInfo{UserID: editor.ID, Username: editor.Username}, ApprovalInput{Decision: "approve"}); !errors.Is(err, ErrNotGroupLeader) {
			t.Fatalf("non-leader approval err = %v, want ErrNotGroupLeader", err)
		}
		// Queueing before full quorum must fail.
		if _, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, req.ID, ActorInfo{UserID: editor.ID, Username: editor.Username}); !errors.Is(err, ErrNotApproved) {
			t.Fatalf("queue before approval err = %v, want ErrNotApproved", err)
		}
		// Both view_edit AND view_only group leaders approve.
		if _, err := h.mgStore.RecordApproval(h.ctx, h.dbID, req.ID, grpEdit.ID, ActorInfo{UserID: leadEdit.ID, Username: leadEdit.Username}, ApprovalInput{Decision: "approve", Comment: "LGTM edit"}); err != nil {
			t.Fatal(err)
		}
		approved, err := h.mgStore.RecordApproval(h.ctx, h.dbID, req.ID, grpView.ID, ActorInfo{UserID: leadView.ID, Username: leadView.Username}, ApprovalInput{Decision: "approve", Comment: "LGTM view"})
		if err != nil || approved.Status != ReviewApproved {
			t.Fatalf("approval failed: status=%q err=%v", approved.Status, err)
		}
		job, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, req.ID, ActorInfo{UserID: editor.ID, Username: editor.Username})
		if err != nil || job.Status != JobQueued {
			t.Fatalf("enqueue failed: job=%#v err=%v", job, err)
		}
		if ok, err := h.worker.ProcessNextJob(h.ctx); err != nil || !ok {
			t.Fatalf("worker ProcessNextJob failed: ok=%v err=%v", ok, err)
		}
		got, err := h.mgStore.GetJob(h.ctx, h.dbID, job.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}

	// Apply A then B.
	if job := approveAndRun(migA.ID, OpApply, nil); job.Status != JobSucceeded {
		t.Fatalf("apply A status = %s (%s)", job.Status, job.ErrorSummary)
	}
	if job := approveAndRun(migB.ID, OpApply, nil); job.Status != JobSucceeded {
		t.Fatalf("apply B status = %s (%s)", job.Status, job.ErrorSummary)
	}

	// Executed migration A must be immutable.
	if _, err := h.mgStore.Update(h.ctx, h.dbID, migA.ID, editor.ID, Input{Name: "Mutated A", UpSQL: "SELECT 1", DownSQL: "SELECT 1"}); !errors.Is(err, ErrNotEditable) {
		t.Fatalf("editing executed migration err = %v, want ErrNotEditable", err)
	}

	// Rolling back A while B is still active must fail (latest active only!).
	if _, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, migA.ID, ActorInfo{UserID: editor.ID, Username: editor.Username}, ReviewRequestInput{Operation: OpRollback}); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("rollback A before B err = %v, want ErrOutOfOrder", err)
	}

	// Rollback B succeeds, then Forward B succeeds.
	if job := approveAndRun(migB.ID, OpRollback, nil); job.Status != JobSucceeded {
		t.Fatalf("rollback B status = %s (%s)", job.Status, job.ErrorSummary)
	}
	if job := approveAndRun(migB.ID, OpForward, nil); job.Status != JobSucceeded {
		t.Fatalf("forward B status = %s (%s)", job.Status, job.ErrorSummary)
	}

	// Failed attempt must still be persisted in append-only DB history (Spec v4 strikethrough revision).
	migFail, err := h.mgStore.Create(h.ctx, h.dbID, editor.ID, Input{
		Name:               "Broken SQL C",
		UpSQL:              "SELECT definitely_missing_pg_function()",
		Reversible:         boolPtr(false),
		IrreversibleReason: "tes kegagalan",
	})
	if err != nil {
		t.Fatal(err)
	}
	if job := approveAndRun(migFail.ID, OpApply, nil); job.Status != JobFailed {
		t.Fatalf("broken migration job status = %s, want failed", job.Status)
	}

	fullHistory, err := h.mgStore.ListHistory(h.ctx, h.dbID, false)
	if err != nil {
		t.Fatal(err)
	}
	// Expected 5 entries: E1(apply A), E2(apply B), E3(rollback B->E2), E4(forward B), E5(failed C).
	if len(fullHistory) != 5 {
		t.Fatalf("fullHistory len = %d, want 5 (%#v)", len(fullHistory), fullHistory)
	}
	if fullHistory[4].Status != JobFailed {
		t.Fatalf("expected failed attempt persisted in DB history, got %s", fullHistory[4].Status)
	}

	hiddenHistory, err := h.mgStore.ListHistory(h.ctx, h.dbID, true)
	if err != nil {
		t.Fatal(err)
	}
	// Hide rollback hides ONLY (E2, E3) pair, leaving E1, E4, E5 visible!
	if len(hiddenHistory) != 3 {
		t.Fatalf("hiddenHistory len = %d, want 3 (%#v)", len(hiddenHistory), hiddenHistory)
	}
}

// TestPostgresMigrationBatchPartialResumeCancelAndLeaderInvalidation exercises fast (<1s)
// batch checkpointing, partial failure, resume, reverse compensation, and leader-change invalidation.
func TestPostgresMigrationBatchPartialResumeCancelAndLeaderInvalidation(t *testing.T) {
	h := setupPostgresMigrationHarness(t)
	defer h.cleanup()

	tbl := "mg2_batch_fast_" + h.suffix
	targetDB, _, err := h.dmStore.OpenTargetDB(h.ctx, h.dbID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = targetDB.ExecContext(h.ctx, `DROP TABLE IF EXISTS public.`+pq.QuoteIdentifier(tbl))
		_ = targetDB.Close()
	}()

	// Create 6 rows (batchSize=2 -> 3 batches) with a CHECK constraint that rejects status='bad_val'.
	if _, err := targetDB.ExecContext(h.ctx, `
		CREATE TABLE public.`+pq.QuoteIdentifier(tbl)+` (
			id BIGSERIAL PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'v1' CHECK (status <> 'bad_val')
		);
		INSERT INTO public.`+pq.QuoteIdentifier(tbl)+` (status) SELECT 'v1' FROM generate_series(1, 6);
	`); err != nil {
		t.Fatal(err)
	}

	lead1, err := h.acStore.CreateUser(h.ctx, "b_lead1_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	lead2, err := h.acStore.CreateUser(h.ctx, "b_lead2_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	grp, err := h.acStore.CreateGroup(h.ctx, "b_grp_"+h.suffix)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.acStore.AddMember(h.ctx, grp.ID, lead1.ID)
	_ = h.acStore.AddMember(h.ctx, grp.ID, lead2.ID)
	_ = h.acStore.SetGroupLeader(h.ctx, grp.ID, lead1.ID)
	_ = h.acStore.SetDatabaseAccess(h.ctx, grp.ID, h.dbID, accesscontrol.AccessViewEdit)
	actor1 := ActorInfo{UserID: lead1.ID, Username: lead1.Username}

	// 1) Batch migration fails on batch #2 (ids 3..4 -> triggers CHECK constraint when id = 4).
	mig, err := h.mgStore.Create(h.ctx, h.dbID, lead1.ID, Input{
		Name:          "Batch partial then cancel",
		UpSQL:         `UPDATE public.` + pq.QuoteIdentifier(tbl) + ` SET status = CASE WHEN id = 4 THEN 'bad_val' ELSE 'v2' END WHERE id > $1 AND id <= $2`,
		DownSQL:       `UPDATE public.` + pq.QuoteIdentifier(tbl) + ` SET status = 'v1' WHERE id > $1 AND id <= $2`,
		ExecutionMode: ModeBatch,
		BatchConfig:   &BatchConfig{TableSchema: "public", TableName: tbl, KeyColumn: "id", BatchSize: 2},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Leader change after queue must invalidate during worker revalidation WITHOUT bricking status='queued'.
	reqInv, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, mig.ID, actor1, ReviewRequestInput{Operation: OpApply})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgStore.RecordApproval(h.ctx, h.dbID, reqInv.ID, grp.ID, actor1, ApprovalInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	invJob, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, reqInv.ID, actor1)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.acStore.SetGroupLeader(h.ctx, grp.ID, lead2.ID); err != nil {
		t.Fatal(err)
	}
	if ok, _ := h.worker.ProcessNextJob(h.ctx); !ok {
		t.Fatalf("expected worker to process invalidated job")
	}
	afterInvJob, _ := h.mgStore.GetJob(h.ctx, h.dbID, invJob.ID)
	afterInvMig, _ := h.mgStore.Get(h.ctx, h.dbID, mig.ID)
	if afterInvJob.Status != JobFailed || afterInvMig.Status != "draft" {
		t.Fatalf("expected failed job + reset draft migration, got job=%s mig=%s", afterInvJob.Status, afterInvMig.Status)
	}

	// Switch back to lead1 and run batch migration -> batch #1 commits (2 rows), batch #2 fails -> partial.
	_ = h.acStore.SetGroupLeader(h.ctx, grp.ID, lead1.ID)
	req1, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, mig.ID, actor1, ReviewRequestInput{Operation: OpApply})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.mgStore.RecordApproval(h.ctx, h.dbID, req1.ID, grp.ID, actor1, ApprovalInput{Decision: "approve"})
	job1, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, req1.ID, actor1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.worker.ProcessNextJob(h.ctx)
	job1Done, _ := h.mgStore.GetJob(h.ctx, h.dbID, job1.ID)
	if job1Done.Status != JobPartial || job1Done.CommittedBatches != 1 || job1Done.CommittedRows != 2 {
		t.Fatalf("expected partial with 1 batch / 2 rows, got %#v", job1Done)
	}

	// Cancel committed batch in reverse order -> reverts ids 1..2 back to 'v1' and unblocks DB.
	reqCancel, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, mig.ID, actor1, ReviewRequestInput{Operation: OpCancelBatch, TargetJobID: &job1.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.mgStore.RecordApproval(h.ctx, h.dbID, reqCancel.ID, grp.ID, actor1, ApprovalInput{Decision: "approve"})
	cancelJob, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, reqCancel.ID, actor1)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = h.worker.ProcessNextJob(h.ctx)
	cancelDone, _ := h.mgStore.GetJob(h.ctx, h.dbID, cancelJob.ID)
	if cancelDone.Status != JobSucceeded {
		t.Fatalf("expected cancel_batch succeeded, got %#v", cancelDone)
	}
	var v2Count int
	_ = targetDB.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM public.`+pq.QuoteIdentifier(tbl)+` WHERE status = 'v2'`).Scan(&v2Count)
	if v2Count != 0 {
		t.Fatalf("expected 0 rows in v2 after reverse batch compensation, got %d", v2Count)
	}
	// Checkpoints query on cancelJob must return the shared root lineage checkpoints.
	cps, err := h.mgStore.ListJobCheckpoints(h.ctx, cancelJob.ID)
	if err != nil || len(cps) < 2 {
		t.Fatalf("expected up+down checkpoints for cancelJob, got checkpoints=%+v err=%v", cps, err)
	}
}

// TestPostgresLargeDatasetBatchMigration satisfies Milestone 2 testing requirement:
// prepares a dataset where a single unbatched UPDATE takes > 1 second, records row count and duration,
// and executes a batched migration on the exact same dataset (plus tests resume & reverse compensation).
func TestPostgresLargeDatasetBatchMigration(t *testing.T) {
	h := setupPostgresMigrationHarness(t)
	defer h.cleanup()

	if os.Getenv("RUN_LARGE_BATCH_BENCHMARK") == "" {
		t.Skip("set RUN_LARGE_BATCH_BENCHMARK=1 bersama TEST_DATABASE_URL untuk menjalankan benchmark dataset besar (>1 detik)")
	}

	tableName := "mg2_large_items_" + h.suffix
	targetDB, _, err := h.dmStore.OpenTargetDB(h.ctx, h.dbID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = targetDB.ExecContext(h.ctx, `DROP TABLE IF EXISTS public.`+pq.QuoteIdentifier(tableName))
		_ = targetDB.Close()
	}()

	_, err = targetDB.ExecContext(h.ctx, `
		CREATE TABLE public.`+pq.QuoteIdentifier(tableName)+` (
			id BIGSERIAL PRIMARY KEY,
			status TEXT NOT NULL DEFAULT 'pending',
			payload TEXT NOT NULL DEFAULT repeat('x', 200),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`)
	if err != nil {
		t.Fatal(err)
	}

	// Grow rows until single unbatched UPDATE takes > 1 second.
	rowCount := int64(250000)
	var unbatchedDuration time.Duration
	for attempt := 0; attempt < 3; attempt++ {
		_, err = targetDB.ExecContext(h.ctx, `
			INSERT INTO public.`+pq.QuoteIdentifier(tableName)+` (status, payload)
			SELECT 'pending', md5(g::text) || repeat('a', 180)
			FROM generate_series(1, $1) AS g`, rowCount)
		if err != nil {
			t.Fatal(err)
		}
		var total int64
		_ = targetDB.QueryRowContext(h.ctx, `SELECT COUNT(*) FROM public.`+pq.QuoteIdentifier(tableName)).Scan(&total)

		start := time.Now()
		_, err = targetDB.ExecContext(h.ctx, `UPDATE public.`+pq.QuoteIdentifier(tableName)+` SET status = 'unbatched', updated_at = NOW()`)
		if err != nil {
			t.Fatal(err)
		}
		unbatchedDuration = time.Since(start)
		t.Logf("Unbatched UPDATE: rows=%d duration=%s", total, unbatchedDuration)
		if unbatchedDuration > time.Second {
			rowCount = total
			break
		}
		// Reset and add more rows so unbatched exceeds 1 second.
		_, _ = targetDB.ExecContext(h.ctx, `UPDATE public.`+pq.QuoteIdentifier(tableName)+` SET status = 'pending'`)
	}
	if unbatchedDuration <= time.Second {
		t.Fatalf("unbatched UPDATE duration (%s) harus > 1 detik", unbatchedDuration)
	}

	// Reset statuses and run batched migration on the exact same dataset.
	_, err = targetDB.ExecContext(h.ctx, `UPDATE public.`+pq.QuoteIdentifier(tableName)+` SET status = 'pending'`)
	if err != nil {
		t.Fatal(err)
	}

	user, err := h.acStore.CreateUser(h.ctx, "batch_lead_"+h.suffix, "PasswordSuperAman123!")
	if err != nil {
		t.Fatal(err)
	}
	grp, err := h.acStore.CreateGroup(h.ctx, "batch_team_"+h.suffix)
	if err != nil {
		t.Fatal(err)
	}
	_ = h.acStore.AddMember(h.ctx, grp.ID, user.ID)
	_ = h.acStore.SetGroupLeader(h.ctx, grp.ID, user.ID)
	_ = h.acStore.SetDatabaseAccess(h.ctx, grp.ID, h.dbID, accesscontrol.AccessViewEdit)

	mig, err := h.mgStore.Create(h.ctx, h.dbID, user.ID, Input{
		Name:          "Batch update large dataset",
		UpSQL:         `UPDATE public.` + pq.QuoteIdentifier(tableName) + ` SET status = 'batched', updated_at = NOW() WHERE id > $1 AND id <= $2`,
		DownSQL:       `UPDATE public.` + pq.QuoteIdentifier(tableName) + ` SET status = 'pending', updated_at = NOW() WHERE id > $1 AND id <= $2`,
		ExecutionMode: ModeBatch,
		BatchConfig: &BatchConfig{
			TableSchema: "public",
			TableName:   tableName,
			KeyColumn:   "id",
			BatchSize:   25000,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.mgStore.CreateReviewRequest(h.ctx, h.dbID, mig.ID, ActorInfo{UserID: user.ID, Username: user.Username}, ReviewRequestInput{Operation: OpApply})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.mgStore.RecordApproval(h.ctx, h.dbID, req.ID, grp.ID, ActorInfo{UserID: user.ID, Username: user.Username}, ApprovalInput{Decision: "approve"}); err != nil {
		t.Fatal(err)
	}
	job, err := h.mgStore.EnqueueReviewRequest(h.ctx, h.dbID, req.ID, ActorInfo{UserID: user.ID, Username: user.Username})
	if err != nil {
		t.Fatal(err)
	}

	batchStart := time.Now()
	if ok, err := h.worker.ProcessNextJob(h.ctx); err != nil || !ok {
		t.Fatalf("batch worker failed: ok=%v err=%v", ok, err)
	}
	batchDuration := time.Since(batchStart)

	doneJob, err := h.mgStore.GetJob(h.ctx, h.dbID, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("Batched UPDATE: rows=%d committedRows=%d committedBatches=%d duration=%s (unbatched=%s)",
		rowCount, doneJob.CommittedRows, doneJob.CommittedBatches, batchDuration, unbatchedDuration)
	if doneJob.Status != JobSucceeded || doneJob.CommittedRows != rowCount {
		t.Fatalf("batch job did not finish cleanly: %#v", doneJob)
	}
}
