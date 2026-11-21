package databasemanagement

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Fibarrr/IF3110_K02-03/backend/internal/telemetry"
	"github.com/lib/pq"
	"go.opentelemetry.io/otel/attribute"
)

const indexDetailsQuery = `
	SELECT idx.relname AS index_name,
	       i.indisunique AS is_unique,
	       i.indisprimary AS is_primary,
	       COALESCE(ARRAY(
	           SELECT pg_catalog.pg_get_indexdef(i.indexrelid, k.ord::int, true)
	           FROM unnest(i.indkey::smallint[]) WITH ORDINALITY AS k(attnum, ord)
	           ORDER BY k.ord
	       ), ARRAY[]::text[]) AS column_names,
	       pg_catalog.pg_get_indexdef(i.indexrelid) AS definition
	FROM pg_catalog.pg_index i
	JOIN pg_catalog.pg_class idx ON idx.oid = i.indexrelid
	WHERE i.indrelid = $1
	ORDER BY i.indisprimary DESC, idx.relname`

type ConnectionStatus struct {
	DatabaseID      string    `json:"databaseId"`
	Connected       bool      `json:"connected"`
	CurrentDatabase string    `json:"currentDatabase,omitempty"`
	CurrentUser     string    `json:"currentUser,omitempty"`
	ServerVersion   string    `json:"serverVersion,omitempty"`
	Error           string    `json:"error,omitempty"`
	CheckedAt       time.Time `json:"checkedAt"`
}

type SchemaSummary struct {
	Name   string         `json:"name"`
	Tables []TableSummary `json:"tables"`
}

type TableSummary struct {
	Name          string `json:"name"`
	Type          string `json:"type"`
	EstimatedRows int64  `json:"estimatedRows"`
}

type ColumnInfo struct {
	Name            string `json:"name"`
	DataType        string `json:"dataType"`
	IsNullable      bool   `json:"isNullable"`
	DefaultValue    string `json:"defaultValue"`
	OrdinalPosition int    `json:"ordinalPosition"`
	IsPrimaryKey    bool   `json:"isPrimaryKey"`
}

type ConstraintInfo struct {
	Name              string   `json:"name"`
	Type              string   `json:"type"`
	Columns           []string `json:"columns"`
	ReferencedSchema  string   `json:"referencedSchema,omitempty"`
	ReferencedTable   string   `json:"referencedTable,omitempty"`
	ReferencedColumns []string `json:"referencedColumns,omitempty"`
	Definition        string   `json:"definition"`
}

type IndexInfo struct {
	Name       string   `json:"name"`
	IsUnique   bool     `json:"isUnique"`
	IsPrimary  bool     `json:"isPrimary"`
	Columns    []string `json:"columns"`
	Definition string   `json:"definition"`
}

type TableDetails struct {
	Schema      string           `json:"schema"`
	Name        string           `json:"name"`
	Type        string           `json:"type"`
	Columns     []ColumnInfo     `json:"columns"`
	Constraints []ConstraintInfo `json:"constraints"`
	Indexes     []IndexInfo      `json:"indexes"`
}

type DataColumn struct {
	Name         string `json:"name"`
	DatabaseType string `json:"databaseType"`
}

type TableRowsPage struct {
	Schema    string           `json:"schema"`
	Table     string           `json:"table"`
	Columns   []DataColumn     `json:"columns"`
	Rows      []map[string]any `json:"rows"`
	Limit     int              `json:"limit"`
	Offset    int              `json:"offset"`
	TotalRows int64            `json:"totalRows"`
	HasMore   bool             `json:"hasMore"`
}

type Viewer struct {
	store *Store
}

func NewViewer(store *Store) *Viewer {
	return &Viewer{store: store}
}

func (v *Viewer) PingConnection(ctx context.Context, databaseID string) (ConnectionStatus, error) {
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.viewer.ping_connection")
	defer span.End()

	checkedAt := time.Now().UTC()
	targetDB, conn, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		if errors.Is(err, ErrNotFound) {
			return ConnectionStatus{}, ErrNotFound
		}
		return ConnectionStatus{DatabaseID: conn.ID, Connected: false, Error: err.Error(), CheckedAt: checkedAt}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const pingQuery = `SELECT current_database(), current_user, version()`
	var currentDB, currentUser, version string
	if err := telemetry.ObserveSQL(qCtx, conn.ID, pingQuery, nil, func() (int64, error) {
		if scanErr := targetDB.QueryRowContext(qCtx, pingQuery).Scan(&currentDB, &currentUser, &version); scanErr != nil {
			return 0, scanErr
		}
		return 1, nil
	}); err != nil {
		targetErr := TargetConnectionError{Message: "gagal membaca status koneksi database target"}
		return ConnectionStatus{DatabaseID: conn.ID, Connected: false, Error: targetErr.Error(), CheckedAt: checkedAt}, targetErr
	}
	return ConnectionStatus{
		DatabaseID:      conn.ID,
		Connected:       true,
		CurrentDatabase: currentDB,
		CurrentUser:     currentUser,
		ServerVersion:   version,
		CheckedAt:       checkedAt,
	}, nil
}

func (v *Viewer) ListSchemas(ctx context.Context, databaseID string) ([]SchemaSummary, error) {
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.viewer.list_schemas")
	defer span.End()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return nil, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const schemaQuery = `
		SELECT nspname FROM pg_catalog.pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema')
		  AND nspname !~ '^pg_toast' AND nspname !~ '^pg_temp_'
		ORDER BY nspname`
	schemas := make([]SchemaSummary, 0)
	indexByName := make(map[string]int)
	if err := telemetry.ObserveSQL(qCtx, databaseID, schemaQuery, nil, func() (int64, error) {
		rows, qErr := targetDB.QueryContext(qCtx, schemaQuery)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if sErr := rows.Scan(&name); sErr != nil {
				return int64(len(schemas)), sErr
			}
			indexByName[name] = len(schemas)
			schemas = append(schemas, SchemaSummary{Name: name, Tables: make([]TableSummary, 0)})
		}
		return int64(len(schemas)), rows.Err()
	}); err != nil {
		return nil, TargetConnectionError{Message: "gagal membaca daftar schema database target"}
	}

	const tableQuery = `
		SELECT n.nspname, c.relname,
		       CASE c.relkind
		           WHEN 'r' THEN 'BASE TABLE'
		           WHEN 'p' THEN 'PARTITIONED TABLE'
		           WHEN 'v' THEN 'VIEW'
		           WHEN 'm' THEN 'MATERIALIZED VIEW'
		           ELSE 'TABLE'
		       END AS table_type,
		       GREATEST(c.reltuples::bigint, 0) AS estimated_rows
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE c.relkind IN ('r', 'p', 'v', 'm')
		  AND n.nspname NOT IN ('pg_catalog', 'information_schema')
		  AND n.nspname !~ '^pg_toast' AND n.nspname !~ '^pg_temp_'
		  AND c.relname NOT IN ('_mg_execution_markers', '_mg_batch_checkpoints')
		ORDER BY n.nspname, c.relname`
	if err := telemetry.ObserveSQL(qCtx, databaseID, tableQuery, nil, func() (int64, error) {
		rows, qErr := targetDB.QueryContext(qCtx, tableQuery)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		var count int64
		for rows.Next() {
			var schemaName string
			var tbl TableSummary
			if sErr := rows.Scan(&schemaName, &tbl.Name, &tbl.Type, &tbl.EstimatedRows); sErr != nil {
				return count, sErr
			}
			idx, ok := indexByName[schemaName]
			if !ok {
				idx = len(schemas)
				indexByName[schemaName] = idx
				schemas = append(schemas, SchemaSummary{Name: schemaName, Tables: make([]TableSummary, 0)})
			}
			schemas[idx].Tables = append(schemas[idx].Tables, tbl)
			count++
		}
		return count, rows.Err()
	}); err != nil {
		return nil, TargetConnectionError{Message: "gagal membaca daftar tabel database target"}
	}
	return schemas, nil
}

func (v *Viewer) GetTableDetails(ctx context.Context, databaseID, schemaName, tableName string) (TableDetails, error) {
	schemaName = strings.TrimSpace(schemaName)
	tableName = strings.TrimSpace(tableName)
	if schemaName == "" || tableName == "" || isSystemSchema(schemaName) || isInternalTrackingTable(tableName) {
		return TableDetails{}, ErrNotFound
	}
	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.viewer.get_table_details",
		attribute.String("db.schema", schemaName),
		attribute.String("db.table", tableName),
	)
	defer span.End()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return TableDetails{}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	const oidQuery = `
		SELECT c.oid,
		       CASE c.relkind
		           WHEN 'r' THEN 'BASE TABLE'
		           WHEN 'p' THEN 'PARTITIONED TABLE'
		           WHEN 'v' THEN 'VIEW'
		           WHEN 'm' THEN 'MATERIALIZED VIEW'
		           ELSE 'TABLE'
		       END
		FROM pg_catalog.pg_class c
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm')`
	var tableOID int64
	var tableType string
	if err := telemetry.ObserveSQL(qCtx, databaseID, oidQuery, []any{schemaName, tableName}, func() (int64, error) {
		if sErr := targetDB.QueryRowContext(qCtx, oidQuery, schemaName, tableName).Scan(&tableOID, &tableType); sErr != nil {
			return 0, sErr
		}
		return 1, nil
	}); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return TableDetails{}, ErrNotFound
		}
		return TableDetails{}, TargetConnectionError{Message: "gagal memeriksa metadata tabel"}
	}

	details := TableDetails{
		Schema:      schemaName,
		Name:        tableName,
		Type:        tableType,
		Columns:     make([]ColumnInfo, 0),
		Constraints: make([]ConstraintInfo, 0),
		Indexes:     make([]IndexInfo, 0),
	}

	const colQuery = `
		SELECT a.attname,
		       pg_catalog.format_type(a.atttypid, a.atttypmod) AS data_type,
		       NOT a.attnotnull AS is_nullable,
		       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '') AS default_value,
		       a.attnum AS ordinal_position,
		       EXISTS (
		           SELECT 1 FROM pg_catalog.pg_index i
		           WHERE i.indrelid = a.attrelid AND i.indisprimary AND a.attnum = ANY(i.indkey)
		       ) AS is_primary_key
		FROM pg_catalog.pg_attribute a
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
		ORDER BY a.attnum`
	if err := telemetry.ObserveSQL(qCtx, databaseID, colQuery, []any{tableOID}, func() (int64, error) {
		rows, qErr := targetDB.QueryContext(qCtx, colQuery, tableOID)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		for rows.Next() {
			var col ColumnInfo
			if sErr := rows.Scan(&col.Name, &col.DataType, &col.IsNullable, &col.DefaultValue, &col.OrdinalPosition, &col.IsPrimaryKey); sErr != nil {
				return int64(len(details.Columns)), sErr
			}
			details.Columns = append(details.Columns, col)
		}
		return int64(len(details.Columns)), rows.Err()
	}); err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca kolom tabel"}
	}

	const conQuery = `
		SELECT con.conname,
		       CASE con.contype
		           WHEN 'p' THEN 'PRIMARY KEY'
		           WHEN 'f' THEN 'FOREIGN KEY'
		           WHEN 'u' THEN 'UNIQUE'
		           WHEN 'c' THEN 'CHECK'
		           WHEN 'x' THEN 'EXCLUDE'
		           ELSE con.contype::text
		       END AS constraint_type,
		       COALESCE(ARRAY(
		           SELECT a.attname FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		           JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		           ORDER BY k.ord
		       ), ARRAY[]::text[]) AS column_names,
		       COALESCE(ref_ns.nspname, '') AS referenced_schema,
		       COALESCE(ref_tbl.relname, '') AS referenced_table,
		       COALESCE(ARRAY(
		           SELECT a.attname FROM unnest(con.confkey) WITH ORDINALITY AS k(attnum, ord)
		           JOIN pg_catalog.pg_attribute a ON a.attrelid = con.confrelid AND a.attnum = k.attnum
		           ORDER BY k.ord
		       ), ARRAY[]::text[]) AS referenced_columns,
		       pg_catalog.pg_get_constraintdef(con.oid, true) AS definition
		FROM pg_catalog.pg_constraint con
		LEFT JOIN pg_catalog.pg_class ref_tbl ON ref_tbl.oid = con.confrelid
		LEFT JOIN pg_catalog.pg_namespace ref_ns ON ref_ns.oid = ref_tbl.relnamespace
		WHERE con.conrelid = $1
		ORDER BY con.conname`
	if err := telemetry.ObserveSQL(qCtx, databaseID, conQuery, []any{tableOID}, func() (int64, error) {
		rows, qErr := targetDB.QueryContext(qCtx, conQuery, tableOID)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		for rows.Next() {
			var con ConstraintInfo
			if sErr := rows.Scan(&con.Name, &con.Type, pq.Array(&con.Columns), &con.ReferencedSchema, &con.ReferencedTable, pq.Array(&con.ReferencedColumns), &con.Definition); sErr != nil {
				return int64(len(details.Constraints)), sErr
			}
			if con.Columns == nil {
				con.Columns = make([]string, 0)
			}
			details.Constraints = append(details.Constraints, con)
		}
		return int64(len(details.Constraints)), rows.Err()
	}); err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca constraint tabel"}
	}

	if err := telemetry.ObserveSQL(qCtx, databaseID, indexDetailsQuery, []any{tableOID}, func() (int64, error) {
		rows, qErr := targetDB.QueryContext(qCtx, indexDetailsQuery, tableOID)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		for rows.Next() {
			var idx IndexInfo
			if sErr := rows.Scan(&idx.Name, &idx.IsUnique, &idx.IsPrimary, pq.Array(&idx.Columns), &idx.Definition); sErr != nil {
				return int64(len(details.Indexes)), sErr
			}
			if idx.Columns == nil {
				idx.Columns = make([]string, 0)
			}
			details.Indexes = append(details.Indexes, idx)
		}
		return int64(len(details.Indexes)), rows.Err()
	}); err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca index tabel"}
	}

	return details, nil
}

func (v *Viewer) GetTableRows(ctx context.Context, databaseID, schemaName, tableName string, limit, offset int) (TableRowsPage, error) {
	schemaName = strings.TrimSpace(schemaName)
	tableName = strings.TrimSpace(tableName)
	if schemaName == "" || tableName == "" || isSystemSchema(schemaName) || isInternalTrackingTable(tableName) {
		return TableRowsPage{}, ErrNotFound
	}
	if limit < 1 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	if offset < 0 {
		offset = 0
	}

	ctx = telemetry.WithDatabaseID(ctx, databaseID)
	ctx, span := telemetry.StartSpan(ctx, "db.viewer.get_table_rows",
		attribute.String("db.schema", schemaName),
		attribute.String("db.table", tableName),
		attribute.Int("db.limit", limit),
		attribute.Int("db.offset", offset),
	)
	defer span.End()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return TableRowsPage{}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tx, err := targetDB.BeginTx(qCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		telemetry.RecordSpanError(span, err)
		return TableRowsPage{}, TargetConnectionError{Message: "gagal memulai transaksi baca pada database target"}
	}
	defer func() {
		rbErr := tx.Rollback()
		if errors.Is(rbErr, sql.ErrTxDone) {
			rbErr = nil
		}
		telemetry.LogSQLRollback(qCtx, databaseID, "readonly_tx_end", rbErr)
	}()

	const existsQuery = `
		SELECT EXISTS (
		    SELECT 1 FROM pg_catalog.pg_class c
		    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		    WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm')
		)`
	var exists bool
	if err := telemetry.ObserveSQL(qCtx, databaseID, existsQuery, []any{schemaName, tableName}, func() (int64, error) {
		return 1, tx.QueryRowContext(qCtx, existsQuery, schemaName, tableName).Scan(&exists)
	}); err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal memeriksa keberadaan tabel"}
	}
	if !exists {
		return TableRowsPage{}, ErrNotFound
	}

	qualifiedTable := pq.QuoteIdentifier(schemaName) + "." + pq.QuoteIdentifier(tableName)
	countQuery := "SELECT COUNT(*) FROM " + qualifiedTable
	var totalRows int64
	if err := telemetry.ObserveSQL(qCtx, databaseID, countQuery, nil, func() (int64, error) {
		return 1, tx.QueryRowContext(qCtx, countQuery).Scan(&totalRows)
	}); err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal menghitung jumlah baris tabel"}
	}

	selectQuery := "SELECT * FROM " + qualifiedTable + " LIMIT $1 OFFSET $2"
	var columns []DataColumn
	resultRows := make([]map[string]any, 0)
	if err := telemetry.ObserveSQL(qCtx, databaseID, selectQuery, []any{limit, offset}, func() (int64, error) {
		rows, qErr := tx.QueryContext(qCtx, selectQuery, limit, offset)
		if qErr != nil {
			return 0, qErr
		}
		defer rows.Close()
		colTypes, cErr := rows.ColumnTypes()
		if cErr != nil {
			return 0, cErr
		}
		columns = make([]DataColumn, len(colTypes))
		for i, ct := range colTypes {
			columns[i] = DataColumn{Name: ct.Name(), DatabaseType: ct.DatabaseTypeName()}
		}
		for rows.Next() {
			rawValues := make([]any, len(columns))
			scanDest := make([]any, len(columns))
			for i := range rawValues {
				scanDest[i] = &rawValues[i]
			}
			if sErr := rows.Scan(scanDest...); sErr != nil {
				return int64(len(resultRows)), sErr
			}
			rowMap := make(map[string]any, len(columns))
			for i, col := range columns {
				rowMap[col.Name] = formatCellValue(rawValues[i])
			}
			resultRows = append(resultRows, rowMap)
		}
		return int64(len(resultRows)), rows.Err()
	}); err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal membaca data tabel"}
	}

	return TableRowsPage{
		Schema:    schemaName,
		Table:     tableName,
		Columns:   columns,
		Rows:      resultRows,
		Limit:     limit,
		Offset:    offset,
		TotalRows: totalRows,
		HasMore:   int64(offset+len(resultRows)) < totalRows,
	}, nil
}

func isSystemSchema(name string) bool {
	s := strings.ToLower(strings.TrimSpace(name))
	return s == "pg_catalog" || s == "information_schema" || strings.HasPrefix(s, "pg_toast") || strings.HasPrefix(s, "pg_temp")
}

func isInternalTrackingTable(name string) bool {
	s := strings.ToLower(strings.TrimSpace(name))
	return s == "_mg_execution_markers" || s == "_mg_batch_checkpoints"
}

func normalizePagination(rawLimit, rawOffset string) (limit int, offset int, err error) {
	limit = 50
	offset = 0
	if strings.TrimSpace(rawLimit) != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(rawLimit))
		if err != nil || parsed < 1 {
			return 0, 0, InputError("parameter limit harus berupa angka positif")
		}
		if parsed > 200 {
			parsed = 200
		}
		limit = parsed
	}
	if strings.TrimSpace(rawOffset) != "" {
		parsed, err := strconv.Atoi(strings.TrimSpace(rawOffset))
		if err != nil || parsed < 0 {
			return 0, 0, InputError("parameter offset tidak boleh negatif")
		}
		offset = parsed
	}
	return limit, offset, nil
}

func formatCellValue(v any) any {
	switch val := v.(type) {
	case nil:
		return nil
	case []byte:
		return string(val)
	case time.Time:
		return val.UTC().Format(time.RFC3339)
	case float64:
		return formatFloat(val, val)
	case float32:
		return formatFloat(float64(val), val)
	default:
		return val
	}
}

func formatFloat(f64 float64, original any) any {
	switch {
	case math.IsNaN(f64):
		return "NaN"
	case math.IsInf(f64, 1):
		return "Infinity"
	case math.IsInf(f64, -1):
		return "-Infinity"
	default:
		return original
	}
}
