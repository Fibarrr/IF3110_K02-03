package migration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/Fibarrr/IF3110_K02-03/backend/internal/databasemanagement"
	"github.com/lib/pq"
)

const (
	maxSQLBytes          = 256 << 10
	maxExecutionDuration = 5 * time.Minute
	defaultMaxAttempts   = 3
	defaultBatchSize     = 1000
	maxBatchSize         = 100000
)

const (
	ModeStandard = "standard"
	ModeBatch    = "batch"

	OpApply       = "apply"
	OpRollback    = "rollback"
	OpForward     = "forward"
	OpResume      = "resume"
	OpCancelBatch = "cancel_batch"

	ReviewPending     = "pending_review"
	ReviewApproved    = "approved"
	ReviewRejected    = "rejected"
	ReviewInvalidated = "invalidated"
	ReviewQueued      = "queued"
	ReviewCompleted   = "completed"

	JobQueued            = "queued"
	JobRunning           = "running"
	JobSucceeded         = "succeeded"
	JobFailed            = "failed"
	JobPartial           = "partial"
	JobPartiallyReverted = "partially_reverted"
	JobUncertain         = "uncertain"
)

var (
	ErrNotFound          = errors.New("migration tidak ditemukan")
	ErrNotEditable       = errors.New("migration yang pernah dieksekusi bersifat immutable; buat migration baru untuk perbaikan")
	ErrOutOfOrder        = errors.New("urutan migration tidak sesuai")
	ErrAlreadyApplied    = errors.New("migration sudah berstatus aktif")
	ErrAlreadyRunning    = errors.New("migration sedang berjalan atau menunggu di queue")
	ErrIrreversible      = errors.New("migration ditandai irreversible dan tidak dapat di-rollback")
	ErrMissingLeader     = errors.New("semua grup dengan akses database harus memiliki leader aktif sebelum review")
	ErrNoApproverGroups  = errors.New("database belum memiliki grup akses untuk proses approval")
	ErrApprovalInvalid   = errors.New("approval tidak lagi valid karena perubahan grup, leader, atau revisi; ajukan review baru")
	ErrNotApproved       = errors.New("review request belum disetujui oleh semua leader grup terkait")
	ErrNotGroupLeader    = errors.New("hanya leader aktif dari grup terkait yang dapat memberikan keputusan")
	ErrDatabaseBlocked   = errors.New("database sedang tertahan oleh migration partial atau hasil commit belum pasti")
	ErrExecution         = errors.New("SQL migration gagal dijalankan; transaksi di-rollback")
	ErrDirectExecBlocked = errors.New("eksekusi langsung dinonaktifkan di Milestone 2; gunakan alur review, approval, dan queue")
)

var (
	param1Pattern = regexp.MustCompile(`\$1(?:[^0-9]|$)`)
	param2Pattern = regexp.MustCompile(`\$2(?:[^0-9]|$)`)
)

type InputError string

func (e InputError) Error() string { return string(e) }

type BatchConfig struct {
	TableSchema string `json:"tableSchema,omitempty"`
	TableName   string `json:"tableName,omitempty"`
	KeyColumn   string `json:"keyColumn,omitempty"`
	BatchSize   int    `json:"batchSize,omitempty"`
}

type Input struct {
	Name               string       `json:"name"`
	SQL                string       `json:"sql,omitempty"`
	UpSQL              string       `json:"upSQL,omitempty"`
	DownSQL            string       `json:"downSQL,omitempty"`
	Reversible         *bool        `json:"reversible,omitempty"`
	IrreversibleReason string       `json:"irreversibleReason,omitempty"`
	ExecutionMode      string       `json:"executionMode,omitempty"`
	BatchConfig        *BatchConfig `json:"batchConfig,omitempty"`
}

type Migration struct {
	ID                 int64        `json:"id"`
	DatabaseID         string       `json:"databaseId"`
	Version            int64        `json:"version"`
	Name               string       `json:"name"`
	SQL                string       `json:"sql"`
	UpSQL              string       `json:"upSQL"`
	DownSQL            string       `json:"downSQL"`
	Reversible         bool         `json:"reversible"`
	IrreversibleReason string       `json:"irreversibleReason,omitempty"`
	ExecutionMode      string       `json:"executionMode"`
	BatchConfig        *BatchConfig `json:"batchConfig,omitempty"`
	CurrentRevision    int          `json:"currentRevision"`
	HasExecuted        bool         `json:"hasExecuted"`
	ActiveExecutionID  *int64       `json:"activeExecutionId,omitempty"`
	Status             string       `json:"status"`
	CreatedBy          int64        `json:"createdBy"`
	CreatedAt          time.Time    `json:"createdAt"`
	UpdatedAt          time.Time    `json:"updatedAt"`
	ExecutedBy         *int64       `json:"executedBy,omitempty"`
	ExecutedAt         *time.Time   `json:"executedAt,omitempty"`
	LastError          string       `json:"lastError,omitempty"`
}

type MigrationRevision struct {
	ID                 int64        `json:"id"`
	MigrationID        int64        `json:"migrationId"`
	Revision           int          `json:"revision"`
	Name               string       `json:"name"`
	UpSQL              string       `json:"upSQL"`
	DownSQL            string       `json:"downSQL"`
	Reversible         bool         `json:"reversible"`
	IrreversibleReason string       `json:"irreversibleReason,omitempty"`
	ExecutionMode      string       `json:"executionMode"`
	BatchConfig        *BatchConfig `json:"batchConfig,omitempty"`
	CreatedBy          int64        `json:"createdBy"`
	CreatedAt          time.Time    `json:"createdAt"`
}

type ReviewRequestInput struct {
	Operation         string `json:"operation"`
	Revision          int    `json:"revision,omitempty"`
	TargetExecutionID *int64 `json:"targetExecutionId,omitempty"`
	TargetJobID       *int64 `json:"targetJobId,omitempty"`
	Comment           string `json:"comment,omitempty"`
}

type ApprovalInput struct {
	Decision string `json:"decision"`
	Comment  string `json:"comment,omitempty"`
}

type ReviewApproval struct {
	RequestID      int64                        `json:"requestId"`
	GroupID        int64                        `json:"groupId"`
	GroupName      string                       `json:"groupName"`
	Access         accesscontrol.DatabaseAccess `json:"access"`
	LeaderUserID   int64                        `json:"leaderUserId"`
	LeaderUsername string                       `json:"leaderUsername"`
	Decision       string                       `json:"decision"`
	Comment        string                       `json:"comment,omitempty"`
	DecidedBy      *int64                       `json:"decidedBy,omitempty"`
	DecidedAt      *time.Time                   `json:"decidedAt,omitempty"`
}

type ReviewAuditEvent struct {
	ID            int64     `json:"id"`
	RequestID     int64     `json:"requestId"`
	DatabaseID    string    `json:"databaseId"`
	GroupID       *int64    `json:"groupId,omitempty"`
	ActorUserID   int64     `json:"actorUserId"`
	ActorUsername string    `json:"actorUsername,omitempty"`
	EventType     string    `json:"eventType"`
	Decision      string    `json:"decision,omitempty"`
	Comment       string    `json:"comment,omitempty"`
	CreatedAt     time.Time `json:"createdAt"`
}

type ReviewRequest struct {
	ID                  int64              `json:"id"`
	DatabaseID          string             `json:"databaseId"`
	MigrationID         int64              `json:"migrationId"`
	MigrationVersion    int64              `json:"migrationVersion,omitempty"`
	MigrationName       string             `json:"migrationName,omitempty"`
	Revision            int                `json:"revision"`
	Operation           string             `json:"operation"`
	TargetExecutionID   *int64             `json:"targetExecutionId,omitempty"`
	TargetJobID         *int64             `json:"targetJobId,omitempty"`
	RequestedBy         int64              `json:"requestedBy"`
	RequestedByUsername string             `json:"requestedByUsername,omitempty"`
	Status              string             `json:"status"`
	Comment             string             `json:"comment,omitempty"`
	InvalidationReason  string             `json:"invalidationReason,omitempty"`
	Approvals           []ReviewApproval   `json:"approvals"`
	AuditEvents         []ReviewAuditEvent `json:"auditEvents,omitempty"`
	CreatedAt           time.Time          `json:"createdAt"`
	UpdatedAt           time.Time          `json:"updatedAt"`
}

type Job struct {
	ID                  int64      `json:"id"`
	DatabaseID          string     `json:"databaseId"`
	MigrationID         int64      `json:"migrationId"`
	MigrationVersion    int64      `json:"migrationVersion,omitempty"`
	MigrationName       string     `json:"migrationName,omitempty"`
	ReviewRequestID     int64      `json:"reviewRequestId"`
	Revision            int        `json:"revision"`
	Operation           string     `json:"operation"`
	TargetExecutionID   *int64     `json:"targetExecutionId,omitempty"`
	ParentJobID         *int64     `json:"parentJobId,omitempty"`
	RootJobID           int64      `json:"rootJobId,omitempty"`
	RequestedBy         int64      `json:"requestedBy"`
	RequestedByUsername string     `json:"requestedByUsername,omitempty"`
	Status              string     `json:"status"`
	Attempt             int        `json:"attempt"`
	MaxAttempts         int        `json:"maxAttempts"`
	NextRunAt           time.Time  `json:"nextRunAt"`
	WorkerID            string     `json:"workerId,omitempty"`
	TotalRowsEstimated  int64      `json:"totalRowsEstimated"`
	CommittedBatches    int        `json:"committedBatches"`
	CommittedRows       int64      `json:"committedRows"`
	LastCursor          int64      `json:"lastCursor"`
	MaxCursor           int64      `json:"maxCursor"`
	ErrorSummary        string     `json:"errorSummary,omitempty"`
	CreatedAt           time.Time  `json:"createdAt"`
	StartedAt           *time.Time `json:"startedAt,omitempty"`
	FinishedAt          *time.Time `json:"finishedAt,omitempty"`
	UpdatedAt           time.Time  `json:"updatedAt"`
}

type ExecutionHistoryEntry struct {
	ExecutionID        int64      `json:"executionId"`
	DatabaseID         string     `json:"databaseId"`
	MigrationID        int64      `json:"migrationId"`
	JobID              *int64     `json:"jobId,omitempty"`
	ReviewRequestID    *int64     `json:"reviewRequestId,omitempty"`
	MigrationVersion   int64      `json:"migrationVersion"`
	MigrationName      string     `json:"migrationName"`
	Revision           int        `json:"revision"`
	Operation          string     `json:"operation"`
	TargetExecutionID  *int64     `json:"targetExecutionId,omitempty"`
	Attempt            int        `json:"attempt"`
	ExecutedBy         int64      `json:"executedBy"`
	ExecutedByUsername string     `json:"executedByUsername,omitempty"`
	SQLExecuted        string     `json:"sqlExecuted"`
	Status             string     `json:"status"`
	CommittedBatches   int        `json:"committedBatches"`
	CommittedRows      int64      `json:"committedRows"`
	ErrorSummary       string     `json:"errorSummary,omitempty"`
	StartedAt          time.Time  `json:"startedAt"`
	FinishedAt         *time.Time `json:"finishedAt,omitempty"`
}

type BatchCheckpoint struct {
	JobID        int64     `json:"jobId"`
	ExecutionID  int64     `json:"executionId"`
	BatchNo      int       `json:"batchNo"`
	Direction    string    `json:"direction"`
	StartCursor  int64     `json:"startCursor"`
	EndCursor    int64     `json:"endCursor"`
	RowsAffected int64     `json:"rowsAffected"`
	Status       string    `json:"status"`
	CommittedAt  time.Time `json:"committedAt"`
}

type TargetStore interface {
	GetConnection(ctx context.Context, databaseID string) (databasemanagement.DatabaseConnection, error)
	OpenTargetDB(ctx context.Context, databaseID string) (*sql.DB, databasemanagement.DatabaseConnection, error)
}

type AccessChecker interface {
	ListDatabaseApprovers(ctx context.Context, databaseID string) ([]accesscontrol.DatabaseApprover, error)
	EffectiveAccess(ctx context.Context, userID int64, databaseID string) (accesscontrol.DatabaseAccess, error)
}

type normalizedInput struct {
	Name               string
	UpSQL              string
	DownSQL            string
	Reversible         bool
	IrreversibleReason string
	ExecutionMode      string
	BatchConfig        *BatchConfig
}

func validateInput(input Input) (normalizedInput, error) {
	name := strings.TrimSpace(input.Name)
	if name == "" || len(name) > 120 {
		return normalizedInput{}, InputError("nama migration harus berisi 1 sampai 120 karakter")
	}
	upSQL := strings.TrimSpace(input.UpSQL)
	if upSQL == "" {
		upSQL = strings.TrimSpace(input.SQL)
	}
	if upSQL == "" || len(upSQL) > maxSQLBytes {
		return normalizedInput{}, InputError("SQL up migration tidak boleh kosong dan maksimal 256 KiB")
	}
	downSQL := strings.TrimSpace(input.DownSQL)
	if len(downSQL) > maxSQLBytes {
		return normalizedInput{}, InputError("SQL down migration maksimal 256 KiB")
	}
	reason := strings.TrimSpace(input.IrreversibleReason)
	if len(reason) > 500 {
		return normalizedInput{}, InputError("alasan irreversible maksimal 500 karakter")
	}

	reversible := downSQL != ""
	if input.Reversible != nil {
		reversible = *input.Reversible
	}
	if reversible {
		if downSQL == "" {
			return normalizedInput{}, InputError("migration reversible wajib menyertakan SQL down, atau tandai sebagai irreversible beserta alasannya")
		}
		reason = ""
	} else {
		if reason == "" {
			return normalizedInput{}, InputError("migration irreversible wajib menyertakan alasan")
		}
		downSQL = ""
	}

	mode := strings.ToLower(strings.TrimSpace(input.ExecutionMode))
	if mode == "" {
		mode = ModeStandard
	}
	if mode != ModeStandard && mode != ModeBatch {
		return normalizedInput{}, InputError("executionMode harus 'standard' atau 'batch'")
	}

	var batchCfg *BatchConfig
	if mode == ModeBatch {
		if input.BatchConfig == nil {
			return normalizedInput{}, InputError("batchConfig wajib diisi untuk mode batch")
		}
		cfg, err := validateBatchConfig(*input.BatchConfig, upSQL, downSQL, reversible)
		if err != nil {
			return normalizedInput{}, err
		}
		batchCfg = &cfg
	}

	return normalizedInput{
		Name: name, UpSQL: upSQL, DownSQL: downSQL,
		Reversible: reversible, IrreversibleReason: reason,
		ExecutionMode: mode, BatchConfig: batchCfg,
	}, nil
}

func validateBatchConfig(cfg BatchConfig, upSQL, downSQL string, reversible bool) (BatchConfig, error) {
	cfg.TableSchema = strings.TrimSpace(cfg.TableSchema)
	if cfg.TableSchema == "" {
		cfg.TableSchema = "public"
	}
	cfg.TableName = strings.TrimSpace(cfg.TableName)
	cfg.KeyColumn = strings.TrimSpace(cfg.KeyColumn)
	if cfg.KeyColumn == "" {
		cfg.KeyColumn = "id"
	}
	if !isSafeIdentifier(cfg.TableSchema) || !isSafeIdentifier(cfg.TableName) || !isSafeIdentifier(cfg.KeyColumn) {
		return BatchConfig{}, InputError("tableSchema, tableName, dan keyColumn harus berupa identifier PostgreSQL yang valid")
	}
	if cfg.BatchSize == 0 {
		cfg.BatchSize = defaultBatchSize
	}
	if cfg.BatchSize < 1 || cfg.BatchSize > maxBatchSize {
		return BatchConfig{}, InputError(fmt.Sprintf("batchSize harus berada di antara 1 sampai %d", maxBatchSize))
	}
	if !param1Pattern.MatchString(upSQL) || !param2Pattern.MatchString(upSQL) {
		return BatchConfig{}, InputError("SQL up mode batch wajib menggunakan parameter cursor $1 (last_id) dan $2 (end_id)")
	}
	if reversible && (!param1Pattern.MatchString(downSQL) || !param2Pattern.MatchString(downSQL)) {
		return BatchConfig{}, InputError("SQL down mode batch wajib menggunakan parameter cursor $1 (last_id) dan $2 (end_id)")
	}
	return cfg, nil
}

func isSafeIdentifier(ident string) bool {
	if ident == "" || len(ident) > 63 {
		return false
	}
	for i, r := range ident {
		isLetter := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_'
		isDigit := r >= '0' && r <= '9'
		if (i == 0 && !isLetter) || (!isLetter && !isDigit) {
			return false
		}
	}
	return true
}

// FilterHideRollback hides every pair of (cancelled execution, successful rollback/cancel_batch)
// matched strictly by executionId while keeping later forwards and failed attempts visible.
func FilterHideRollback(history []ExecutionHistoryEntry) []ExecutionHistoryEntry {
	hidden := make(map[int64]struct{})
	for _, entry := range history {
		if (entry.Operation == OpRollback || entry.Operation == OpCancelBatch) && entry.Status == JobSucceeded && entry.TargetExecutionID != nil {
			hidden[entry.ExecutionID] = struct{}{}
			hidden[*entry.TargetExecutionID] = struct{}{}
		}
	}
	filtered := make([]ExecutionHistoryEntry, 0, len(history))
	for _, entry := range history {
		if _, hide := hidden[entry.ExecutionID]; !hide {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

func IsTransientError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, databasemanagement.ErrTargetConnection) {
		return true
	}
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		code := string(pqErr.Code)
		if code == "40001" || code == "40P01" || code == "55P03" || code == "57P03" || code == "53300" || strings.HasPrefix(code, "08") {
			return true
		}
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "unexpected eof")
}

func RetryBackoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := time.Second << uint(attempt-1)
	if delay > 30*time.Second {
		return 30 * time.Second
	}
	return delay
}

func approverSnapshotsMatch(snapshot []ReviewApproval, live []accesscontrol.DatabaseApprover) (bool, string) {
	if len(live) == 0 {
		return false, "database tidak lagi memiliki grup akses"
	}
	liveByGroup := make(map[int64]accesscontrol.DatabaseApprover, len(live))
	for _, a := range live {
		if a.LeaderUserID == nil {
			return false, fmt.Sprintf("grup %q belum memiliki leader aktif", a.GroupName)
		}
		liveByGroup[a.GroupID] = a
	}
	if len(snapshot) != len(liveByGroup) {
		return false, "daftar grup yang memiliki akses ke database telah berubah"
	}
	for _, s := range snapshot {
		cur, ok := liveByGroup[s.GroupID]
		if !ok {
			return false, fmt.Sprintf("grup %q tidak lagi memiliki akses ke database", s.GroupName)
		}
		if cur.LeaderUserID == nil || *cur.LeaderUserID != s.LeaderUserID {
			return false, fmt.Sprintf("leader grup %q telah berubah sejak review diajukan", s.GroupName)
		}
	}
	return true, ""
}

func normalizeDatabaseID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }
