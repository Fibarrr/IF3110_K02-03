import React, { useState } from "react";
import { formatDateTime, statusBadgeClass } from "../api.js";

export default function MigrationJobsHistory({
  jobs,
  migrations,
  history,
  hasActiveJob,
  canEdit,
  hideRollback,
  setHideRollback,
  onSubmitReview,
  onResolveUncertain,
}) {
  const [expandedExecId, setExpandedExecId] = useState(null);

  return (
    <>
      <section className="card">
        <div className="section-title-row">
          <div>
            <h2>Monitor Job Asynchronous &amp; Progres Batch</h2>
            <p className="muted" style={{ margin: 0 }}>
              {hasActiveJob
                ? "Memantau status worker secara otomatis setiap 2 detik..."
                : "Seluruh eksekusi migration berjalan melalui queue dan worker Go."}
            </p>
          </div>
          {hasActiveJob && <span className="badge badge-warn">Worker Aktif</span>}
        </div>

        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Job</th>
                <th>Migration &amp; Operasi</th>
                <th>Status</th>
                <th>Progres Batch / Row</th>
                <th>Attempt &amp; Worker</th>
                <th>Aksi Pemulihan</th>
              </tr>
            </thead>
            <tbody>
              {jobs.length === 0 ? (
                <tr><td colSpan={6} className="muted">Belum ada job yang dijalankan.</td></tr>
              ) : (
                jobs.map((j) => {
                  const mig = migrations.find((m) => m.id === j.migrationId);
                  const pct =
                    j.totalRowsEstimated > 0
                      ? Math.min(100, Math.round((j.committedRows / j.totalRowsEstimated) * 100))
                      : j.status === "succeeded"
                      ? 100
                      : 0;
                  return (
                    <tr key={j.id}>
                      <td><strong>#{j.id}</strong></td>
                      <td>
                        <div>
                          v{j.migrationVersion} {j.migrationName}{" "}
                          <span className="badge badge-info">{j.operation}</span>
                        </div>
                        {j.errorSummary && (
                          <div style={{ color: "#991b1b", fontSize: "0.78rem", marginTop: "0.2rem" }}>
                            {j.errorSummary}
                          </div>
                        )}
                      </td>
                      <td><span className={statusBadgeClass(j.status)}>{j.status}</span></td>
                      <td style={{ minWidth: "180px" }}>
                        {j.committedBatches > 0 || j.totalRowsEstimated > 0 ? (
                          <>
                            <div style={{ fontSize: "0.8rem" }}>
                              {j.committedBatches} batch • {j.committedRows}
                              {j.totalRowsEstimated > 0 ? ` / ${j.totalRowsEstimated}` : ""} row
                            </div>
                            <div className="progress-track">
                              <div className="progress-fill" style={{ width: `${pct}%` }} />
                            </div>
                          </>
                        ) : (
                          <span className="muted">Standard</span>
                        )}
                      </td>
                      <td>
                        <div>Attempt {j.attempt}/{j.maxAttempts}</div>
                        <div className="muted" style={{ fontSize: "0.75rem" }}>{j.workerId || "menunggu worker"}</div>
                      </td>
                      <td>
                        <div className="row-actions">
                          {canEdit && mig && j.status === "partial" && (
                            <button type="button" className="btn btn-primary btn-sm" onClick={() => onSubmitReview(mig, "resume", j.id)}>
                              Ajukan Resume
                            </button>
                          )}
                          {canEdit && mig && (j.status === "partial" || j.status === "partially_reverted") && mig.reversible && (
                            <button type="button" className="btn btn-danger btn-sm" onClick={() => onSubmitReview(mig, "cancel_batch", j.id)}>
                              Ajukan Batalkan Batch
                            </button>
                          )}
                          {canEdit && j.status === "uncertain" && (
                            <>
                              <button type="button" className="btn btn-success btn-sm" onClick={() => onResolveUncertain(j.id, "succeeded")}>
                                Tandai Sukses
                              </button>
                              <button type="button" className="btn btn-danger btn-sm" onClick={() => onResolveUncertain(j.id, "failed")}>
                                Tandai Gagal
                              </button>
                            </>
                          )}
                        </div>
                      </td>
                    </tr>
                  );
                })
              )}
            </tbody>
          </table>
        </div>
      </section>

      <section className="card">
        <div className="section-title-row">
          <div>
            <h2>Riwayat Eksekusi Migration (Append-Only)</h2>
            <p className="muted" style={{ margin: 0 }}>
              Mencatat seluruh attempt eksekusi (berhasil maupun gagal) beserta salinan SQL yang dijalankan.
            </p>
          </div>
          <label className="checkbox-label">
            <input type="checkbox" checked={hideRollback} onChange={(e) => setHideRollback(e.target.checked)} />
            Hide rollback
          </label>
        </div>

        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>Exec ID</th>
                <th>Migration</th>
                <th>Operasi</th>
                <th>Status</th>
                <th>Pengguna</th>
                <th>Waktu</th>
                <th>SQL</th>
              </tr>
            </thead>
            <tbody>
              {history.length === 0 ? (
                <tr><td colSpan={7} className="muted">Belum ada riwayat eksekusi.</td></tr>
              ) : (
                history.map((h) => (
                  <React.Fragment key={h.executionId}>
                    <tr>
                      <td><strong>E{h.executionId}</strong></td>
                      <td>
                        v{h.migrationVersion} {h.migrationName} <span className="muted">(r{h.revision})</span>
                      </td>
                      <td>
                        <span className="badge badge-info">{h.operation}</span>
                        {h.targetExecutionId && (
                          <span className="muted" style={{ marginLeft: "0.35rem" }}>&rarr; E{h.targetExecutionId}</span>
                        )}
                      </td>
                      <td>
                        <span className={statusBadgeClass(h.status)}>{h.status}</span>
                        {h.committedBatches > 0 && (
                          <div className="muted" style={{ fontSize: "0.75rem" }}>
                            {h.committedBatches} batch / {h.committedRows} row
                          </div>
                        )}
                        {h.errorSummary && <div style={{ color: "#991b1b", fontSize: "0.76rem" }}>{h.errorSummary}</div>}
                      </td>
                      <td>{h.executedByUsername || `#${h.executedBy}`}</td>
                      <td>{formatDateTime(h.startedAt)}</td>
                      <td>
                        <button
                          type="button"
                          className="btn btn-outline btn-sm"
                          onClick={() => setExpandedExecId((prev) => (prev === h.executionId ? null : h.executionId))}
                        >
                          {expandedExecId === h.executionId ? "Tutup SQL" : "Lihat SQL"}
                        </button>
                      </td>
                    </tr>
                    {expandedExecId === h.executionId && (
                      <tr>
                        <td colSpan={7}>
                          <div className="muted">Salinan SQL yang dijalankan pada E{h.executionId}:</div>
                          <pre className="code-block">{h.sqlExecuted}</pre>
                        </td>
                      </tr>
                    )}
                  </React.Fragment>
                ))
              )}
            </tbody>
          </table>
        </div>
      </section>
    </>
  );
}
