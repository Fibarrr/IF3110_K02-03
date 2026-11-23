import React, { useCallback, useEffect, useState } from "react";
import { apiFetch, clearCSRFToken, setCSRFToken } from "./api.js";
import LoginPage from "./pages/LoginPage.jsx";
import WorkspacePage from "./pages/WorkspacePage.jsx";
import AdminPage from "./pages/AdminPage.jsx";
import DatabasePage from "./pages/DatabasePage.jsx";

function parseRoute(pathname) {
  if (pathname === "/login") return { page: "login" };
  if (pathname === "/admin") return { page: "admin" };
  const dbMatch = pathname.match(/^\/databases\/([^/]+)$/);
  if (dbMatch) {
    return { page: "database", databaseId: decodeURIComponent(dbMatch[1]) };
  }
  return { page: "workspace" };
}

export default function App() {
  const [path, setPath] = useState(() => window.location.pathname || "/");
  const [user, setUser] = useState(null);
  const [authLoading, setAuthLoading] = useState(true);
  const [alert, setAlert] = useState(null);
  const [mobileMenuOpen, setMobileMenuOpen] = useState(false);

  const navigate = useCallback((nextPath, { replace = false } = {}) => {
    setMobileMenuOpen(false);
    setAlert(null);
    if (nextPath === window.location.pathname) {
      setPath(nextPath);
      return;
    }
    if (replace) {
      window.history.replaceState({}, "", nextPath);
    } else {
      window.history.pushState({}, "", nextPath);
    }
    setPath(nextPath);
  }, []);

  useEffect(() => {
    const onPopState = () => {
      setPath(window.location.pathname || "/");
      setMobileMenuOpen(false);
    };
    window.addEventListener("popstate", onPopState);
    return () => window.removeEventListener("popstate", onPopState);
  }, []);

  const refreshSession = useCallback(async () => {
    setAuthLoading(true);
    try {
      const data = await apiFetch("/api/auth/me");
      setUser(data.user || null);
      if (data.csrfToken) setCSRFToken(data.csrfToken);
      return data.user || null;
    } catch (err) {
      if (err.status === 401) {
        setUser(null);
        clearCSRFToken();
      }
      return null;
    } finally {
      setAuthLoading(false);
    }
  }, []);

  useEffect(() => {
    refreshSession();
  }, [refreshSession]);

  const route = parseRoute(path);

  useEffect(() => {
    if (!authLoading && !user && route.page !== "login") {
      navigate("/login", { replace: true });
    }
  }, [authLoading, user, route.page, navigate]);

  const notify = useCallback((message, type = "info") => {
    if (!message) {
      setAlert(null);
      return;
    }
    setAlert({ message, type });
  }, []);

  const handleLogout = async () => {
    try {
      await apiFetch("/api/auth/logout", { method: "POST" });
    } catch {
      // Ignore logout errors and clear local state anyway.
    }
    setUser(null);
    clearCSRFToken();
    navigate("/login", { replace: true });
  };

  const handleLoggedIn = (loggedInUser, csrfToken) => {
    setUser(loggedInUser);
    if (csrfToken) setCSRFToken(csrfToken);
    navigate("/", { replace: true });
  };

  if (route.page === "login") {
    return (
      <div className="app-shell">
        <header className="topbar">
          <div className="topbar-left">
            <span className="brand">Platform Manajemen Database</span>
          </div>
        </header>
        <main className="login-shell">
          <LoginPage
            user={user}
            onLoggedIn={handleLoggedIn}
            onGoHome={() => navigate("/", { replace: true })}
          />
        </main>
      </div>
    );
  }

  return (
    <div className="app-shell">
      <header className="topbar">
        <div className="topbar-left">
          <button type="button" className="brand" onClick={() => navigate("/")}>
            Platform Manajemen Database
          </button>
          <nav className="nav-links" aria-label="Navigasi utama">
            <button
              type="button"
              className={`nav-link ${route.page === "workspace" || route.page === "database" ? "active" : ""}`}
              onClick={() => navigate("/")}
            >
              Database
            </button>
            {user?.platformAdmin && (
              <button
                type="button"
                className={`nav-link ${route.page === "admin" ? "active" : ""}`}
                onClick={() => navigate("/admin")}
              >
                Panel Admin
              </button>
            )}
          </nav>
        </div>

        <div className="topbar-right">
          {user && (
            <>
              <span className="user-chip" title={user.username}>
                {user.username}
              </span>
              <span className={`badge ${user.platformAdmin ? "badge-success" : "badge-neutral"}`}>
                {user.platformAdmin ? "Platform Admin" : "Pengguna"}
              </span>
              <button type="button" className="btn btn-ghost btn-sm" onClick={handleLogout}>
                Keluar
              </button>
            </>
          )}
        </div>

        <button
          type="button"
          className="menu-toggle"
          aria-expanded={mobileMenuOpen}
          onClick={() => setMobileMenuOpen((v) => !v)}
        >
          {mobileMenuOpen ? "Tutup" : "Menu"}
        </button>
      </header>

      <div className={`mobile-drawer ${mobileMenuOpen ? "open" : ""}`}>
        {user && (
          <div className="row-actions" style={{ justifyContent: "space-between" }}>
            <span>
              <strong>{user.username}</strong> ({user.platformAdmin ? "Platform Admin" : "Pengguna"})
            </span>
          </div>
        )}
        <button type="button" className="nav-link" style={{ textAlign: "left" }} onClick={() => navigate("/")}>
          Daftar Database
        </button>
        {user?.platformAdmin && (
          <button type="button" className="nav-link" style={{ textAlign: "left" }} onClick={() => navigate("/admin")}>
            Panel Platform Admin
          </button>
        )}
        {user && (
          <button type="button" className="btn btn-danger btn-sm" onClick={handleLogout}>
            Keluar
          </button>
        )}
      </div>

      <main className="main-container">
        {alert && (
          <div className={`alert alert-${alert.type}`} role="status">
            <span>{alert.message}</span>
            <button type="button" className="btn btn-outline btn-sm" onClick={() => setAlert(null)}>
              Tutup
            </button>
          </div>
        )}

        {authLoading ? (
          <div className="card">
            <p className="muted">Memeriksa sesi pengguna...</p>
            <div className="skeleton-box" />
          </div>
        ) : !user ? null : route.page === "admin" ? (
          <AdminPage user={user} notify={notify} navigate={navigate} />
        ) : route.page === "database" ? (
          <DatabasePage databaseId={route.databaseId} user={user} notify={notify} navigate={navigate} />
        ) : (
          <WorkspacePage user={user} notify={notify} navigate={navigate} />
        )}
      </main>
    </div>
  );
}
