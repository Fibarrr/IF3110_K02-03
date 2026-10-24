package databasemanagement

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/lib/pq"
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
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	Columns    []string `json:"columns"`
	Definition string   `json:"definition"`
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
	checkedAt := time.Now().UTC()
	release, err := v.store.acquireTargetSlot(ctx)
	if err != nil {
		return ConnectionStatus{DatabaseID: databaseID, Connected: false, Error: err.Error(), CheckedAt: checkedAt}, err
	}
	defer release()

	targetDB, conn, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return ConnectionStatus{}, ErrNotFound
		}
		return ConnectionStatus{
			DatabaseID: conn.ID,
			Connected:  false,
			Error:      err.Error(),
			CheckedAt:  checkedAt,
		}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var currentDB, currentUser, version string
	if err := targetDB.QueryRowContext(qCtx, `SELECT current_database(), current_user, version()`).
		Scan(&currentDB, &currentUser, &version); err != nil {
		targetErr := TargetConnectionError{Message: "gagal membaca status koneksi database target"}
		return ConnectionStatus{
			DatabaseID: conn.ID,
			Connected:  false,
			Error:      targetErr.Error(),
			CheckedAt:  checkedAt,
		}, targetErr
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
	release, err := v.store.acquireTargetSlot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		return nil, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	schemaRows, err := targetDB.QueryContext(qCtx, `
		SELECT nspname
		FROM pg_catalog.pg_namespace
		WHERE nspname NOT IN ('pg_catalog', 'information_schema')
		  AND nspname !~ '^pg_toast'
		  AND nspname !~ '^pg_temp_'
		ORDER BY nspname`)
	if err != nil {
		return nil, TargetConnectionError{Message: "gagal membaca daftar schema database target"}
	}
	defer schemaRows.Close()

	schemas := make([]SchemaSummary, 0)
	indexByName := make(map[string]int)
	for schemaRows.Next() {
		var name string
		if err := schemaRows.Scan(&name); err != nil {
			return nil, err
		}
		indexByName[name] = len(schemas)
		schemas = append(schemas, SchemaSummary{Name: name, Tables: make([]TableSummary, 0)})
	}
	if err := schemaRows.Err(); err != nil {
		return nil, err
	}

	tableRows, err := targetDB.QueryContext(qCtx, `
		SELECT n.nspname,
		       c.relname,
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
		  AND n.nspname !~ '^pg_toast'
		  AND n.nspname !~ '^pg_temp_'
		ORDER BY n.nspname, c.relname`)
	if err != nil {
		return nil, TargetConnectionError{Message: "gagal membaca daftar tabel database target"}
	}
	defer tableRows.Close()

	for tableRows.Next() {
		var schemaName string
		var tbl TableSummary
		if err := tableRows.Scan(&schemaName, &tbl.Name, &tbl.Type, &tbl.EstimatedRows); err != nil {
			return nil, err
		}
		idx, ok := indexByName[schemaName]
		if !ok {
			idx = len(schemas)
			indexByName[schemaName] = idx
			schemas = append(schemas, SchemaSummary{Name: schemaName, Tables: make([]TableSummary, 0)})
		}
		schemas[idx].Tables = append(schemas[idx].Tables, tbl)
	}
	return schemas, tableRows.Err()
}

func (v *Viewer) GetTableDetails(ctx context.Context, databaseID, schemaName, tableName string) (TableDetails, error) {
	schemaName = strings.TrimSpace(schemaName)
	tableName = strings.TrimSpace(tableName)
	if schemaName == "" || tableName == "" || isSystemSchema(schemaName) {
		return TableDetails{}, ErrNotFound
	}

	release, err := v.store.acquireTargetSlot(ctx)
	if err != nil {
		return TableDetails{}, err
	}
	defer release()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		return TableDetails{}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	var tableOID int64
	var tableType string
	err = targetDB.QueryRowContext(qCtx, `
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
		WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm')`,
		schemaName, tableName).Scan(&tableOID, &tableType)
	if errors.Is(err, sql.ErrNoRows) {
		return TableDetails{}, ErrNotFound
	}
	if err != nil {
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

	colRows, err := targetDB.QueryContext(qCtx, `
		SELECT a.attname,
		       pg_catalog.format_type(a.atttypid, a.atttypmod) AS data_type,
		       NOT a.attnotnull AS is_nullable,
		       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '') AS default_value,
		       a.attnum AS ordinal_position,
		       EXISTS (
		           SELECT 1
		           FROM pg_catalog.pg_index i
		           WHERE i.indrelid = a.attrelid
		             AND i.indisprimary
		             AND a.attnum = ANY(i.indkey)
		       ) AS is_primary_key
		FROM pg_catalog.pg_attribute a
		LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		WHERE a.attrelid = $1
		  AND a.attnum > 0
		  AND NOT a.attisdropped
		ORDER BY a.attnum`, tableOID)
	if err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca kolom tabel"}
	}
	defer colRows.Close()

	for colRows.Next() {
		var col ColumnInfo
		if err := colRows.Scan(&col.Name, &col.DataType, &col.IsNullable, &col.DefaultValue, &col.OrdinalPosition, &col.IsPrimaryKey); err != nil {
			return TableDetails{}, err
		}
		details.Columns = append(details.Columns, col)
	}
	if err := colRows.Err(); err != nil {
		return TableDetails{}, err
	}

	conRows, err := targetDB.QueryContext(qCtx, `
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
		           SELECT a.attname
		           FROM unnest(con.conkey) WITH ORDINALITY AS k(attnum, ord)
		           JOIN pg_catalog.pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = k.attnum
		           ORDER BY k.ord
		       ), ARRAY[]::text[]) AS column_names,
		       pg_catalog.pg_get_constraintdef(con.oid, true) AS definition
		FROM pg_catalog.pg_constraint con
		WHERE con.conrelid = $1
		ORDER BY con.conname`, tableOID)
	if err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca constraint tabel"}
	}
	defer conRows.Close()

	for conRows.Next() {
		var con ConstraintInfo
		if err := conRows.Scan(&con.Name, &con.Type, pq.Array(&con.Columns), &con.Definition); err != nil {
			return TableDetails{}, err
		}
		if con.Columns == nil {
			con.Columns = make([]string, 0)
		}
		details.Constraints = append(details.Constraints, con)
	}
	if err := conRows.Err(); err != nil {
		return TableDetails{}, err
	}

	idxRows, err := targetDB.QueryContext(qCtx, indexDetailsQuery, tableOID)
	if err != nil {
		return TableDetails{}, TargetConnectionError{Message: "gagal membaca index tabel"}
	}
	defer idxRows.Close()

	for idxRows.Next() {
		var idx IndexInfo
		if err := idxRows.Scan(&idx.Name, &idx.IsUnique, &idx.IsPrimary, pq.Array(&idx.Columns), &idx.Definition); err != nil {
			return TableDetails{}, err
		}
		if idx.Columns == nil {
			idx.Columns = make([]string, 0)
		}
		details.Indexes = append(details.Indexes, idx)
	}
	if err := idxRows.Err(); err != nil {
		return TableDetails{}, err
	}

	return details, nil
}

func (v *Viewer) GetTableRows(ctx context.Context, databaseID, schemaName, tableName string, limit, offset int) (TableRowsPage, error) {
	schemaName = strings.TrimSpace(schemaName)
	tableName = strings.TrimSpace(tableName)
	if schemaName == "" || tableName == "" || isSystemSchema(schemaName) {
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

	release, err := v.store.acquireTargetSlot(ctx)
	if err != nil {
		return TableRowsPage{}, err
	}
	defer release()

	targetDB, _, err := v.store.OpenTargetDB(ctx, databaseID)
	if err != nil {
		return TableRowsPage{}, err
	}
	defer targetDB.Close()

	qCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	tx, err := targetDB.BeginTx(qCtx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal memulai transaksi baca pada database target"}
	}
	defer tx.Rollback()

	var exists bool
	if err := tx.QueryRowContext(qCtx, `
		SELECT EXISTS (
		    SELECT 1
		    FROM pg_catalog.pg_class c
		    JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		    WHERE n.nspname = $1 AND c.relname = $2 AND c.relkind IN ('r', 'p', 'v', 'm')
		)`, schemaName, tableName).Scan(&exists); err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal memeriksa keberadaan tabel"}
	}
	if !exists {
		return TableRowsPage{}, ErrNotFound
	}

	qualifiedTable := pq.QuoteIdentifier(schemaName) + "." + pq.QuoteIdentifier(tableName)

	var totalRows int64
	if err := tx.QueryRowContext(qCtx, "SELECT COUNT(*) FROM "+qualifiedTable).Scan(&totalRows); err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal menghitung jumlah baris tabel"}
	}

	rows, err := tx.QueryContext(qCtx, "SELECT * FROM "+qualifiedTable+" LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return TableRowsPage{}, TargetConnectionError{Message: "gagal membaca data tabel"}
	}
	defer rows.Close()

	colTypes, err := rows.ColumnTypes()
	if err != nil {
		return TableRowsPage{}, err
	}
	columns := make([]DataColumn, len(colTypes))
	for i, ct := range colTypes {
		columns[i] = DataColumn{
			Name:         ct.Name(),
			DatabaseType: ct.DatabaseTypeName(),
		}
	}

	resultRows := make([]map[string]any, 0)
	for rows.Next() {
		rawValues := make([]any, len(columns))
		scanDest := make([]any, len(columns))
		for i := range rawValues {
			scanDest[i] = &rawValues[i]
		}
		if err := rows.Scan(scanDest...); err != nil {
			return TableRowsPage{}, err
		}
		rowMap := make(map[string]any, len(columns))
		for i, col := range columns {
			rowMap[col.Name] = formatCellValue(rawValues[i])
		}
		resultRows = append(resultRows, rowMap)
	}
	if err := rows.Err(); err != nil {
		return TableRowsPage{}, err
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
		if math.IsNaN(val) {
			return "NaN"
		}
		if math.IsInf(val, 1) {
			return "Infinity"
		}
		if math.IsInf(val, -1) {
			return "-Infinity"
		}
		return val
	case float32:
		f64 := float64(val)
		if math.IsNaN(f64) {
			return "NaN"
		}
		if math.IsInf(f64, 1) {
			return "Infinity"
		}
		if math.IsInf(f64, -1) {
			return "-Infinity"
		}
		return val
	default:
		return val
	}
}
