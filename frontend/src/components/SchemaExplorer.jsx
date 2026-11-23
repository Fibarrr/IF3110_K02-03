import React, { useEffect, useRef, useState } from "react";
import { apiFetch } from "../api.js";

export default function SchemaExplorer({ databaseId, schemas, loading, notify }) {
  const [filter, setFilter] = useState("");
  const [selected, setSelected] = useState(null);
  const [subTab, setSubTab] = useState("columns");
  const [details, setDetails] = useState(null);
  const [detailsLoading, setDetailsLoading] = useState(false);

  const [rowsPage, setRowsPage] = useState(null);
  const [rowsLoading, setRowsLoading] = useState(false);
  const [limit, setLimit] = useState(50);
  const [offset, setOffset] = useState(0);

  const detailsSeq = useRef(0);
  const rowsSeq = useRef(0);

  useEffect(() => {
    setSelected(null);
    setDetails(null);
    setRowsPage(null);
    setOffset(0);
  }, [databaseId]);

  useEffect(() => {
    if (!selected) return;
    const seq = ++detailsSeq.current;
    setDetailsLoading(true);
    apiFetch(
      `/api/databases/${encodeURIComponent(databaseId)}/schemas/${encodeURIComponent(selected.schema)}/tables/${encodeURIComponent(selected.table)}`
    )
      .then((data) => {
        if (seq === detailsSeq.current) setDetails(data);
      })
      .catch((err) => {
        if (seq === detailsSeq.current) notify(err.message, "error");
      })
      .finally(() => {
        if (seq === detailsSeq.current) setDetailsLoading(false);
      });
  }, [databaseId, selected, notify]);

  const fetchRows = (target, nextLimit, nextOffset) => {
    if (!target) return;
    const seq = ++rowsSeq.current;
    setRowsLoading(true);
    apiFetch(
      `/api/databases/${encodeURIComponent(databaseId)}/schemas/${encodeURIComponent(target.schema)}/tables/${encodeURIComponent(target.table)}/rows?limit=${nextLimit}&offset=${nextOffset}`
    )
      .then((data) => {
        if (seq === rowsSeq.current) setRowsPage(data);
      })
      .catch((err) => {
        if (seq === rowsSeq.current) notify(err.message, "error");
      })
      .finally(() => {
        if (seq === rowsSeq.current) setRowsLoading(false);
      });
  };

  useEffect(() => {
    if (selected && subTab === "rows") {
      fetchRows(selected, limit, offset);
    }
  }, [selected, subTab, limit, offset]);

  const query = filter.trim().toLowerCase();
  const filteredSchemas = schemas
    .map((s) => ({
      ...s,
      tables: (s.tables || []).filter(
        (t) => !query || s.name.toLowerCase().includes(query) || t.name.toLowerCase().includes(query)
      ),
    }))
    .filter((s) => s.tables.length > 0 || !query);

  const handleSelectTable = (schemaName, tableName) => {
    setSelected({ schema: schemaName, table: tableName });
    setOffset(0);
    setRowsPage(null);
  };

  return (
    <div className="explorer-layout">
      <aside className="card">
        <h2>Schema &amp; Tabel</h2>
        <input
          type="search"
          value={filter}
          onChange={(e) => setFilter(e.target.value)}
          placeholder="Cari schema / tabel..."
          aria-label="Cari schema atau tabel"
        />
        <div className="schema-tree">
          {loading ? (
            <div className="skeleton-box" />
          ) : filteredSchemas.length === 0 ? (
            <p className="muted">Tidak ada schema atau tabel ditemukan.</p>
          ) : (
            filteredSchemas.map((schema) => (
              <div key={schema.name}>
                <div className="schema-group-title">{schema.name}</div>
                {schema.tables.length === 0 ? (
                  <p className="muted" style={{ margin: "0 0 0.4rem" }}>(Kosong)</p>
                ) : (
                  schema.tables.map((t) => {
                    const isActive = selected?.schema === schema.name && selected?.table === t.name;
                    return (
                      <button
                        key={`${schema.name}.${t.name}`}
                        type="button"
                        className={`table-item-btn ${isActive ? "active" : ""}`}
                        onClick={() => handleSelectTable(schema.name, t.name)}
                      >
                        <span style={{ overflow: "hidden", textOverflow: "ellipsis" }}>{t.name}</span>
                        <span className="muted" style={{ fontSize: "0.75rem" }}>~{t.estimatedRows}</span>
                      </button>
                    );
                  })
                )}
              </div>
            ))
          )}
        </div>
      </aside>

      <section className="card">
        <div className="section-title-row">
          <div>
            <h2>{selected ? `${selected.schema}.${selected.table}` : "Pilih tabel di panel kiri"}</h2>
            <p className="muted" style={{ margin: 0 }}>
              {details ? `${details.type} — ${(details.columns || []).length} kolom` : "Detail kolom, constraint, index, dan baris data."}
            </p>
          </div>
          <div className="row-actions" role="tablist">
            {[
              ["columns", "Kolom"],
              ["constraints", "Constraint"],
              ["indexes", "Index"],
              ["rows", "Data"],
            ].map(([key, label]) => (
              <button
                key={key}
                type="button"
                className={`tab-btn ${subTab === key ? "active" : ""}`}
                onClick={() => setSubTab(key)}
              >
                {label}
              </button>
            ))}
          </div>
        </div>

        {!selected ? (
          <p className="muted">Pilih salah satu tabel untuk menampilkan struktur dan datanya.</p>
        ) : detailsLoading && subTab !== "rows" ? (
          <div className="skeleton-box" />
        ) : subTab === "columns" ? (
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>#</th>
                  <th>Nama Kolom</th>
                  <th>Tipe Data</th>
                  <th>Nullable</th>
                  <th>Default</th>
                  <th>PK</th>
                </tr>
              </thead>
              <tbody>
                {(details?.columns || []).map((c) => (
                  <tr key={c.name}>
                    <td>{c.ordinalPosition}</td>
                    <td><strong>{c.name}</strong></td>
                    <td><code>{c.dataType}</code></td>
                    <td>{c.isNullable ? "YES" : "NO"}</td>
                    <td><code>{c.defaultValue || "-"}</code></td>
                    <td>{c.isPrimaryKey ? <span className="badge badge-info">PK</span> : "-"}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        ) : subTab === "constraints" ? (
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>Nama</th>
                  <th>Tipe</th>
                  <th>Kolom</th>
                  <th>Definisi</th>
                </tr>
              </thead>
              <tbody>
                {(details?.constraints || []).length === 0 ? (
                  <tr><td colSpan={4} className="muted">Tidak ada constraint.</td></tr>
                ) : (
                  details.constraints.map((c) => (
                    <tr key={c.name}>
                      <td><strong>{c.name}</strong></td>
                      <td><span className="badge badge-neutral">{c.type}</span></td>
                      <td>{(c.columns || []).join(", ")}</td>
                      <td><code>{c.definition}</code></td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        ) : subTab === "indexes" ? (
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>Nama Index</th>
                  <th>Primary</th>
                  <th>Unique</th>
                  <th>Kolom</th>
                  <th>Definisi</th>
                </tr>
              </thead>
              <tbody>
                {(details?.indexes || []).length === 0 ? (
                  <tr><td colSpan={5} className="muted">Tidak ada index.</td></tr>
                ) : (
                  details.indexes.map((idx) => (
                    <tr key={idx.name}>
                      <td><strong>{idx.name}</strong></td>
                      <td>{idx.isPrimary ? "Ya" : "Tidak"}</td>
                      <td>{idx.isUnique ? "Ya" : "Tidak"}</td>
                      <td>{(idx.columns || []).join(", ")}</td>
                      <td><code>{idx.definition}</code></td>
                    </tr>
                  ))
                )}
              </tbody>
            </table>
          </div>
        ) : (
          <div>
            <div className="section-title-row">
              <span className="muted">
                {rowsPage
                  ? `Menampilkan ${rowsPage.rows.length} dari ${rowsPage.totalRows} baris (offset ${rowsPage.offset})`
                  : "Memuat baris..."}
              </span>
              <div className="row-actions">
                <select
                  aria-label="Jumlah baris per halaman"
                  style={{ width: "auto" }}
                  value={limit}
                  onChange={(e) => {
                    setLimit(Number(e.target.value));
                    setOffset(0);
                  }}
                >
                  {[25, 50, 100, 200].map((n) => (
                    <option key={n} value={n}>{n} baris</option>
                  ))}
                </select>
                <button
                  type="button"
                  className="btn btn-outline btn-sm"
                  disabled={offset === 0 || rowsLoading}
                  onClick={() => setOffset((o) => Math.max(0, o - limit))}
                >
                  &laquo; Sebelumnya
                </button>
                <button
                  type="button"
                  className="btn btn-outline btn-sm"
                  disabled={!rowsPage?.hasMore || rowsLoading}
                  onClick={() => setOffset((o) => o + limit)}
                >
                  Berikutnya &raquo;
                </button>
                <button
                  type="button"
                  className="btn btn-outline btn-sm"
                  disabled={rowsLoading}
                  onClick={() => fetchRows(selected, limit, offset)}
                >
                  Segarkan
                </button>
              </div>
            </div>

            {rowsLoading ? (
              <div className="skeleton-box" />
            ) : !rowsPage || rowsPage.columns.length === 0 ? (
              <p className="muted">Tidak ada data baris.</p>
            ) : (
              <div className="table-scroll">
                <table className="data-table">
                  <thead>
                    <tr>
                      {rowsPage.columns.map((col) => (
                        <th key={col.name}>{col.name}</th>
                      ))}
                    </tr>
                  </thead>
                  <tbody>
                    {rowsPage.rows.length === 0 ? (
                      <tr>
                        <td colSpan={rowsPage.columns.length} className="muted">
                          Tabel kosong pada halaman ini.
                        </td>
                      </tr>
                    ) : (
                      rowsPage.rows.map((row, rIdx) => (
                        <tr key={rIdx}>
                          {rowsPage.columns.map((col) => {
                            const val = row[col.name];
                            return (
                              <td key={col.name}>
                                {val === null || val === undefined ? (
                                  <span className="muted">NULL</span>
                                ) : typeof val === "object" ? (
                                  <code>{JSON.stringify(val)}</code>
                                ) : (
                                  String(val)
                                )}
                              </td>
                            );
                          })}
                        </tr>
                      ))
                    )}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        )}
      </section>
    </div>
  );
}
