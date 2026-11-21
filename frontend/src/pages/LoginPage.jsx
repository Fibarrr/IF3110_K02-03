import React, { useState } from "react";
import { apiFetch } from "../api.js";

export default function LoginPage({ user, onLoggedIn, onGoHome }) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState("");

  if (user) {
    return (
      <section className="card login-card">
        <h1>Sudah Masuk</h1>
        <p className="muted">
          Anda sedang masuk sebagai <strong>{user.username}</strong>.
        </p>
        <button type="button" className="btn btn-primary" onClick={onGoHome}>
          Buka Daftar Database
        </button>
      </section>
    );
  }

  const handleSubmit = async (e) => {
    e.preventDefault();
    setError("");
    setSubmitting(true);
    try {
      const data = await apiFetch("/api/auth/login", {
        method: "POST",
        body: { username: username.trim(), password },
      });
      onLoggedIn(data.user, data.csrfToken);
    } catch (err) {
      setError(err.message || "Gagal masuk");
    } finally {
      setSubmitting(false);
    }
  };

  return (
    <section className="card login-card">
      <h1>Masuk ke Platform</h1>
      <p className="muted" style={{ marginBottom: "1rem" }}>
        Gunakan akun platform untuk mengakses database dan mengelola migration.
      </p>
      {error && (
        <div className="alert alert-error" role="alert">
          <span>{error}</span>
        </div>
      )}
      <form className="form-stack" onSubmit={handleSubmit}>
        <div className="field">
          <label htmlFor="login-username">Username</label>
          <input
            id="login-username"
            type="text"
            autoComplete="username"
            required
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            placeholder="mis. admin"
          />
        </div>
        <div className="field">
          <label htmlFor="login-password">Password</label>
          <input
            id="login-password"
            type="password"
            autoComplete="current-password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Masukkan password"
          />
        </div>
        <button type="submit" className="btn btn-primary" disabled={submitting}>
          {submitting ? "Memeriksa..." : "Masuk"}
        </button>
      </form>
    </section>
  );
}
