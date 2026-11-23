import React, { useCallback, useEffect, useState } from "react";
import { apiFetch, formatAccess } from "../api.js";

const emptyDBForm = {
  editingId: "",
  id: "",
  name: "",
  host: "localhost",
  port: 5432,
  databaseName: "",
  username: "",
  password: "",
  sslMode: "disable",
  description: "",
};

export default function AdminPage({ user, notify, navigate }) {
  const [databases, setDatabases] = useState([]);
  const [groups, setGroups] = useState([]);
  const [users, setUsers] = useState([]);
  const [loading, setLoading] = useState(true);
  const [testResults, setTestResults] = useState({});

  const [dbForm, setDbForm] = useState(emptyDBForm);
  const [newGroupName, setNewGroupName] = useState("");
  const [newUser, setNewUser] = useState({ username: "", password: "" });

  const [selectedGroupId, setSelectedGroupId] = useState(null);
  const [groupMembers, setGroupMembers] = useState([]);
  const [groupGrants, setGroupGrants] = useState([]);
  const [memberUserId, setMemberUserId] = useState("");
  const [leaderUserId, setLeaderUserId] = useState("");
  const [grantDbId, setGrantDbId] = useState("");
  const [grantAccess, setGrantAccess] = useState("view_only");

  const loadAll = useCallback(async () => {
    setLoading(true);
    try {
      const [dbRes, grpRes, usrRes] = await Promise.all([
        apiFetch("/api/admin/databases"),
        apiFetch("/api/admin/groups"),
        apiFetch("/api/admin/users"),
      ]);
      setDatabases(dbRes.databases || []);
      setGroups(grpRes.groups || []);
      setUsers(usrRes.users || []);
    } catch (err) {
      notify(err.message || "Gagal memuat data admin", "error");
    } finally {
      setLoading(false);
    }
  }, [notify]);

  const loadGroupDetails = useCallback(
    async (groupId) => {
      if (!groupId) return;
      try {
        const [memRes, grantRes] = await Promise.all([
          apiFetch(`/api/admin/groups/${groupId}/members`),
          apiFetch(`/api/admin/groups/${groupId}/databases`),
        ]);
        setGroupMembers(memRes.members || []);
        setGroupGrants(grantRes.databases || []);
      } catch (err) {
        notify(err.message || "Gagal memuat detail grup", "error");
      }
    },
    [notify]
  );

  useEffect(() => {
    if (user?.platformAdmin) {
      loadAll();
    }
  }, [user, loadAll]);

  useEffect(() => {
    if (selectedGroupId) {
      loadGroupDetails(selectedGroupId);
    }
  }, [selectedGroupId, loadGroupDetails]);

  const handleSelectGroup = (g) => {
    setSelectedGroupId(g.id);
    setLeaderUserId(g.leaderUserId ? String(g.leaderUserId) : "");
  };

  if (!user?.platformAdmin) {
    return (
      <section className="card">
        <h1>Akses Ditolak</h1>
        <p className="muted">
          Halaman Panel Platform Admin hanya dapat diakses oleh pengguna dengan peran <strong>Platform Admin</strong>.
        </p>
        <button type="button" className="btn btn-primary" onClick={() => navigate("/")}>
          Kembali ke Daftar Database
        </button>
      </section>
    );
  }

  const selectedGroup = groups.find((g) => g.id === selectedGroupId) || null;
  const activeGroupMembers = groupMembers.filter((m) => !m.disabled);

  const handleSaveDB = async (e) => {
    e.preventDefault();
    const payload = {
      id: dbForm.id.trim(),
      name: dbForm.name.trim(),
      host: dbForm.host.trim(),
      port: Number(dbForm.port) || 5432,
      databaseName: dbForm.databaseName.trim(),
      username: dbForm.username.trim(),
      password: dbForm.password,
      sslMode: dbForm.sslMode,
      description: dbForm.description.trim(),
    };
    try {
      if (dbForm.editingId) {
        await apiFetch(`/api/admin/databases/${encodeURIComponent(dbForm.editingId)}`, {
          method: "PUT",
          body: payload,
        });
        notify(`Koneksi "${dbForm.editingId}" berhasil diperbarui.`, "success");
      } else {
        await apiFetch("/api/admin/databases", { method: "POST", body: payload });
        notify("Koneksi database baru berhasil ditambahkan.", "success");
      }
      setDbForm(emptyDBForm);
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleEditDB = (db) => {
    setDbForm({
      editingId: db.id,
      id: db.id,
      name: db.name,
      host: db.host,
      port: db.port,
      databaseName: db.databaseName,
      username: db.username,
      password: "",
      sslMode: db.sslMode,
      description: db.description || "",
    });
  };

  const handleDeleteDB = async (dbId) => {
    if (!window.confirm(`Hapus koneksi "${dbId}" beserta seluruh izin grup terkait?`)) return;
    try {
      await apiFetch(`/api/admin/databases/${encodeURIComponent(dbId)}`, { method: "DELETE" });
      if (dbForm.editingId === dbId) setDbForm(emptyDBForm);
      notify(`Koneksi "${dbId}" dihapus.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleTestDB = async (dbId) => {
    setTestResults((prev) => ({ ...prev, [dbId]: { testing: true } }));
    try {
      const res = await apiFetch(`/api/admin/databases/${encodeURIComponent(dbId)}/test`, { method: "POST" });
      setTestResults((prev) => ({ ...prev, [dbId]: { connected: true, info: res.currentDatabase } }));
    } catch (err) {
      setTestResults((prev) => ({ ...prev, [dbId]: { connected: false, error: err.message } }));
    }
  };

  const handleCreateGroup = async (e) => {
    e.preventDefault();
    try {
      const grp = await apiFetch("/api/admin/groups", {
        method: "POST",
        body: { name: newGroupName.trim() },
      });
      setNewGroupName("");
      notify(`Grup "${grp.name}" berhasil dibuat.`, "success");
      await loadAll();
      setSelectedGroupId(grp.id);
      setLeaderUserId("");
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleDeleteGroup = async (groupId, name) => {
    if (!window.confirm(`Hapus grup "${name}"?`)) return;
    try {
      await apiFetch(`/api/admin/groups/${groupId}`, { method: "DELETE" });
      if (selectedGroupId === groupId) setSelectedGroupId(null);
      notify(`Grup "${name}" dihapus.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleAddMember = async (e) => {
    e.preventDefault();
    if (!selectedGroupId || !memberUserId) return;
    try {
      await apiFetch(`/api/admin/groups/${selectedGroupId}/members/${memberUserId}`, { method: "PUT" });
      setMemberUserId("");
      notify("Anggota berhasil ditambahkan ke grup.", "success");
      await loadGroupDetails(selectedGroupId);
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleRemoveMember = async (uid) => {
    if (!selectedGroupId) return;
    try {
      await apiFetch(`/api/admin/groups/${selectedGroupId}/members/${uid}`, { method: "DELETE" });
      notify("Anggota dikeluarkan dari grup.", "success");
      await loadGroupDetails(selectedGroupId);
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleSetLeader = async (e) => {
    e.preventDefault();
    if (!selectedGroupId || !leaderUserId) return;
    try {
      await apiFetch(`/api/admin/groups/${selectedGroupId}/leader`, {
        method: "PUT",
        body: { userId: Number(leaderUserId) },
      });
      notify("Leader grup berhasil ditetapkan.", "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleGrantDB = async (e) => {
    e.preventDefault();
    if (!selectedGroupId || !grantDbId) return;
    try {
      await apiFetch(`/api/admin/groups/${selectedGroupId}/databases/${encodeURIComponent(grantDbId)}/access`, {
        method: "PUT",
        body: { access: grantAccess },
      });
      notify("Izin akses database diperbarui.", "success");
      await loadGroupDetails(selectedGroupId);
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleRevokeDB = async (dbId) => {
    if (!selectedGroupId) return;
    try {
      await apiFetch(`/api/admin/groups/${selectedGroupId}/databases/${encodeURIComponent(dbId)}/access`, {
        method: "DELETE",
      });
      notify("Izin akses database dicabut.", "success");
      await loadGroupDetails(selectedGroupId);
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleCreateUser = async (e) => {
    e.preventDefault();
    try {
      await apiFetch("/api/admin/users", {
        method: "POST",
        body: { username: newUser.username.trim(), password: newUser.password },
      });
      setNewUser({ username: "", password: "" });
      notify("Pengguna baru berhasil ditambahkan.", "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleToggleDisabled = async (u) => {
    try {
      await apiFetch(`/api/admin/users/${u.id}/disabled`, {
        method: "PATCH",
        body: { disabled: !u.disabled },
      });
      notify(`Status pengguna "${u.username}" diperbarui.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleToggleAdmin = async (u) => {
    try {
      await apiFetch(`/api/admin/users/${u.id}/platform-admin`, {
        method: "PATCH",
        body: { platformAdmin: !u.platformAdmin },
      });
      notify(`Peran Platform Admin "${u.username}" diperbarui.`, "success");
      await loadAll();
    } catch (err) {
      notify(err.message, "error");
    }
  };

  const handleResetPassword = async (u) => {
    const nextPassword = window.prompt(`Password baru untuk "${u.username}" (minimal 12 karakter):`);
    if (!nextPassword) return;
    try {
      await apiFetch(`/api/admin/users/${u.id}/password`, {
        method: "PATCH",
        body: { password: nextPassword },
      });
      notify(`Password "${u.username}" berhasil diganti.`, "success");
    } catch (err) {
      notify(err.message, "error");
    }
  };

  return (
    <div className="admin-grid">
      <section className="page-header" style={{ marginBottom: 0 }}>
        <div>
          <h1>Panel Platform Admin</h1>
          <p className="muted">
            Kelola koneksi PostgreSQL, pengguna, grup, penetapan leader grup untuk approval migration, dan izin database.
          </p>
        </div>
        <button type="button" className="btn btn-outline" onClick={loadAll} disabled={loading}>
          Muat Ulang Semua
        </button>
      </section>

      {/* 1. Database Connections */}
      <section className="card">
        <div className="section-title-row">
          <h2>1. Koneksi Database PostgreSQL</h2>
          {dbForm.editingId && (
            <button type="button" className="btn btn-outline btn-sm" onClick={() => setDbForm(emptyDBForm)}>
              Batal Edit (Mode Tambah Baru)
            </button>
          )}
        </div>

        <form className="form-grid" onSubmit={handleSaveDB} style={{ marginBottom: "1rem" }}>
          <div className="field">
            <label htmlFor="db-id">ID Database</label>
            <input
              id="db-id"
              type="text"
              disabled={Boolean(dbForm.editingId)}
              value={dbForm.id}
              onChange={(e) => setDbForm({ ...dbForm, id: e.target.value })}
              placeholder="opsional saat tambah baru (mis. orders-db)"
            />
          </div>
          <div className="field">
            <label htmlFor="db-name">Nama Tampilan</label>
            <input
              id="db-name"
              type="text"
              required
              value={dbForm.name}
              onChange={(e) => setDbForm({ ...dbForm, name: e.target.value })}
              placeholder="mis. Orders Production"
            />
          </div>
          <div className="field">
            <label htmlFor="db-host">Host</label>
            <input
              id="db-host"
              type="text"
              required
              value={dbForm.host}
              onChange={(e) => setDbForm({ ...dbForm, host: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="db-port">Port</label>
            <input
              id="db-port"
              type="number"
              min="1"
              max="65535"
              required
              value={dbForm.port}
              onChange={(e) => setDbForm({ ...dbForm, port: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="db-dbname">Nama Database PostgreSQL</label>
            <input
              id="db-dbname"
              type="text"
              required
              value={dbForm.databaseName}
              onChange={(e) => setDbForm({ ...dbForm, databaseName: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="db-user">Username Database</label>
            <input
              id="db-user"
              type="text"
              required
              value={dbForm.username}
              onChange={(e) => setDbForm({ ...dbForm, username: e.target.value })}
            />
          </div>
          <div className="field">
            <label htmlFor="db-pass">Password Database</label>
            <input
              id="db-pass"
              type="password"
              autoComplete="new-password"
              value={dbForm.password}
              onChange={(e) => setDbForm({ ...dbForm, password: e.target.value })}
              placeholder={dbForm.editingId ? "Kosongkan jika tidak diubah" : "Password PostgreSQL"}
            />
          </div>
          <div className="field">
            <label htmlFor="db-ssl">SSL Mode</label>
            <select id="db-ssl" value={dbForm.sslMode} onChange={(e) => setDbForm({ ...dbForm, sslMode: e.target.value })}>
              <option value="disable">disable</option>
              <option value="require">require</option>
              <option value="verify-ca">verify-ca</option>
              <option value="verify-full">verify-full</option>
            </select>
          </div>
          <div className="field field-full">
            <label htmlFor="db-desc">Deskripsi</label>
            <input
              id="db-desc"
              type="text"
              value={dbForm.description}
              onChange={(e) => setDbForm({ ...dbForm, description: e.target.value })}
              placeholder="Catatan singkat"
            />
          </div>
          <div className="field field-full">
            <button type="submit" className="btn btn-primary" style={{ alignSelf: "flex-start" }}>
              {dbForm.editingId ? `Perbarui "${dbForm.editingId}"` : "Simpan Koneksi Database"}
            </button>
          </div>
        </form>

        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>ID</th>
                <th>Nama</th>
                <th>Target</th>
                <th>User / SSL</th>
                <th>Status Tes</th>
                <th>Aksi</th>
              </tr>
            </thead>
            <tbody>
              {databases.length === 0 ? (
                <tr>
                  <td colSpan={6} className="muted">
                    Belum ada koneksi database terdaftar.
                  </td>
                </tr>
              ) : (
                databases.map((db) => {
                  const tr = testResults[db.id];
                  return (
                    <tr key={db.id}>
                      <td><code>{db.id}</code></td>
                      <td>{db.name}</td>
                      <td>{db.host}:{db.port}/{db.databaseName}</td>
                      <td>{db.username} ({db.sslMode})</td>
                      <td>
                        {!tr ? (
                          <span className="muted">-</span>
                        ) : tr.testing ? (
                          <span className="badge badge-warn">Menguji...</span>
                        ) : tr.connected ? (
                          <span className="badge badge-success">Terhubung ({tr.info})</span>
                        ) : (
                          <span className="badge badge-danger">{tr.error}</span>
                        )}
                      </td>
                      <td>
                        <div className="row-actions">
                          <button type="button" className="btn btn-outline btn-sm" onClick={() => handleTestDB(db.id)}>
                            Uji
                          </button>
                          <button type="button" className="btn btn-outline btn-sm" onClick={() => handleEditDB(db)}>
                            Edit
                          </button>
                          <button type="button" className="btn btn-danger btn-sm" onClick={() => handleDeleteDB(db.id)}>
                            Hapus
                          </button>
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

      {/* 2. Groups, Leaders, Members & Database Grants */}
      <section className="card">
        <h2>2. Grup, Leader Approval, Anggota, &amp; Izin Database</h2>
        <p className="muted" style={{ marginBottom: "0.75rem" }}>
          Setiap grup yang memiliki akses database (<code>view_only</code> maupun <code>view_edit</code>) wajib memiliki
          seorang <strong>Leader aktif</strong> untuk menyetujui setiap perubahan migration.
        </p>

        <form className="inline-form" onSubmit={handleCreateGroup}>
          <input
            type="text"
            required
            value={newGroupName}
            onChange={(e) => setNewGroupName(e.target.value)}
            placeholder="Nama grup baru (mis. tim-backend)"
          />
          <button type="submit" className="btn btn-primary">
            Buat Grup
          </button>
        </form>

        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>ID</th>
                <th>Nama Grup</th>
                <th>Leader Grup</th>
                <th>Aksi</th>
              </tr>
            </thead>
            <tbody>
              {groups.length === 0 ? (
                <tr>
                  <td colSpan={4} className="muted">Belum ada grup.</td>
                </tr>
              ) : (
                groups.map((g) => (
                  <tr key={g.id}>
                    <td>{g.id}</td>
                    <td><strong>{g.name}</strong></td>
                    <td>
                      {g.leaderUserId ? (
                        <span className="badge badge-success">{g.leaderUsername} (#{g.leaderUserId})</span>
                      ) : (
                        <span className="badge badge-warn">Belum ada leader</span>
                      )}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button
                          type="button"
                          className={`btn btn-sm ${selectedGroupId === g.id ? "btn-primary" : "btn-outline"}`}
                          onClick={() => handleSelectGroup(g)}
                        >
                          Kelola Grup
                        </button>
                        <button type="button" className="btn btn-danger btn-sm" onClick={() => handleDeleteGroup(g.id, g.name)}>
                          Hapus
                        </button>
                      </div>
                    </td>
                  </tr>
                ))
              )}
            </tbody>
          </table>
        </div>

        {selectedGroup && (
          <div className="subcard">
            <div className="section-title-row">
              <h3>Detail Grup: {selectedGroup.name}</h3>
              <span className={selectedGroup.leaderUserId ? "badge badge-success" : "badge badge-warn"}>
                Leader: {selectedGroup.leaderUserId ? selectedGroup.leaderUsername : "Belum ditetapkan"}
              </span>
            </div>

            <div className="subcard" style={{ marginTop: 0, marginBottom: "1rem", background: "#ffffff" }}>
              <h4>Tetapkan / Ganti Leader Grup</h4>
              <form className="inline-form" style={{ marginBottom: 0 }} onSubmit={handleSetLeader}>
                <select value={leaderUserId} onChange={(e) => setLeaderUserId(e.target.value)} required>
                  <option value="">-- Pilih anggota aktif sebagai leader --</option>
                  {activeGroupMembers.map((m) => (
                    <option key={m.id} value={m.id}>
                      {m.username} (#{m.id})
                    </option>
                  ))}
                </select>
                <button type="submit" className="btn btn-primary btn-sm" disabled={activeGroupMembers.length === 0}>
                  Simpan Leader
                </button>
              </form>
              {activeGroupMembers.length === 0 && (
                <p className="muted" style={{ margin: "0.4rem 0 0" }}>
                  Tambahkan anggota aktif ke grup terlebih dahulu sebelum menetapkan leader.
                </p>
              )}
            </div>

            <div className="split-columns">
              <div>
                <h4>Anggota Grup</h4>
                <form className="inline-form" onSubmit={handleAddMember}>
                  <select value={memberUserId} onChange={(e) => setMemberUserId(e.target.value)} required>
                    <option value="">-- Pilih pengguna --</option>
                    {users.map((u) => (
                      <option key={u.id} value={u.id}>
                        {u.username} {u.disabled ? "(nonaktif)" : ""}
                      </option>
                    ))}
                  </select>
                  <button type="submit" className="btn btn-outline btn-sm">
                    Tambah
                  </button>
                </form>
                <ul className="item-list">
                  {groupMembers.length === 0 ? (
                    <li className="muted">Belum ada anggota.</li>
                  ) : (
                    groupMembers.map((m) => (
                      <li key={m.id}>
                        <span>
                          {m.username}{" "}
                          {selectedGroup.leaderUserId === m.id && <span className="badge badge-success">Leader</span>}
                          {m.disabled && <span className="badge badge-danger">Nonaktif</span>}
                        </span>
                        <button type="button" className="btn btn-danger btn-sm" onClick={() => handleRemoveMember(m.id)}>
                          Keluarkan
                        </button>
                      </li>
                    ))
                  )}
                </ul>
              </div>

              <div>
                <h4>Izin Akses Database</h4>
                <form className="inline-form" onSubmit={handleGrantDB}>
                  <select value={grantDbId} onChange={(e) => setGrantDbId(e.target.value)} required>
                    <option value="">-- Pilih database --</option>
                    {databases.map((db) => (
                      <option key={db.id} value={db.id}>
                        {db.name} ({db.id})
                      </option>
                    ))}
                  </select>
                  <select value={grantAccess} onChange={(e) => setGrantAccess(e.target.value)}>
                    <option value="view_only">View Only</option>
                    <option value="view_edit">View + Edit</option>
                  </select>
                  <button type="submit" className="btn btn-primary btn-sm">
                    Simpan Izin
                  </button>
                </form>
                <ul className="item-list">
                  {groupGrants.length === 0 ? (
                    <li className="muted">Belum ada izin database.</li>
                  ) : (
                    groupGrants.map((g) => (
                      <li key={g.databaseId}>
                        <span>
                          <code>{g.databaseId}</code> — <strong>{formatAccess(g.access)}</strong>
                        </span>
                        <button type="button" className="btn btn-danger btn-sm" onClick={() => handleRevokeDB(g.databaseId)}>
                          Cabut
                        </button>
                      </li>
                    ))
                  )}
                </ul>
              </div>
            </div>
          </div>
        )}
      </section>

      {/* 3. Users */}
      <section className="card">
        <h2>3. Pengguna Platform</h2>
        <form className="inline-form" onSubmit={handleCreateUser}>
          <input
            type="text"
            required
            value={newUser.username}
            onChange={(e) => setNewUser({ ...newUser, username: e.target.value })}
            placeholder="Username (3-64 karakter)"
          />
          <input
            type="password"
            required
            value={newUser.password}
            onChange={(e) => setNewUser({ ...newUser, password: e.target.value })}
            placeholder="Password (minimal 12 karakter)"
          />
          <button type="submit" className="btn btn-primary">
            Tambah Pengguna
          </button>
        </form>

        <div className="table-scroll">
          <table className="data-table">
            <thead>
              <tr>
                <th>ID</th>
                <th>Username</th>
                <th>Peran</th>
                <th>Status</th>
                <th>Aksi</th>
              </tr>
            </thead>
            <tbody>
              {users.map((u) => (
                <tr key={u.id}>
                  <td>{u.id}</td>
                  <td><strong>{u.username}</strong></td>
                  <td>
                    <span className={`badge ${u.platformAdmin ? "badge-success" : "badge-neutral"}`}>
                      {u.platformAdmin ? "Platform Admin" : "Pengguna"}
                    </span>
                  </td>
                  <td>
                    <span className={`badge ${u.disabled ? "badge-danger" : "badge-success"}`}>
                      {u.disabled ? "Nonaktif" : "Aktif"}
                    </span>
                  </td>
                  <td>
                    <div className="row-actions">
                      <button type="button" className="btn btn-outline btn-sm" onClick={() => handleResetPassword(u)}>
                        Ganti Password
                      </button>
                      <button type="button" className="btn btn-outline btn-sm" onClick={() => handleToggleAdmin(u)}>
                        {u.platformAdmin ? "Cabut Admin" : "Jadikan Admin"}
                      </button>
                      <button
                        type="button"
                        className={`btn btn-sm ${u.disabled ? "btn-success" : "btn-danger"}`}
                        onClick={() => handleToggleDisabled(u)}
                      >
                        {u.disabled ? "Aktifkan" : "Nonaktifkan"}
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </section>
    </div>
  );
}
