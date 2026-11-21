package migration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/accesscontrol"
	"github.com/lib/pq"
)

func boolPtr(v bool) *bool { return &v }
func i64Ptr(v int64) *int64 { return &v }

func TestValidateInput(t *testing.T) {
	tests := []struct {
		name    string
		input   Input
		wantErr bool
	}{
		{
			name: "valid reversible standard",
			input: Input{
				Name:    "Create items table",
				UpSQL:   "CREATE TABLE items (id INT PRIMARY KEY)",
				DownSQL: "DROP TABLE items",
			},
		},
		{
			name: "valid irreversible with reason",
			input: Input{
				Name:               "Drop legacy column",
				UpSQL:              "ALTER TABLE items DROP COLUMN old_col",
				Reversible:         boolPtr(false),
				IrreversibleReason: "data kolom lama tidak dapat dipulihkan",
			},
		},
		{
			name: "valid batch migration",
			input: Input{
				Name:          "Backfill status",
				UpSQL:         "UPDATE public.orders SET status = 'v2' WHERE id > $1 AND id <= $2",
				DownSQL:       "UPDATE public.orders SET status = 'v1' WHERE id > $1 AND id <= $2",
				ExecutionMode: ModeBatch,
				BatchConfig: &BatchConfig{
					TableSchema: "public",
					TableName:   "orders",
					KeyColumn:   "id",
					BatchSize:   5000,
				},
			},
		},
		{
			name: "missing downSQL without irreversible reason",
			input: Input{
				Name:  "No down SQL",
				UpSQL: "SELECT 1",
			},
			wantErr: true,
		},
		{
			name: "reversible explicitly true but empty downSQL",
			input: Input{
				Name:       "Bad reversible",
				UpSQL:      "SELECT 1",
				Reversible: boolPtr(true),
			},
			wantErr: true,
		},
		{
			name: "batch mode missing placeholders",
			input: Input{
				Name:          "Bad batch",
				UpSQL:         "UPDATE public.orders SET status = 'v2'",
				DownSQL:       "UPDATE public.orders SET status = 'v1'",
				ExecutionMode: ModeBatch,
				BatchConfig:   &BatchConfig{TableName: "orders", BatchSize: 500},
			},
			wantErr: true,
		},
		{
			name: "batch mode invalid identifier",
			input: Input{
				Name:          "SQLi table name",
				UpSQL:         "UPDATE orders SET x=1 WHERE id > $1 AND id <= $2",
				DownSQL:       "UPDATE orders SET x=0 WHERE id > $1 AND id <= $2",
				ExecutionMode: ModeBatch,
				BatchConfig:   &BatchConfig{TableName: "orders; DROP TABLE users", BatchSize: 100},
			},
			wantErr: true,
		},
		{name: "empty name", input: Input{UpSQL: "SELECT 1", DownSQL: "SELECT 1"}, wantErr: true},
		{name: "empty SQL", input: Input{Name: "Empty", DownSQL: "SELECT 1"}, wantErr: true},
		{name: "name too long", input: Input{Name: string(make([]byte, 121)), UpSQL: "SELECT 1", DownSQL: "SELECT 1"}, wantErr: true},
		{name: "SQL too long", input: Input{Name: "Large", UpSQL: string(make([]byte, maxSQLBytes+1)), DownSQL: "SELECT 1"}, wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := validateInput(test.input)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateInput() error = %v, wantErr %v", err, test.wantErr)
			}
		})
	}
}

func TestFilterHideRollbackMatchesSpecTable(t *testing.T) {
	// Spec example:
	// Full history: A [E1], B [E2], B (rollback E2) [E3], B (forward) [E4], C [E5]
	// Plus a failed rollback [E6] targeting E4 and a failed apply [E7] which must stay visible.
	history := []ExecutionHistoryEntry{
		{ExecutionID: 1, MigrationName: "A", Operation: OpApply, Status: JobSucceeded},
		{ExecutionID: 2, MigrationName: "B", Operation: OpApply, Status: JobSucceeded},
		{ExecutionID: 3, MigrationName: "B", Operation: OpRollback, TargetExecutionID: i64Ptr(2), Status: JobSucceeded},
		{ExecutionID: 4, MigrationName: "B", Operation: OpForward, Status: JobSucceeded},
		{ExecutionID: 5, MigrationName: "C", Operation: OpApply, Status: JobSucceeded},
		{ExecutionID: 6, MigrationName: "C", Operation: OpRollback, TargetExecutionID: i64Ptr(5), Status: JobFailed},
		{ExecutionID: 7, MigrationName: "D", Operation: OpApply, Status: JobFailed},
	}

	filtered := FilterHideRollback(history)
	wantIDs := []int64{1, 4, 5, 6, 7}
	if len(filtered) != len(wantIDs) {
		t.Fatalf("len(filtered) = %d, want %d (%#v)", len(filtered), len(wantIDs), filtered)
	}
	for i, want := range wantIDs {
		if filtered[i].ExecutionID != want {
			t.Fatalf("filtered[%d].ExecutionID = %d, want %d", i, filtered[i].ExecutionID, want)
		}
	}
}

func TestApproverSnapshotsMatch(t *testing.T) {
	snapshot := []ReviewApproval{
		{GroupID: 1, GroupName: "backend", LeaderUserID: 10},
		{GroupID: 2, GroupName: "analytics", LeaderUserID: 20},
	}

	t.Run("exact match", func(t *testing.T) {
		live := []accesscontrol.DatabaseApprover{
			{GroupID: 1, GroupName: "backend", LeaderUserID: i64Ptr(10)},
			{GroupID: 2, GroupName: "analytics", LeaderUserID: i64Ptr(20)},
		}
		if ok, reason := approverSnapshotsMatch(snapshot, live); !ok {
			t.Fatalf("expected match, got %q", reason)
		}
	})

	t.Run("leader changed invalidates", func(t *testing.T) {
		live := []accesscontrol.DatabaseApprover{
			{GroupID: 1, GroupName: "backend", LeaderUserID: i64Ptr(10)},
			{GroupID: 2, GroupName: "analytics", LeaderUserID: i64Ptr(99)},
		}
		if ok, _ := approverSnapshotsMatch(snapshot, live); ok {
			t.Fatalf("expected mismatch when group leader changed")
		}
	})

	t.Run("new group granted invalidates", func(t *testing.T) {
		live := []accesscontrol.DatabaseApprover{
			{GroupID: 1, GroupName: "backend", LeaderUserID: i64Ptr(10)},
			{GroupID: 2, GroupName: "analytics", LeaderUserID: i64Ptr(20)},
			{GroupID: 3, GroupName: "qa", LeaderUserID: i64Ptr(30)},
		}
		if ok, _ := approverSnapshotsMatch(snapshot, live); ok {
			t.Fatalf("expected mismatch when another group gained access")
		}
	})

	t.Run("missing leader invalidates", func(t *testing.T) {
		live := []accesscontrol.DatabaseApprover{
			{GroupID: 1, GroupName: "backend", LeaderUserID: i64Ptr(10)},
			{GroupID: 2, GroupName: "analytics", LeaderUserID: nil},
		}
		if ok, _ := approverSnapshotsMatch(snapshot, live); ok {
			t.Fatalf("expected mismatch when leader is nil")
		}
	})
}

func TestTransientErrorAndRetryBackoff(t *testing.T) {
	if !IsTransientError(&pq.Error{Code: "40001"}) {
		t.Fatalf("expected serialization failure 40001 to be transient")
	}
	if !IsTransientError(&pq.Error{Code: "40P01"}) {
		t.Fatalf("expected deadlock 40P01 to be transient")
	}
	if !IsTransientError(context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded to be transient")
	}
	if IsTransientError(&pq.Error{Code: "42601"}) {
		t.Fatalf("syntax error 42601 should not be transient")
	}
	if IsTransientError(errors.New("constraint violation")) {
		t.Fatalf("plain semantic error should not be transient")
	}
	if got := RetryBackoff(1); got != time.Second {
		t.Fatalf("RetryBackoff(1) = %s, want 1s", got)
	}
	if got := RetryBackoff(3); got != 4*time.Second {
		t.Fatalf("RetryBackoff(3) = %s, want 4s", got)
	}
	if got := RetryBackoff(10); got != 30*time.Second {
		t.Fatalf("RetryBackoff(10) = %s, want capped 30s", got)
	}
}
