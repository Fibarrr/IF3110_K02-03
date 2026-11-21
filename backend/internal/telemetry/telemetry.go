package telemetry

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/lib/pq"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/stdout/stdouttrace"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
)

const tracerName = "github.com/Fibarrr/IF3110_K02-03/backend"

type ctxKey uint8

const (
	correlationKey ctxKey = iota + 1
	actorKey
	databaseKey
	migrationMetaKey
	requestMetaKey
)

// Actor identifies the authenticated user without exposing credentials or session tokens.
type Actor struct {
	UserID        int64  `json:"userId,omitempty"`
	Username      string `json:"username,omitempty"`
	PlatformAdmin bool   `json:"platformAdmin,omitempty"`
}

// MigrationMeta carries migration/job/execution correlation fields into structured logs.
type MigrationMeta struct {
	MigrationID int64
	JobID       int64
	ExecutionID int64
	Attempt     int
	BatchNo     int
}

// requestMeta lets downstream handlers attach actor and databaseID back to the outer HTTP middleware.
type requestMeta struct {
	mu         sync.Mutex
	actor      Actor
	hasActor   bool
	databaseID string
}

type Config struct {
	ServiceName   string
	LogWriter     io.Writer
	TraceWriter   io.Writer
	EnableConsole bool
	PrettyTrace   bool
}

type Runtime struct {
	Logger         *slog.Logger
	TracerProvider *sdktrace.TracerProvider
}

var (
	dsnPasswordPattern = regexp.MustCompile(`(?i)(postgres(?:ql)?://[^:@/\s]+:)([^@/\s]+)(@)`)
	kvSecretPattern    = regexp.MustCompile(`(?i)\b([a-z0-9_-]*(?:password|passwd|secret|token|csrf|session|authorization|cookie))\s*([=:])\s*(?:'[^']*'|"[^"]*"|[^\s,;&"']+)`)
)

// Init configures structured JSON logging (slog) and OpenTelemetry tracing with W3C propagation.
func Init(cfg Config) (*Runtime, error) {
	serviceName := strings.TrimSpace(cfg.ServiceName)
	if serviceName == "" {
		serviceName = "if3110-db-platform"
	}
	logOut := cfg.LogWriter
	if logOut == nil {
		logOut = os.Stderr
	}
	logger := slog.New(slog.NewJSONHandler(logOut, &slog.HandlerOptions{Level: slog.LevelInfo})).
		With(slog.String("service", serviceName))
	slog.SetDefault(logger)

	res, err := resource.New(context.Background(), resource.WithAttributes(semconv.ServiceName(serviceName)))
	if err != nil {
		return nil, fmt.Errorf("create otel resource: %w", err)
	}
	opts := []sdktrace.TracerProviderOption{sdktrace.WithResource(res)}
	if cfg.EnableConsole {
		traceOut := cfg.TraceWriter
		if traceOut == nil {
			traceOut = os.Stdout
		}
		expOpts := []stdouttrace.Option{stdouttrace.WithWriter(traceOut)}
		if cfg.PrettyTrace {
			expOpts = append(expOpts, stdouttrace.WithPrettyPrint())
		}
		exporter, err := stdouttrace.New(expOpts...)
		if err != nil {
			return nil, fmt.Errorf("create otel stdout exporter: %w", err)
		}
		opts = append(opts, sdktrace.WithSyncer(exporter))
	}

	tp := sdktrace.NewTracerProvider(opts...)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return &Runtime{Logger: logger, TracerProvider: tp}, nil
}

func (r *Runtime) Shutdown(ctx context.Context) error {
	if r == nil || r.TracerProvider == nil {
		return nil
	}
	return r.TracerProvider.Shutdown(ctx)
}

func Tracer() trace.Tracer { return otel.Tracer(tracerName) }

func StartSpan(ctx context.Context, name string, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	base := make([]attribute.KeyValue, 0, len(attrs)+4)
	if corr := CorrelationID(ctx); corr != "" {
		base = append(base, attribute.String("correlation.id", corr))
	}
	if dbID := DatabaseIDFromContext(ctx); dbID != "" {
		base = append(base, attribute.String("db.id", dbID))
	}
	if actor, ok := ActorFromContext(ctx); ok && actor.UserID > 0 {
		base = append(base, attribute.Int64("actor.user_id", actor.UserID))
		if actor.Username != "" {
			base = append(base, attribute.String("actor.username", actor.Username))
		}
	}
	base = append(base, attrs...)
	return Tracer().Start(ctx, name, trace.WithAttributes(base...))
}

func RecordSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	redacted := RedactString(err.Error())
	span.RecordError(errors.New(redacted))
	span.SetStatus(codes.Error, redacted)
	if code := SQLState(err); code != "" {
		span.SetAttributes(attribute.String("db.sqlstate", code))
	}
}

func NewCorrelationID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("corr-%d", time.Now().UTC().UnixNano())
	}
	return hex.EncodeToString(raw[:])
}

func WithCorrelationID(ctx context.Context, id string) context.Context {
	id = strings.TrimSpace(id)
	if id == "" {
		id = NewCorrelationID()
	}
	return context.WithValue(ctx, correlationKey, id)
}

func CorrelationID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	id, _ := ctx.Value(correlationKey).(string)
	return id
}

func WithActor(ctx context.Context, actor Actor) context.Context {
	if meta, ok := ctx.Value(requestMetaKey).(*requestMeta); ok && meta != nil {
		meta.mu.Lock()
		meta.actor = actor
		meta.hasActor = true
		meta.mu.Unlock()
	}
	return context.WithValue(ctx, actorKey, actor)
}

func ActorFromContext(ctx context.Context) (Actor, bool) {
	if ctx == nil {
		return Actor{}, false
	}
	if actor, ok := ctx.Value(actorKey).(Actor); ok {
		return actor, true
	}
	if meta, ok := ctx.Value(requestMetaKey).(*requestMeta); ok && meta != nil {
		meta.mu.Lock()
		defer meta.mu.Unlock()
		return meta.actor, meta.hasActor
	}
	return Actor{}, false
}

func WithDatabaseID(ctx context.Context, databaseID string) context.Context {
	dbID := strings.ToLower(strings.TrimSpace(databaseID))
	if meta, ok := ctx.Value(requestMetaKey).(*requestMeta); ok && meta != nil && dbID != "" {
		meta.mu.Lock()
		meta.databaseID = dbID
		meta.mu.Unlock()
	}
	return context.WithValue(ctx, databaseKey, dbID)
}

func DatabaseIDFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if dbID, ok := ctx.Value(databaseKey).(string); ok && dbID != "" {
		return dbID
	}
	if meta, ok := ctx.Value(requestMetaKey).(*requestMeta); ok && meta != nil {
		meta.mu.Lock()
		defer meta.mu.Unlock()
		return meta.databaseID
	}
	return ""
}

func WithMigrationMeta(ctx context.Context, meta MigrationMeta) context.Context {
	return context.WithValue(ctx, migrationMetaKey, meta)
}

func MigrationMetaFromContext(ctx context.Context) (MigrationMeta, bool) {
	if ctx == nil {
		return MigrationMeta{}, false
	}
	meta, ok := ctx.Value(migrationMetaKey).(MigrationMeta)
	return meta, ok
}

// InjectTraceCarrier serializes OpenTelemetry trace context and correlation ID for queue jobs.
func InjectTraceCarrier(ctx context.Context) map[string]string {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	out := make(map[string]string, len(carrier)+1)
	for k, v := range carrier {
		out[k] = v
	}
	if corr := CorrelationID(ctx); corr != "" {
		out["correlation_id"] = corr
	}
	return out
}

// ExtractTraceCarrier restores OpenTelemetry trace context and correlation ID from a queue job.
func ExtractTraceCarrier(ctx context.Context, data map[string]string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(data) == 0 {
		return ctx
	}
	carrier := propagation.MapCarrier{}
	for k, v := range data {
		if k == "correlation_id" {
			ctx = WithCorrelationID(ctx, v)
			continue
		}
		carrier[k] = v
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}

// RedactString masks DSN passwords, session identifiers, and unquoted/quoted secret key-value pairs.
func RedactString(input string) string {
	if input == "" {
		return ""
	}
	out := dsnPasswordPattern.ReplaceAllString(input, "${1}[REDACTED]${3}")
	return kvSecretPattern.ReplaceAllString(out, "${1}${2}[REDACTED]")
}

// MaskParams returns safe log representations without leaking string/byte parameter values.
func MaskParams(params ...any) []string {
	if len(params) == 0 {
		return nil
	}
	masked := make([]string, len(params))
	for i, p := range params {
		switch v := p.(type) {
		case nil:
			masked[i] = "<nil>"
		case int, int32, int64, uint, uint32, uint64, float32, float64, bool:
			masked[i] = fmt.Sprint(v)
		case []byte:
			masked[i] = fmt.Sprintf("<bytes:%d>", len(v))
		case string:
			masked[i] = fmt.Sprintf("<masked:len=%d>", utf8.RuneCountInString(v))
		default:
			masked[i] = fmt.Sprintf("<%T>", v)
		}
	}
	return masked
}

func SQLState(err error) string {
	var pqErr *pq.Error
	if errors.As(err, &pqErr) {
		return string(pqErr.Code)
	}
	return ""
}

func CompactSQL(query string) string {
	return truncateRunes(RedactString(strings.Join(strings.Fields(strings.TrimSpace(query)), " ")), 500)
}

func TruncateRunes(s string, maxRunes int) string { return truncateRunes(s, maxRunes) }

func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 || utf8.RuneCountInString(s) <= maxRunes {
		return s
	}
	runes := []rune(s)
	return string(runes[:maxRunes]) + "...<truncated>"
}

// ObserveSQL logs SQL start & result (with duration, row count, SQLSTATE) and records span errors on failure.
func ObserveSQL(ctx context.Context, databaseID, query string, params []any, fn func() (int64, error)) error {
	LogSQLStart(ctx, databaseID, query, params...)
	started := time.Now()
	rows, err := fn()
	LogSQLResult(ctx, databaseID, query, params, time.Since(started), rows, err)
	if err != nil {
		RecordSpanError(trace.SpanFromContext(ctx), err)
	}
	return err
}

func LogEvent(ctx context.Context, level slog.Level, event, status string, extra ...slog.Attr) {
	attrs := baseLogAttrs(ctx, event, status)
	attrs = append(attrs, extra...)
	slog.Default().LogAttrs(ctx, level, event, attrs...)
}

func LogSQLStart(ctx context.Context, databaseID, query string, params ...any) {
	if databaseID != "" {
		ctx = WithDatabaseID(ctx, databaseID)
	}
	attrs := []slog.Attr{slog.String("query", CompactSQL(query))}
	if masked := MaskParams(params...); len(masked) > 0 {
		attrs = append(attrs, slog.Any("params", masked))
	}
	LogEvent(ctx, slog.LevelInfo, "sql.start", "started", attrs...)
}

func LogSQLResult(ctx context.Context, databaseID, query string, params []any, duration time.Duration, rowsAffected int64, err error) {
	if databaseID != "" {
		ctx = WithDatabaseID(ctx, databaseID)
	}
	attrs := []slog.Attr{
		slog.String("query", CompactSQL(query)),
		slog.Float64("duration_ms", float64(duration.Microseconds())/1000.0),
	}
	if masked := MaskParams(params...); len(masked) > 0 {
		attrs = append(attrs, slog.Any("params", masked))
	}
	if rowsAffected >= 0 {
		attrs = append(attrs, slog.Int64("rows", rowsAffected))
	}
	level, status := slog.LevelInfo, "ok"
	if err != nil {
		level, status = slog.LevelError, "failed"
		attrs = append(attrs, slog.String("error", RedactString(err.Error())))
		if state := SQLState(err); state != "" {
			attrs = append(attrs, slog.String("sqlstate", state))
		}
	}
	LogEvent(ctx, level, "sql.result", status, attrs...)
}

func LogSQLCommit(ctx context.Context, databaseID string, duration time.Duration, err error) {
	if databaseID != "" {
		ctx = WithDatabaseID(ctx, databaseID)
	}
	attrs := []slog.Attr{slog.Float64("duration_ms", float64(duration.Microseconds())/1000.0)}
	level, status := slog.LevelInfo, "committed"
	if err != nil {
		level, status = slog.LevelError, "commit_failed"
		attrs = append(attrs, slog.String("error", RedactString(err.Error())))
		if state := SQLState(err); state != "" {
			attrs = append(attrs, slog.String("sqlstate", state))
		}
	}
	LogEvent(ctx, level, "sql.commit", status, attrs...)
}

func LogSQLRollback(ctx context.Context, databaseID, reason string, err error) {
	if databaseID != "" {
		ctx = WithDatabaseID(ctx, databaseID)
	}
	var attrs []slog.Attr
	if reason != "" {
		attrs = append(attrs, slog.String("reason", RedactString(reason)))
	}
	level, status := slog.LevelInfo, "rolled_back"
	if err != nil {
		level, status = slog.LevelWarn, "rollback_failed"
		attrs = append(attrs, slog.String("error", RedactString(err.Error())))
		if state := SQLState(err); state != "" {
			attrs = append(attrs, slog.String("sqlstate", state))
		}
	}
	LogEvent(ctx, level, "sql.rollback", status, attrs...)
}

func baseLogAttrs(ctx context.Context, event, status string) []slog.Attr {
	attrs := []slog.Attr{
		slog.String("event", event),
		slog.String("status", status),
	}
	if corr := CorrelationID(ctx); corr != "" {
		attrs = append(attrs, slog.String("correlation_id", corr))
	}
	if actor, ok := ActorFromContext(ctx); ok && (actor.UserID > 0 || actor.Username != "") {
		attrs = append(attrs, slog.Group("actor",
			slog.Int64("user_id", actor.UserID),
			slog.String("username", actor.Username),
			slog.Bool("platform_admin", actor.PlatformAdmin),
		))
	}
	if dbID := DatabaseIDFromContext(ctx); dbID != "" {
		attrs = append(attrs, slog.String("database", dbID))
	}
	if meta, ok := MigrationMetaFromContext(ctx); ok {
		if meta.MigrationID > 0 {
			attrs = append(attrs, slog.Int64("migration_id", meta.MigrationID))
		}
		if meta.JobID > 0 {
			attrs = append(attrs, slog.Int64("job_id", meta.JobID))
		}
		if meta.ExecutionID > 0 {
			attrs = append(attrs, slog.Int64("execution_id", meta.ExecutionID))
		}
		if meta.Attempt > 0 {
			attrs = append(attrs, slog.Int("attempt", meta.Attempt))
		}
		if meta.BatchNo > 0 {
			attrs = append(attrs, slog.Int("batch", meta.BatchNo))
		}
	}
	if ctx != nil {
		if sc := trace.SpanContextFromContext(ctx); sc.IsValid() {
			attrs = append(attrs,
				slog.String("trace_id", sc.TraceID().String()),
				slog.String("span_id", sc.SpanID().String()),
			)
		}
	}
	return attrs
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// HTTPMiddleware attaches correlation ID, OpenTelemetry HTTP span, and structured request logs.
func HTTPMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
		corrID := strings.TrimSpace(r.Header.Get("X-Correlation-ID"))
		if corrID == "" || len(corrID) > 64 {
			corrID = NewCorrelationID()
		}
		meta := &requestMeta{}
		ctx = context.WithValue(ctx, requestMetaKey, meta)
		ctx = WithCorrelationID(ctx, corrID)
		w.Header().Set("X-Correlation-ID", corrID)

		ctx, span := StartSpan(ctx, "http."+r.Method+" "+r.URL.Path,
			attribute.String("http.method", r.Method),
			attribute.String("http.path", r.URL.Path),
		)
		defer span.End()

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		started := time.Now()
		next.ServeHTTP(rec, r.WithContext(ctx))
		duration := time.Since(started)

		if dbID := DatabaseIDFromContext(ctx); dbID != "" {
			span.SetAttributes(attribute.String("db.id", dbID))
		}
		if actor, ok := ActorFromContext(ctx); ok && actor.UserID > 0 {
			span.SetAttributes(attribute.Int64("actor.user_id", actor.UserID))
		}
		span.SetAttributes(
			attribute.Int("http.status_code", rec.status),
			attribute.Float64("http.duration_ms", float64(duration.Microseconds())/1000.0),
		)

		level, status := slog.LevelInfo, "ok"
		if rec.status >= 500 {
			level, status = slog.LevelError, "error"
			span.SetStatus(codes.Error, http.StatusText(rec.status))
		} else if rec.status >= 400 {
			level, status = slog.LevelWarn, "rejected"
		}
		LogEvent(ctx, level, "http.request", status,
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.Int("http_status", rec.status),
			slog.Float64("duration_ms", float64(duration.Microseconds())/1000.0),
		)
	})
}
