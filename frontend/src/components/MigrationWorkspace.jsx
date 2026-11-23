import React, { useCallback, useEffect, useRef, useState } from "react";
import { apiFetch, formatAccess, formatDateTime, statusBadgeClass } from "../api.js";
import MigrationDraftForm, { emptyDraft } from "./MigrationDraftForm.jsx";
import MigrationJobsHistory from "./MigrationJobsHistory.jsx";

export default function MigrationWorkspace({ databaseId, access, user, notify, onDatabaseChanged }) {
  const canEdit = access === "view_edit";

  const [approvers, setApprovers] = useState([]);
  const [migrations, setMigrations] = useState([]);
  const [history, setHistory] = useState([]);
  const [reviewRequests, setReviewRequests] = useState([]);
  const [jobs, setJobs] = useState([]);
  const [hideRollback, setHideRollback] = useState(false);
  const [loading, setLoading] = useState(true);

  const [draft, setDraft] = useState(emptyDraft);
  const [saving, setSaving] = useState(false);
  const [commentByRequest, setCommentByRequest] = useState({});

  const loadAll = useCallback(
    async ({ silent = false } = {}) => {
      if (!silent) setLoading(true);
      try {
        const [appRes, migRes, revRes, jobRes] = await Promise.all([
          apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/approvers`),
          apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/migrations?hideRollback=${hideRollback}`),
          apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/review-requests`),
          apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/jobs`),
        ]);
        setApprovers(appRes.approvers || []);
        setMigrations(migRes.migrations || []);
        setHistory(migRes.history || []);
        setReviewRequests(revRes.reviewRequests || []);
        setJobs(jobRes.jobs || []);
      } catch (err) {
        if (!silent) notify(err.message || "Gagal memuat data migration", "error");
      } finally {
        if (!silent) setLoading(false);
      }
    },
    [databaseId, hideRollback, notify]
  );

  useEffect(() => {
    loadAll();
  }, [loadAll]);

  const hasActiveJob = jobs.some((j) => j.status === "queued" || j.status === "running");
  const prevActiveRef = useRef(false);

  useEffect(() => {
    if (prevActiveRef.current && !hasActiveJob && onDatabaseChanged) {
      onDatabaseChanged();
    }
    prevActiveRef.current = hasActiveJob;
  }, [hasActiveJob, onDatabaseChanged]);

  useEffect(() => {
    if (!hasActiveJob) return undefined;
    const timer = window.setInterval(() => loadAll({ silent: true }), 2000);
    return () => window.clearInterval(timer);
  }, [hasActiveJob, loadAll]);

  const activeApplied = migrations.filter((m) => m.status === "applied");
  const latestActiveMigration = activeApplied.length > 0 ? activeApplied[activeApplied.length - 1] : null;
  const missingLeaderGroups = approvers.filter((a) => !a.leaderUserId);

  const handleSelectForEdit = (m) => {
    setDraft({
      id: m.id,
      name: m.name,
      upSQL: m.upSQL || m.sql || "",
      downSQL: m.downSQL || "",
      reversible: Boolean(m.reversible),
      irreversibleReason: m.irreversibleReason || "",
      executionMode: m.executionMode || "standard",
      tableSchema: m.batchConfig?.tableSchema || "public",
      tableName: m.batchConfig?.tableName || "",
      keyColumn: m.batchConfig?.keyColumn || "id",
      batchSize: m.batchConfig?.batchSize || 1000,
    });
  };

  const handleSaveDraft = async (e) => {
    e.preventDefault();
    setSaving(true);
    const body = {
      name: draft.name.trim(),
      upSQL: draft.upSQL,
      downSQL: draft.reversible ? draft.downSQL : "",
      reversible: draft.reversible,
      irreversibleReason: draft.reversible ? "" : draft.irreversibleReason.trim(),
      executionMode: draft.executionMode,
      batchConfig:
        draft.executionMode === "batch"
          ? {
              tableSchema: draft.tableSchema.trim() || "public",
              tableName: draft.tableName.trim(),
              keyColumn: draft.keyColumn.trim() || "id",
              batchSize: Number(draft.batchSize) || 1000,
            }
          : null,
    };
    try {
      if (draft.id) {
        await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/migrations/${draft.id}`, { method: "PUT", body });
        notify("Draft migration berhasil diperbarui ke revisi baru.", "success");
      } else {
        await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/migrations`, { method: "POST", body });
        notify("Draft migration baru berhasil disimpan.", "success");
      }
      setDraft(emptyDraft);
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    } finally {
      setSaving(false);
    }
  };

  const handleSubmitReview = async (migration, operation, targetJobId = null) => {
    const comment = window.prompt(`Catatan pengajuan "${operation}" untuk v${migration.version} (${migration.name}):`, "");
    if (comment === null) return;
    const payload = { operation, revision: migration.currentRevision, comment };
    if (operation === "rollback" && migration.activeExecutionId) payload.targetExecutionId = migration.activeExecutionId;
    if (targetJobId) payload.targetJobId = targetJobId;
    try {
      await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/migrations/${migration.id}/review-requests`, {
        method: "POST",
        body: payload,
      });
      notify(`Review request (${operation}) berhasil diajukan ke semua leader grup.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleDecision = async (reqId, groupId, decision) => {
    const comment = (commentByRequest[reqId] || "").trim();
    try {
      await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/review-requests/${reqId}/approvals/${groupId}`, {
        method: "PUT",
        body: { decision, comment },
      });
      notify(`Keputusan "${decision}" tercatat pada audit review.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleEnqueue = async (reqId) => {
    try {
      const job = await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/review-requests/${reqId}/queue`, {
        method: "POST",
      });
      notify(`Job #${job.id} (${job.operation}) masuk antrean asynchronous worker.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleResolveUncertain = async (jobId, status) => {
    const note = window.prompt(`Catatan verifikasi target database untuk resolusi job #${jobId} sebagai "${status}":`, "");
    if (note === null) return;
    try {
      await apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/jobs/${jobId}/resolve-uncertain`, {
        method: "POST",
        body: { status, note },
      });
      notify(`Job #${jobId} berhasil diresolusi sebagai ${status}.`, "success");
      await loadAll();
      if (onDatabaseChanged) onDatabaseChanged();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  return (
    <div className="stack-lg">
      <section className="card">
        <div className="section-title-row" style={{ marginBottom: "0.45rem" }}>
          <div>
            <h2>Approver Wajib Database Ini</h2>
            <p className="muted" style={{ margin: 0 }}>
              Setiap operasi perubahan (apply, rollback, forward, resume, cancel batch) wajib disetujui leader dari{" "}
              <strong>semua grup</strong> yang memiliki akses (<code>view_only</code> maupun <code>view_edit</code>).
            </p>
          </div>
          <button type="button" className="btn btn-outline btn-sm" onClick={() => loadAll()} disabled={loading}>
            Segarkan Semua
          </button>
        </div>
        {missingLeaderGroups.length > 0 && (
          <div className="alert alert-warn" style={{ marginBottom: "0.65rem" }}>
            <span>
              Perhatian: Grup <strong>{missingLeaderGroups.map((g) => g.groupName).join(", ")}</strong> belum memiliki
              leader aktif. Platform Admin harus menetapkan leader sebelum review dapat diajukan.
            </span>
          </div>
        )}
        <div className="row-actions">
          {approvers.length === 0 ? (
            <span className="muted">Belum ada grup dengan grant pada database ini.</span>
          ) : (
            approvers.map((a) => (
              <span key={a.groupId} className={`badge ${a.leaderUserId ? "badge-info" : "badge-danger"}`}>
                {a.groupName} ({formatAccess(a.access)}) — Leader: {a.leaderUsername || "BELUM ADA"}
              </span>
            ))
          )}
        </div>
      </section>

      <div className="migration-grid">
        <MigrationDraftForm draft={draft} setDraft={setDraft} canEdit={canEdit} saving={saving} onSubmit={handleSaveDraft} />

        <section className="card">
          <h2>Daftar Definisi Migration</h2>
          <div className="table-scroll">
            <table className="data-table">
              <thead>
                <tr>
                  <th>Ver</th>
                  <th>Migration</th>
                  <th>Mode &amp; Rev</th>
                  <th>Status</th>
                  <th>Aksi</th>
                </tr>
              </thead>
              <tbody>
                {migrations.length === 0 ? (
                  <tr><td colSpan={5} className="muted">Belum ada migration.</td></tr>
                ) : (
                  migrations.map((m) => {
                    const isLatestActive = latestActiveMigration?.id === m.id;
                    return (
                      <tr key={m.id}>
                        <td><strong>v{m.version}</strong></td>
                        <td>
                          <div><strong>{m.name}</strong></div>
                          <div className="muted" style={{ fontSize: "0.78rem" }}>
                            {m.reversible ? "Reversible" : `Irreversible: ${m.irreversibleReason}`}
                          </div>
                        </td>
                        <td>
                          <span className="badge badge-neutral">{m.executionMode}</span>{" "}
                          <span className="muted">r{m.currentRevision}</span>
                        </td>
                        <td><span className={statusBadgeClass(m.status)}>{m.status}</span></td>
                        <td>
                          <div className="row-actions">
                            {canEdit && !m.hasExecuted && (
                              <button type="button" className="btn btn-outline btn-sm" onClick={() => handleSelectForEdit(m)}>
                                Edit
                              </button>
                            )}
                            {canEdit && (m.status === "draft" || m.status === "pending_review" || m.status === "approved") && (
                              <button type="button" className="btn btn-primary btn-sm" onClick={() => handleSubmitReview(m, "apply")}>
                                Ajukan Apply
                              </button>
                            )}
                            {canEdit && isLatestActive && m.reversible && (
                              <button type="button" className="btn btn-danger btn-sm" onClick={() => handleSubmitReview(m, "rollback")}>
                                Ajukan Rollback
                              </button>
                            )}
                            {canEdit && (m.status === "rolled_back" || m.status === "failed") && (
                              <button type="button" className="btn btn-primary btn-sm" onClick={() => handleSubmitReview(m, "forward")}>
                                Ajukan Forward
                              </button>
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
      </div>

      <section className="card">
        <h2>Review &amp; Approval Leader Grup</h2>
        {reviewRequests.length === 0 ? (
          <p className="muted">Belum ada pengajuan review pada database ini.</p>
        ) : (
          <div className="stack-lg">
            {reviewRequests.map((req) => {
              const myPendingApprovals = (req.approvals || []).filter(
                (a) => a.leaderUserId === user?.id && (req.status === "pending_review" || req.status === "rejected" || req.status === "approved")
              );
              return (
                <article key={req.id} className="subcard" style={{ marginTop: 0 }}>
                  <div className="section-title-row" style={{ marginBottom: "0.45rem" }}>
                    <div>
                      <strong>Review #{req.id} — v{req.migrationVersion} {req.migrationName}</strong>{" "}
                      <span className="badge badge-info">{req.operation}</span>{" "}
                      <span className="muted">revisi {req.revision}</span>
                      {req.targetExecutionId && <span className="muted"> • target exec #{req.targetExecutionId}</span>}
                      {req.targetJobId && <span className="muted"> • target job #{req.targetJobId}</span>}
                    </div>
                    <div className="row-actions">
                      <span className={statusBadgeClass(req.status)}>{req.status}</span>
                      {canEdit && req.status === "approved" && (
                        <button type="button" className="btn btn-primary btn-sm" onClick={() => handleEnqueue(req.id)}>
                          Kirim ke Queue (Async)
                        </button>
                      )}
                    </div>
                  </div>
                  <p className="muted" style={{ margin: "0 0 0.45rem" }}>
                    Diajukan oleh <strong>{req.requestedByUsername || `#${req.requestedBy}`}</strong> pada{" "}
                    {formatDateTime(req.createdAt)}{req.comment ? ` — "${req.comment}"` : ""}
                  </p>
                  {req.invalidationReason && (
                    <div className="alert alert-error" style={{ marginBottom: "0.5rem" }}>
                      <span>Invalidasi: {req.invalidationReason}</span>
                    </div>
                  )}
                  {myPendingApprovals.length > 0 && (
                    <div className="inline-form" style={{ marginBottom: "0.55rem" }}>
                      <input
                        type="text"
                        value={commentByRequest[req.id] || ""}
                        onChange={(e) => setCommentByRequest((prev) => ({ ...prev, [req.id]: e.target.value }))}
                        placeholder="Catatan keputusan leader (opsional)..."
                      />
                    </div>
                  )}
                  <div className="approval-pill-list">
                    {(req.approvals || []).map((a) => {
                      const canDecide =
                        a.leaderUserId === user?.id &&
                        (req.status === "pending_review" || req.status === "approved" || req.status === "rejected");
                      return (
                        <div key={a.groupId} className="approval-row">
                          <div>
                            <strong>{a.groupName}</strong>{" "}
                            <span className="muted">({formatAccess(a.access)})</span> — Leader:{" "}
                            <strong>{a.leaderUsername}</strong>
                            {a.comment && <span className="muted"> • &ldquo;{a.comment}&rdquo;</span>}
                          </div>
                          <div className="row-actions">
                            <span className={statusBadgeClass(a.decision)}>{a.decision}</span>
                            {canDecide && (
                              <>
                                <button type="button" className="btn btn-success btn-sm" onClick={() => handleDecision(req.id, a.groupId, "approve")}>
                                  Approve
                                </button>
                                <button type="button" className="btn btn-danger btn-sm" onClick={() => handleDecision(req.id, a.groupId, "reject")}>
                                  Reject
                                </button>
                              </>
                            )}
                          </div>
                        </div>
                      );
                    })}
                  </div>
                </article>
              );
            })}
          </div>
        )}
      </section>

      <MigrationJobsHistory
        jobs={jobs}
        migrations={migrations}
        history={history}
        hasActiveJob={hasActiveJob}
        canEdit={canEdit}
        hideRollback={hideRollback}
        setHideRollback={setHideRollback}
        onSubmitReview={handleSubmitReview}
        onResolveUncertain={handleResolveUncertain}
      />
    </div>
  );
}
