import React, { useCallback, useEffect, useState } from "react";
import { apiFetch, formatAccess } from "../api.js";

export default function WorkspacePage({ user, notify, navigate }) {
  const [databases, setDatabases] = useState([]);
  const [loading, setLoading] = useState(true);

  const loadDatabases = useCallback(async () => {
    setLoading(true);
    try {
      const data = await apiFetch("/api/databases");
      setDatabases(data.databases || []);
    } catch (err) {
      notify(err.message || "Gagal memuat daftar database", "error");
    } finally {
      setLoading(false);
    }
  }, [notify]);

  useEffect(() => {
    loadDatabases();
  }, [loadDatabases]);

  return (
    <div>
      <section className="page-header">
        <div>
          <h1>Daftar Database</h1>
          <p className="muted">
            Pilih database PostgreSQL yang dapat Anda akses untuk melihat schema, data, dan alur migration.
          </p>
        </div>
        <div className="header-actions">
          {user?.platformAdmin && (
            <button type="button" className="btn btn-primary" onClick={() => navigate("/admin")}>
              Buka Panel Admin
            </button>
          )}
          <button type="button" className="btn btn-outline" onClick={loadDatabases} disabled={loading}>
            Muat Ulang
          </button>
        </div>
      </section>

      {loading ? (
        <div className="db-grid">
          <div className="skeleton-box" />
          <div className="skeleton-box" />
          <div className="skeleton-box" />
        </div>
      ) : databases.length === 0 ? (
        <section className="card">
          <h2>Belum ada database yang dapat diakses</h2>
          <p className="muted">
            Hak akses database diberikan melalui grup. Hubungi Platform Admin agar grup Anda diberi izin{" "}
            <code>view_only</code> atau <code>view_edit</code>.
          </p>
        </section>
      ) : (
        <div className="db-grid">
          {databases.map((db) => (
            <article key={db.id} className="card db-card">
              <div>
                <div className="section-title-row" style={{ marginBottom: "0.4rem" }}>
                  <h2 style={{ margin: 0 }}>{db.name}</h2>
                  <span className={`badge ${db.access === "view_edit" ? "badge-success" : "badge-info"}`}>
                    {formatAccess(db.access)}
                  </span>
                </div>
                <p className="muted" style={{ margin: "0 0 0.35rem" }}>
                  ID: <code>{db.id}</code>
                </p>
                <p className="muted" style={{ margin: "0 0 0.35rem" }}>
                  {db.host}:{db.port}/{db.databaseName} (SSL: {db.sslMode})
                </p>
                {db.description && <p style={{ margin: "0.4rem 0 0", fontSize: "0.88rem" }}>{db.description}</p>}
              </div>
              <div className="row-actions">
                <button
                  type="button"
                  className="btn btn-primary btn-sm"
                  onClick={() => navigate(`/databases/${encodeURIComponent(db.id)}`)}
                >
                  Buka Workspace Database
                </button>
              </div>
            </article>
          ))}
        </div>
      )}
    </div>
  );
}
