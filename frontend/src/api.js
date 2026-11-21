let cachedCSRFToken = "";

function readCookie(name) {
  const prefix = `${name}=`;
  const parts = document.cookie ? document.cookie.split("; ") : [];
  for (const part of parts) {
    if (part.startsWith(prefix)) {
      return decodeURIComponent(part.slice(prefix.length));
    }
  }
  return "";
}

export function setCSRFToken(token) {
  if (token) cachedCSRFToken = token;
}

export function clearCSRFToken() {
  cachedCSRFToken = "";
}

export function getCSRFToken() {
  return readCookie("if3110_csrf") || cachedCSRFToken;
}

export async function apiFetch(path, options = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = { ...(options.headers || {}) };
  let body = options.body;

  if (method !== "GET" && method !== "HEAD") {
    const token = getCSRFToken();
    if (token) {
      headers["X-CSRF-Token"] = token;
    }
    if (body !== undefined && body !== null && typeof body === "object" && !(body instanceof FormData)) {
      headers["Content-Type"] = "application/json";
      body = JSON.stringify(body);
    }
  }

  const response = await fetch(path, {
    credentials: "same-origin",
    ...options,
    method,
    headers,
    body,
  });

  let payload = null;
  const contentType = response.headers.get("Content-Type") || "";
  if (contentType.includes("application/json")) {
    try {
      payload = await response.json();
    } catch {
      payload = null;
    }
  }

  if (!response.ok) {
    const message = payload?.error || `Permintaan gagal (${response.status})`;
    const err = new Error(message);
    err.status = response.status;
    err.payload = payload;
    throw err;
  }
  return payload;
}

export function formatAccess(access) {
  if (access === "view_edit") return "View + Edit";
  if (access === "view_only") return "View Only";
  return "Tidak Ada Akses";
}

export function formatDateTime(value) {
  if (!value) return "-";
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return String(value);
  return d.toLocaleString("id-ID", {
    year: "numeric",
    month: "short",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
    second: "2-digit",
  });
}

export function statusBadgeClass(status) {
  switch (status) {
    case "applied":
    case "succeeded":
    case "approved":
    case "completed":
    case "approve":
      return "badge badge-success";
    case "failed":
    case "rejected":
    case "reject":
    case "invalidated":
    case "uncertain":
      return "badge badge-danger";
    case "running":
    case "queued":
    case "pending_review":
    case "pending":
    case "partial":
    case "partially_reverted":
      return "badge badge-warn";
    case "rolled_back":
      return "badge badge-info";
    default:
      return "badge badge-neutral";
  }
}
