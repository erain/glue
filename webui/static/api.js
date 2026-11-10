let csrfToken = "";
let onUnauthorized = null;

export const setUnauthorizedHandler = (fn) => { onUnauthorized = fn; };
export const getCSRFToken = () => csrfToken;

export async function fetchSession() {
  const resp = await fetch("/v1/session", { credentials: "same-origin" });
  if (!resp.ok) { csrfToken = ""; return null; }
  const data = await resp.json();
  csrfToken = data.csrf_token || "";
  return data;
}

export async function login(token) {
  const resp = await fetch("/v1/session/login", {
    method: "POST", credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
  if (!resp.ok) throw new Error("Invalid daemon token");
  const session = await fetchSession();
  if (!session) throw new Error("Session verification failed");
  return session;
}

export async function apiRequest(path, options = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = { ...(options.headers || {}) };
  if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS" && csrfToken) headers["X-Glue-CSRF"] = csrfToken;
  const init = { ...options, method, headers, credentials: "same-origin" };
  if (options.json !== undefined) {
    headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(options.json);
    delete init.json;
  }
  const resp = await fetch(path, init);
  if (resp.status === 401) { csrfToken = ""; onUnauthorized?.(); throw new Error("Unauthorized"); }
  if (!resp.ok) {
    const errBody = await resp.json().catch(() => ({}));
    throw new Error(errBody?.error?.message || `HTTP ${resp.status}`);
  }
  return resp.json();
}

export const apiPost = (path, json) => apiRequest(path, { method: "POST", json });
export const apiDelete = (path) => apiRequest(path, { method: "DELETE" });
