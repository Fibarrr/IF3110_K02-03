package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/lib/pq"
	"go.opentelemetry.io/otel/trace"
)

func TestInitLogsJSONWithTraceAndCorrelationIDs(t *testing.T) {
	var logBuf, traceBuf bytes.Buffer
	rt, err := Init(Config{
		ServiceName:   "test-service",
		LogWriter:     &logBuf,
		TraceWriter:   &traceBuf,
		EnableConsole: true,
	})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer rt.Shutdown(context.Background())

	ctx := WithCorrelationID(context.Background(), "corr-123")
	ctx = WithActor(ctx, Actor{UserID: 7, Username: "alice"})
	ctx = WithDatabaseID(ctx, "orders-db")
	ctx = WithMigrationMeta(ctx, MigrationMeta{MigrationID: 4, JobID: 9, ExecutionID: 12, Attempt: 1, BatchNo: 2})
	ctx, span := StartSpan(ctx, "migration.batch.exec")
	err = ObserveSQL(ctx, "orders-db", "UPDATE orders SET status = $1 WHERE id > $2", []any{"secret-val", 100}, func() (int64, error) {
		return 50, nil
	})
	if err != nil {
		t.Fatalf("ObserveSQL: %v", err)
	}
	LogSQLCommit(ctx, "orders-db", 3*time.Millisecond, nil)
	span.End()

	lines := strings.Split(strings.TrimSpace(logBuf.String()), "\n")
	if len(lines) < 3 {
		t.Fatalf("expected 3 structured JSON log lines, got %d", len(lines))
	}
	if strings.Contains(logBuf.String(), "secret-val") {
		t.Fatalf("string SQL parameter should be masked in logs: %s", logBuf.String())
	}

	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[1]), &rec); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if rec["event"] != "sql.result" || rec["status"] != "ok" || rec["correlation_id"] != "corr-123" || rec["database"] != "orders-db" {
		t.Fatalf("unexpected log record: %#v", rec)
	}
	if rec["trace_id"] == "" || rec["span_id"] == "" || rec["batch"] != float64(2) {
		t.Fatalf("missing trace/batch metadata: %#v", rec)
	}
	if traceBuf.Len() == 0 {
		t.Fatalf("expected OpenTelemetry console exporter output")
	}
}

func TestTraceContextPropagationAcrossQueueCarrier(t *testing.T) {
	var logBuf bytes.Buffer
	rt, err := Init(Config{ServiceName: "queue-test", LogWriter: &logBuf})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer rt.Shutdown(context.Background())

	apiCtx := WithCorrelationID(context.Background(), "corr-queue-99")
	apiCtx, apiSpan := StartSpan(apiCtx, "api.enqueue")
	expectedTraceID := apiSpan.SpanContext().TraceID()
	carrier := InjectTraceCarrier(apiCtx)
	apiSpan.End()

	workerCtx := ExtractTraceCarrier(context.Background(), carrier)
	workerCtx, workerSpan := StartSpan(workerCtx, "worker.execute")
	defer workerSpan.End()

	if got := trace.SpanContextFromContext(workerCtx).TraceID(); got != expectedTraceID {
		t.Fatalf("worker trace ID = %s, want %s", got, expectedTraceID)
	}
	if CorrelationID(workerCtx) != "corr-queue-99" {
		t.Fatalf("worker correlation ID = %q, want corr-queue-99", CorrelationID(workerCtx))
	}
}

func TestRedactSensitiveQuotedAndSQLState(t *testing.T) {
	raw := `postgres://admin:Sup3rSecret!@localhost:5432/app password = 'quoted secret' token="dbl secret" if3110_session=tok123`
	redacted := RedactString(raw)
	for _, secret := range []string{"Sup3rSecret!", "quoted secret", "dbl secret", "tok123"} {
		if strings.Contains(redacted, secret) {
			t.Fatalf("secret %q leaked in %q", secret, redacted)
		}
	}

	if got := SQLState(&pq.Error{Code: "40001"}); got != "40001" {
		t.Fatalf("SQLState = %q, want 40001", got)
	}
	if got := SQLState(errors.New("plain")); got != "" {
		t.Fatalf("SQLState plain = %q, want empty", got)
	}
}

func TestHTTPMiddlewareCapturesDownstreamActorAndDatabase(t *testing.T) {
	var logBuf bytes.Buffer
	rt, err := Init(Config{ServiceName: "http-test", LogWriter: &logBuf})
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	defer rt.Shutdown(context.Background())

	h := HTTPMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := WithActor(r.Context(), Actor{UserID: 15, Username: "bob"})
		_ = WithDatabaseID(ctx, "orders-db")
		w.WriteHeader(http.StatusAccepted)
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/databases/orders-db/review-requests/1/queue", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted || rec.Header().Get("X-Correlation-ID") == "" {
		t.Fatalf("unexpected response: status=%d corr=%q", rec.Code, rec.Header().Get("X-Correlation-ID"))
	}
	var entry map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(logBuf.String())), &entry); err != nil {
		t.Fatalf("invalid JSON log: %v", err)
	}
	if entry["database"] != "orders-db" {
		t.Fatalf("expected database in http.request log: %#v", entry)
	}
	actor, _ := entry["actor"].(map[string]any)
	if actor == nil || actor["username"] != "bob" {
		t.Fatalf("expected downstream actor in http.request log: %#v", entry)
	}
}
