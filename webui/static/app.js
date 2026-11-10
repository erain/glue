let csrfToken = "";

export async function fetchSession() {
  const resp = await fetch("/v1/session", { credentials: "same-origin" });
  if (!resp.ok) {
    csrfToken = "";
    return null;
  }
  const data = await resp.json();
  csrfToken = data.csrf_token || "";
  return data;
}

export async function login(token) {
  const resp = await fetch("/v1/session/login", {
    method: "POST",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ token }),
  });
  if (!resp.ok) {
    throw new Error("Invalid token");
  }
  const data = await resp.json();
  csrfToken = data.csrf_token || "";
  return data;
}

export function getCSRFToken() {
  return csrfToken;
}
