import React, { useCallback, useEffect, useState } from "react";
import { apiFetch, formatAccess } from "../api.js";
import SchemaExplorer from "../components/SchemaExplorer.jsx";
import SchemaVisualizer from "../components/SchemaVisualizer.jsx";
import MigrationWorkspace from "../components/MigrationWorkspace.jsx";

export default function DatabasePage({ databaseId, user, notify, navigate }) {
  const [activeTab, setActiveTab] = useState("explorer");
  const [summary, setSummary] = useState(null);
  const [schemas, setSchemas] = useState([]);
  const [access, setAccess] = useState("view_only");
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState("");

  const loadDatabase = useCallback(async () => {
    setLoading(true);
    setError("");
    const [sumRes, schemaRes] = await Promise.allSettled([
      apiFetch(`/api/databases/${encodeURIComponent(databaseId)}`),
      apiFetch(`/api/databases/${encodeURIComponent(databaseId)}/schemas`),
    ]);

    if (sumRes.status === "fulfilled") {
      setSummary(sumRes.value);
      setAccess(sumRes.value.database?.access || "view_only");
    } else {
      const msg = sumRes.reason?.message || "Gagal memuat metadata database";
      setError(msg);
      notify(msg, "error");
    }

    if (schemaRes.status === "fulfilled") {
      setSchemas(schemaRes.value.schemas || []);
      if (schemaRes.value.access) setAccess(schemaRes.value.access);
    } else {
      setSchemas([]);
    }
    setLoading(false);
  }, [databaseId, notify]);

  useEffect(() => {
    loadDatabase();
  }, [loadDatabase]);

  const dbInfo = summary?.database;
  const connStatus = summary?.status;

  return (
    <div>
      <section className="page-header">
        <div>
          <div className="breadcrumb">
            <button
              type="button"
              className="btn btn-outline btn-sm"
              style={{ padding: "0.18rem 0.5rem", marginRight: "0.4rem" }}
              onClick={() => navigate("/")}
            >
              &larr; Daftar Database
            </button>
            <span>{databaseId}</span>
          </div>
          <h1>{dbInfo ? dbInfo.name : `Database: ${databaseId}`}</h1>
          <p className="muted" style={{ margin: 0 }}>
            {dbInfo
              ? `${dbInfo.host}:${dbInfo.port}/${dbInfo.databaseName} — SSL: ${dbInfo.sslMode}`
              : "Memuat metadata koneksi..."}
          </p>
        </div>

        <div className="header-actions">
          <span className={`badge ${access === "view_edit" ? "badge-success" : "badge-info"}`}>
            {formatAccess(access)}
          </span>
          {connStatus && (
            <span className={`badge ${connStatus.connected ? "badge-success" : "badge-danger"}`}>
              {connStatus.connected ? "Terhubung" : "Gagal Terhubung"}
            </span>
          )}
          <button type="button" className="btn btn-outline btn-sm" onClick={loadDatabase} disabled={loading}>
            Muat Ulang Schema
          </button>
        </div>
      </section>

      {error && !loading && !dbInfo ? (
        <section className="card">
          <h2>Tidak dapat membuka database</h2>
          <p className="muted">{error}</p>
        </section>
      ) : (
        <>
          <div className="workspace-tabs" role="tablist" aria-label="Fitur database">
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === "explorer"}
              className={`tab-btn ${activeTab === "explorer" ? "active" : ""}`}
              onClick={() => setActiveTab("explorer")}
            >
              Schema &amp; Table Viewer
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === "visualizer"}
              className={`tab-btn ${activeTab === "visualizer" ? "active" : ""}`}
              onClick={() => setActiveTab("visualizer")}
            >
              Schema Visualizer
            </button>
            <button
              type="button"
              role="tab"
              aria-selected={activeTab === "migrations"}
              className={`tab-btn ${activeTab === "migrations" ? "active" : ""}`}
              onClick={() => setActiveTab("migrations")}
            >
              Migration &amp; Approval
            </button>
          </div>

          {activeTab === "explorer" && (
            <SchemaExplorer databaseId={databaseId} schemas={schemas} loading={loading} notify={notify} />
          )}
          {activeTab === "visualizer" && (
            <SchemaVisualizer databaseId={databaseId} schemas={schemas} notify={notify} />
          )}
          {activeTab === "migrations" && (
            <MigrationWorkspace
              databaseId={databaseId}
              access={access}
              user={user}
              notify={notify}
              onDatabaseChanged={loadDatabase}
            />
          )}
        </>
      )}
    </div>
  );
}
