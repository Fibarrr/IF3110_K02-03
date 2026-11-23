import React, { useCallback, useEffect, useState } from "react";
import { apiFetch } from "../api.js";

export default function SchemaVisualizer({ databaseId, schemas, notify }) {
  const [nodes, setNodes] = useState([]);
  const [edges, setEdges] = useState([]);
  const [filter, setFilter] = useState("");
  const [loading, setLoading] = useState(false);

  const loadGraph = useCallback(async () => {
    const flatTables = [];
    for (const s of schemas || []) {
      for (const t of s.tables || []) {
        flatTables.push({ schema: s.name, table: t.name });
      }
    }
    if (flatTables.length === 0) {
      setNodes([]);
      setEdges([]);
      return;
    }

    setLoading(true);
    try {
      const targetTables = flatTables.slice(0, 40);
      const detailsList = [];
      for (let i = 0; i < targetTables.length; i += 4) {
        const chunk = targetTables.slice(i, i + 4);
        const res = await Promise.all(
          chunk.map((item) =>
            apiFetch(
              `/api/databases/${encodeURIComponent(databaseId)}/schemas/${encodeURIComponent(item.schema)}/tables/${encodeURIComponent(item.table)}`
            )
          )
        );
        detailsList.push(...res);
      }

      const colsPerRow = 3;
      const cellW = 290;
      const cellH = 230;
      const builtNodes = detailsList.map((det, idx) => {
        const key = `${det.schema}.${det.name}`;
        return {
          key,
          schema: det.schema,
          name: det.name,
          columns: det.columns || [],
          constraints: det.constraints || [],
          x: 24 + (idx % colsPerRow) * cellW,
          y: 24 + Math.floor(idx / colsPerRow) * cellH,
        };
      });

      const nodeMap = new Map(builtNodes.map((n) => [n.key, n]));
      const builtEdges = [];
      for (const n of builtNodes) {
        for (const c of n.constraints) {
          if (c.type === "FOREIGN KEY" && c.referencedTable) {
            const targetKey = `${c.referencedSchema || n.schema}.${c.referencedTable}`;
            if (nodeMap.has(targetKey)) {
              builtEdges.push({
                id: `${n.key}-${c.name}-${targetKey}`,
                from: n.key,
                to: targetKey,
                label: `${(c.columns || []).join(",")} -> ${(c.referencedColumns || []).join(",")}`,
              });
            }
          }
        }
      }

      setNodes(builtNodes);
      setEdges(builtEdges);
    } catch (err) {
      notify(err.message || "Gagal memuat diagram schema", "error");
    } finally {
      setLoading(false);
    }
  }, [databaseId, schemas, notify]);

  useEffect(() => {
    loadGraph();
  }, [loadGraph]);

  const query = filter.trim().toLowerCase();
  const visibleNodes = nodes.filter((n) => !query || n.key.toLowerCase().includes(query));
  const visibleKeys = new Set(visibleNodes.map((n) => n.key));
  const nodeByKey = new Map(visibleNodes.map((n) => [n.key, n]));
  const visibleEdges = edges.filter((e) => visibleKeys.has(e.from) && visibleKeys.has(e.to));

  const maxX = visibleNodes.reduce((acc, n) => Math.max(acc, n.x + 280), 820);
  const maxY = visibleNodes.reduce((acc, n) => Math.max(acc, n.y + 220), 380);

  return (
    <section className="card">
      <div className="section-title-row">
        <div>
          <h2>Diagram Relasi Tabel (Schema Visualizer)</h2>
          <p className="muted" style={{ margin: 0 }}>
            Menampilkan kolom Primary Key dan garis relasi Foreign Key antar tabel.
          </p>
        </div>
        <div className="row-actions">
          <input
            type="search"
            style={{ width: "200px" }}
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            placeholder="Cari tabel..."
          />
          <button type="button" className="btn btn-outline btn-sm" onClick={loadGraph} disabled={loading}>
            Muat Ulang
          </button>
        </div>
      </div>

      {loading ? (
        <div className="skeleton-box" />
      ) : visibleNodes.length === 0 ? (
        <p className="muted">Tidak ada tabel untuk ditampilkan di diagram.</p>
      ) : (
        <div className="visualizer-viewport">
          <div className="visualizer-canvas" style={{ width: maxX, height: maxY }}>
            <svg width={maxX} height={maxY} style={{ position: "absolute", inset: 0, pointerEvents: "none" }}>
              {visibleEdges.map((edge) => {
                const from = nodeByKey.get(edge.from);
                const to = nodeByKey.get(edge.to);
                if (!from || !to) return null;
                const x1 = from.x + 120;
                const y1 = from.y + 28;
                const x2 = to.x + 120;
                const y2 = to.y + 28;
                const mx = (x1 + x2) / 2;
                return (
                  <path
                    key={edge.id}
                    d={`M ${x1} ${y1} C ${mx} ${y1}, ${mx} ${y2}, ${x2} ${y2}`}
                    fill="none"
                    stroke="#2563eb"
                    strokeWidth="2"
                    strokeDasharray="4 2"
                  >
                    <title>{edge.label}</title>
                  </path>
                );
              })}
            </svg>

            {visibleNodes.map((node) => (
              <div key={node.key} className="visualizer-node" style={{ left: node.x, top: node.y }}>
                <div className="visualizer-node-head" title={node.key}>
                  {node.key}
                </div>
                <div>
                  {node.columns.slice(0, 6).map((col) => (
                    <div key={col.name} className="visualizer-node-col">
                      <span>
                        {col.isPrimaryKey ? "🔑 " : ""}
                        {col.name}
                      </span>
                      <span className="muted" style={{ fontSize: "0.72rem" }}>{col.dataType}</span>
                    </div>
                  ))}
                  {node.columns.length > 6 && (
                    <div className="visualizer-node-col muted">+{node.columns.length - 6} kolom lainnya</div>
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </section>
  );
}
