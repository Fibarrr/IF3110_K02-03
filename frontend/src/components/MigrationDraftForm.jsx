import React from "react";

export const emptyDraft = {
  id: null,
  name: "",
  upSQL: "",
  downSQL: "",
  reversible: true,
  irreversibleReason: "",
  executionMode: "standard",
  tableSchema: "public",
  tableName: "",
  keyColumn: "id",
  batchSize: 1000,
};

export default function MigrationDraftForm({ draft, setDraft, canEdit, saving, onSubmit }) {
  return (
    <section className="card">
      <div className="section-title-row">
        <div>
          <h2>{draft.id ? `Edit Draft Migration #${draft.id}` : "Buat Draft Migration Baru"}</h2>
          <p className="muted" style={{ margin: 0 }}>
            {canEdit
              ? "SQL yang sudah pernah dieksekusi bersifat immutable; buat migration baru untuk perbaikan."
              : "Anda memiliki akses View Only. Anda tetap dapat menyetujui review jika Anda adalah leader grup."}
          </p>
        </div>
        {draft.id && (
          <button type="button" className="btn btn-outline btn-sm" onClick={() => setDraft(emptyDraft)}>
            Draft Baru
          </button>
        )}
      </div>

      <form className="form-stack" onSubmit={onSubmit}>
        <div className="field">
          <label htmlFor="mig-name">Nama Migration</label>
          <input
            id="mig-name"
            type="text"
            maxLength={120}
            required
            disabled={!canEdit}
            value={draft.name}
            onChange={(e) => setDraft({ ...draft, name: e.target.value })}
            placeholder="mis. Tambah kolom status pesanan"
          />
        </div>

        <div className="form-grid">
          <div className="field">
            <label htmlFor="mig-mode">Mode Eksekusi</label>
            <select
              id="mig-mode"
              disabled={!canEdit}
              value={draft.executionMode}
              onChange={(e) => setDraft({ ...draft, executionMode: e.target.value })}
            >
              <option value="standard">Standard (1 Transaksi, Async Queue)</option>
              <option value="batch">Per Batch (Cursor + Checkpoint per Transaksi)</option>
            </select>
          </div>
          <div className="field" style={{ justifyContent: "flex-end" }}>
            <label className="checkbox-label">
              <input
                type="checkbox"
                disabled={!canEdit}
                checked={draft.reversible}
                onChange={(e) => setDraft({ ...draft, reversible: e.target.checked })}
              />
              Reversible (memiliki SQL Down)
            </label>
          </div>
        </div>

        {draft.executionMode === "batch" && (
          <div className="subcard" style={{ marginTop: 0 }}>
            <h4>Konfigurasi Batch (Keyset Cursor)</h4>
            <div className="form-grid">
              <div className="field">
                <label htmlFor="batch-schema">Schema Tabel</label>
                <input
                  id="batch-schema"
                  type="text"
                  required
                  disabled={!canEdit}
                  value={draft.tableSchema}
                  onChange={(e) => setDraft({ ...draft, tableSchema: e.target.value })}
                />
              </div>
              <div className="field">
                <label htmlFor="batch-table">Nama Tabel Target</label>
                <input
                  id="batch-table"
                  type="text"
                  required
                  disabled={!canEdit}
                  value={draft.tableName}
                  onChange={(e) => setDraft({ ...draft, tableName: e.target.value })}
                  placeholder="mis. orders"
                />
              </div>
              <div className="field">
                <label htmlFor="batch-key">Kolom Key Unik (Monotonik)</label>
                <input
                  id="batch-key"
                  type="text"
                  required
                  disabled={!canEdit}
                  value={draft.keyColumn}
                  onChange={(e) => setDraft({ ...draft, keyColumn: e.target.value })}
                  placeholder="id"
                />
              </div>
              <div className="field">
                <label htmlFor="batch-size">Batch Size</label>
                <input
                  id="batch-size"
                  type="number"
                  min={1}
                  max={100000}
                  required
                  disabled={!canEdit}
                  value={draft.batchSize}
                  onChange={(e) => setDraft({ ...draft, batchSize: e.target.value })}
                />
              </div>
            </div>
            <p className="muted" style={{ margin: "0.45rem 0 0" }}>
              Gunakan parameter <code>$1</code> (last_id eksklusif) dan <code>$2</code> (end_id inklusif), misal:{" "}
              <code>UPDATE public.orders SET status = &apos;v2&apos; WHERE id &gt; $1 AND id &lt;= $2</code>
            </p>
          </div>
        )}

        <div className="field">
          <label htmlFor="mig-up-sql">SQL Up</label>
          <textarea
            id="mig-up-sql"
            className="sql-editor"
            rows={6}
            required
            disabled={!canEdit}
            spellCheck={false}
            value={draft.upSQL}
            onChange={(e) => setDraft({ ...draft, upSQL: e.target.value })}
            placeholder="ALTER TABLE public.items ADD COLUMN status TEXT;"
          />
        </div>

        {draft.reversible ? (
          <div className="field">
            <label htmlFor="mig-down-sql">SQL Down (Rollback / Kompensasi)</label>
            <textarea
              id="mig-down-sql"
              className="sql-editor"
              rows={5}
              required
              disabled={!canEdit}
              spellCheck={false}
              value={draft.downSQL}
              onChange={(e) => setDraft({ ...draft, downSQL: e.target.value })}
              placeholder="ALTER TABLE public.items DROP COLUMN status;"
            />
          </div>
        ) : (
          <div className="field">
            <label htmlFor="mig-irrev">Alasan Irreversible (Wajib)</label>
            <input
              id="mig-irrev"
              type="text"
              required
              disabled={!canEdit}
              value={draft.irreversibleReason}
              onChange={(e) => setDraft({ ...draft, irreversibleReason: e.target.value })}
              placeholder="mis. Menghapus kolom data lama secara permanen"
            />
          </div>
        )}

        <div className="row-actions">
          <button type="submit" className="btn btn-primary" disabled={!canEdit || saving}>
            {saving ? "Menyimpan..." : draft.id ? "Perbarui Draft (Revisi Baru)" : "Simpan Draft"}
          </button>
        </div>
      </form>
    </section>
  );
}
