const TRUNCATE_LIMIT = 4096;
const REFRESH_MS = 10000;
const STATUSES = ["", "created", "running", "waiting_for_input", "achieved", "blocked", "budget_limited", "max_iterations", "errored", "canceled", "interrupted"];
const SSE_EVENT_TYPES = [
  "message", "job_created", "job_started", "job_waiting", "job_resumed", "job_done",
  "plan", "iteration_start", "maker_done", "verdict", "done", "blocked", "budget_limited", "max_iterations",
  "operator_message", "operator_message_delivered", "run_interrupted",
  "permission_request", "permission_decision", "permission_expired", "permission_auto",
  "loop_start", "loop_end", "turn_start", "turn_end", "message_start", "message_end",
  "text_delta", "tool_start", "tool_end", "error", "retry", "guardrail", "auto_continue",
];

let csrfToken = "";
let listTimer = null;
let activeStream = null;
let statusFilter = "";

export async function fetchSession() {
  const resp = await fetch("/v1/session", { credentials: "same-origin" });
  if (!resp.ok) { csrfToken = ""; return null; }
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
  if (!resp.ok) throw new Error("Invalid daemon token");
  const session = await fetchSession();
  if (!session) throw new Error("Session verification failed");
  return session;
}

export function getCSRFToken() { return csrfToken; }

export async function apiRequest(path, options = {}) {
  const method = (options.method || "GET").toUpperCase();
  const headers = { ...(options.headers || {}) };
  if (method !== "GET" && method !== "HEAD" && method !== "OPTIONS" && csrfToken) headers["X-Glue-CSRF"] = csrfToken;
  const resp = await fetch(path, { ...options, method, headers, credentials: "same-origin" });
  if (resp.status === 401) {
    csrfToken = "";
    renderLogin();
    throw new Error("Unauthorized");
  }
  if (!resp.ok) {
    const errBody = await resp.json().catch(() => ({}));
    throw new Error(errBody?.error?.message || `HTTP ${resp.status}`);
  }
  return resp.json();
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === undefined || v === null || v === false) continue;
    if (k === "className") node.className = v;
    else if (k === "textContent") node.textContent = String(v);
    else if (k === "hidden") node.hidden = Boolean(v);
    else if (k.startsWith("on") && typeof v === "function") node.addEventListener(k.slice(2).toLowerCase(), v);
    else node.setAttribute(k, String(v));
  }
  for (const child of children.flat()) {
    if (child === undefined || child === null || child === false) continue;
    node.appendChild(typeof child === "string" ? document.createTextNode(child) : child);
  }
  return node;
}

const truncate = (s, max = 80) => (!s ? "" : s.length > max ? s.slice(0, max - 1) + "…" : s);
const formatRepo = (spec) => {
  const base = spec?.repo?.url || spec?.repo?.path || "—";
  return spec?.repo?.base_ref ? `${base}@${spec.repo.base_ref}` : base;
};
const formatModel = (spec) => [spec?.provider, spec?.model].filter(Boolean).join("/") || "default";
const formatUsage = (u) => `${u?.total_tokens || 0} tok (${u?.input_tokens || 0} in / ${u?.output_tokens || 0} out)`;
const formatTime = (iso) => (iso ? new Date(iso).toLocaleTimeString([], { hour: "2-digit", minute: "2-digit", second: "2-digit" }) : "—");
function formatJSON(val) {
  if (val === undefined || val === null || val === "") return "";
  if (typeof val === "string") {
    try { return JSON.stringify(JSON.parse(val), null, 2); } catch { return val; }
  }
  try { return JSON.stringify(val, null, 2); } catch { return String(val); }
}

function stopBackground() {
  if (listTimer) { clearInterval(listTimer); listTimer = null; }
  if (activeStream) { activeStream.close(); activeStream = null; }
}

function setSessionBanner(text) {
  const s = document.getElementById("session-status");
  if (s) s.textContent = text;
}

function renderLogin() {
  stopBackground();
  setSessionBanner("Sign in required");
  const root = document.getElementById("app");
  if (!root) return;
  const input = el("input", { className: "input", type: "password", name: "token", placeholder: "Daemon token", required: "true", "data-testid": "login-token" });
  const errEl = el("p", { className: "error-text", "data-testid": "login-error", hidden: true });
  const btn = el("button", { className: "btn", type: "submit", "data-testid": "login-submit" }, "Sign in");
  const form = el("form", {
    className: "login-form", "data-testid": "login-form",
    onsubmit: async (e) => {
      e.preventDefault();
      errEl.hidden = true;
      btn.disabled = true;
      try {
        await login(input.value.trim());
        setSessionBanner("Authenticated");
        await route();
      } catch (err) {
        errEl.textContent = err.message || "Login failed";
        errEl.hidden = false;
      } finally {
        btn.disabled = false;
      }
    },
  }, input, btn, errEl);
  root.replaceChildren(el("section", { className: "card login-card", "data-testid": "login-view" }, el("h2", {}, "Sign in to Glue"), el("p", { className: "muted" }, "Enter the daemon token to inspect and watch jobs."), form));
}

async function renderJobsList(root) {
  stopBackground();
  const listEl = el("div", { className: "jobs-list", "data-testid": "jobs-list" });
  const select = el("select", {
    className: "select", "data-testid": "status-filter",
    onchange: (e) => { statusFilter = e.target.value; load(); },
  }, STATUSES.map((st) => el("option", { value: st, ...(st === statusFilter ? { selected: "selected" } : {}) }, st || "All statuses")));
  const refreshBtn = el("button", { className: "btn", type: "button", "data-testid": "refresh-jobs", onclick: () => load() }, "Refresh");
  root.replaceChildren(el("section", { "data-testid": "jobs-view" },
    el("div", { className: "toolbar" }, el("h2", {}, "Jobs"), el("div", { className: "toolbar-controls" }, select, refreshBtn, el("span", { className: "muted", "data-testid": "auto-refresh-label" }, "Auto-refresh: 10s"))),
    listEl
  ));

  async function load() {
    try {
      const q = statusFilter ? `?status=${encodeURIComponent(statusFilter)}` : "";
      const data = await apiRequest(`/v1/jobs${q}`);
      const jobs = await Promise.all((data.jobs || []).map((j) => apiRequest(`/v1/jobs/${encodeURIComponent(j.id)}`).catch(() => j)));
      if (!jobs.length) {
        listEl.replaceChildren(el("p", { className: "muted", "data-testid": "jobs-empty" }, "No jobs found."));
        return;
      }
      listEl.replaceChildren(...jobs.map((j) => {
        const maxIter = j.spec?.budgets?.max_iterations || 10;
        return el("a", { className: "card job-row", href: `#/jobs/${encodeURIComponent(j.id)}`, "data-testid": "job-row", "data-job-id": j.id },
          el("span", { className: `badge status-${j.status}`, "data-testid": "job-status" }, j.status),
          el("div", {},
            el("div", { className: "job-objective", "data-testid": "job-objective", title: j.spec?.objective || "" }, truncate(j.spec?.objective || j.id, 80)),
            el("div", { className: "job-meta" }, el("span", { "data-testid": "job-repo" }, formatRepo(j.spec)), el("span", { "data-testid": "job-model" }, formatModel(j.spec)))
          ),
          el("div", { className: "job-stats" },
            el("span", { "data-testid": "job-iteration" }, `iter ${j.iteration ?? 0}/${maxIter}`),
            el("time", { "data-testid": "job-updated", datetime: j.updated_at || "" }, formatTime(j.updated_at))
          )
        );
      }));
    } catch (err) {
      if (csrfToken) listEl.replaceChildren(el("p", { className: "error-text" }, err.message));
    }
  }
  await load();
  listTimer = setInterval(load, REFRESH_MS);
}

export function renderTruncatedResult(container, fullText) {
  const text = String(fullText || "");
  const pre = el("pre", { className: "pre-wrap tool-output", "data-testid": "tool-output" });
  if (text.length > TRUNCATE_LIMIT) {
    pre.textContent = text.slice(0, TRUNCATE_LIMIT) + "\n… [truncated]";
    const btn = el("button", { className: "btn-link show-more", type: "button", "data-testid": "tool-show-more", onclick: () => { pre.textContent = text; btn.remove(); } }, `Show more (${text.length - TRUNCATE_LIMIT} more chars)`);
    container.replaceChildren(el("div", { className: "tool-label" }, "Result"), pre, btn);
  } else {
    pre.textContent = text || "(empty)";
    container.replaceChildren(el("div", { className: "tool-label" }, "Result"), pre);
  }
}

export function createTranscriptState(refs) {
  return { ...refs, seenSeqs: new Set(), lastSeq: 0, finished: false, currentTextEl: null, currentTextKey: "", toolCards: new Map(), lastTool: null, permissionCards: new Map() };
}

export function applyJobEvent(state, env) {
  const seq = Number(env?.seq || 0);
  if (seq > 0) {
    if (state.seenSeqs.has(seq)) return false;
    state.seenSeqs.add(seq);
    if (seq > state.lastSeq) state.lastSeq = seq;
  }
  const p = env?.payload || {};
  if (env.type === "text_delta") {
    const delta = p.delta || "";
    if (!delta || (env.phase && env.phase !== "maker")) return true;
    const key = `${env.phase || "maker"}:${env.iteration ?? ""}:${env.session_id || ""}`;
    if (!state.currentTextEl || state.currentTextKey !== key) {
      const pre = el("pre", { className: "pre-wrap assistant-text", "data-testid": "assistant-text" });
      state.transcriptEl.appendChild(el("div", { className: "transcript-item assistant-message", "data-testid": "transcript-item", "data-event-type": "text_delta", "data-seq": seq }, pre));
      state.currentTextEl = pre;
      state.currentTextKey = key;
    }
    state.currentTextEl.textContent += delta;
    return true;
  }
  state.currentTextEl = null;

  if (env.type === "tool_start") {
    const call = p.tool_call || {};
    const name = p.tool_name || call.name || "tool";
    const id = p.tool_call_id || call.id || `${name}-${seq}`;
    const statusEl = el("span", { className: "badge status-running", "data-testid": "tool-status" }, "running");
    const resultEl = el("div", { className: "tool-result", "data-testid": "tool-result" });
    const details = el("details", { className: "transcript-item tool-call", "data-testid": "tool-call", "data-tool-name": name, "data-seq": seq },
      el("summary", { className: "tool-summary", "data-testid": "tool-summary" }, el("span", { "data-testid": "tool-name" }, name), statusEl),
      el("div", { className: "tool-body" }, el("div", { className: "tool-label" }, "Arguments"), el("pre", { className: "pre-wrap tool-args", "data-testid": "tool-args" }, formatJSON(call.arguments) || "{}"), resultEl)
    );
    state.transcriptEl.appendChild(details);
    const entry = { details, statusEl, resultEl };
    state.toolCards.set(id, entry);
    state.lastTool = entry;
    return true;
  }

  if (env.type === "tool_end") {
    const call = p.tool_call || {};
    const card = state.toolCards.get(p.tool_call_id || call.id || "") || state.lastTool;
    const out = (p.tool_result?.content || []).map((c) => c.text || "").filter(Boolean).join("\n") || p.error || "";
    const isErr = Boolean(p.tool_result?.is_error || p.error);
    if (card) {
      card.statusEl.className = `badge ${isErr ? "status-errored" : "status-achieved"}`;
      card.statusEl.textContent = isErr ? "error" : "done";
      renderTruncatedResult(card.resultEl, out);
    }
    return true;
  }

  if (env.type === "permission_request") {
    const permId = p.permission_id || `perm-${seq}`;
    const req = p.request || {};
    state.updateStatus("waiting_for_input");
    if (!state.permissionCards.has(permId)) {
      const stateBadge = el("span", { className: "badge status-waiting_for_input", "data-testid": "permission-state" }, "pending");
      const toolLabel = req.action && req.action !== req.tool ? `${req.tool || "tool"} (${req.action})` : (req.tool || req.action || "tool");
      const card = el("div", { className: "transcript-item permission-card", "data-testid": "permission-card", "data-permission-id": permId, "data-seq": seq },
        el("div", { className: "permission-header" }, el("span", { "data-testid": "permission-tool" }, `Permission: ${toolLabel}`), stateBadge),
        req.target ? el("div", { className: "muted", "data-testid": "permission-target" }, `Target: ${req.target}`) : null,
        req.args ? el("pre", { className: "pre-wrap", "data-testid": "permission-args" }, formatJSON(req.args)) : null
      );
      state.transcriptEl.appendChild(card);
      state.permissionCards.set(permId, { card, stateBadge });
    }
    return true;
  }

  if (env.type === "permission_decision" || env.type === "permission_expired") {
    const nextState = env.type === "permission_expired" ? "expired" : p.decision?.allow ? "allowed" : "denied";
    const entry = state.permissionCards.get(p.permission_id || "");
    if (entry) {
      entry.stateBadge.className = `badge status-${nextState}`;
      entry.stateBadge.textContent = nextState;
    }
    const anyPending = [...state.permissionCards.values()].some((c) => c.stateBadge.textContent === "pending");
    state.updateStatus(anyPending ? "waiting_for_input" : "running");
    return true;
  }

  const checklist = p.Checklist ?? p.checklist ?? p.result?.Checklist ?? p.result?.checklist;
  const iter = p.Iteration ?? p.iteration ?? p.from_iteration ?? p.result?.Iterations ?? p.result?.iterations ?? env.iteration;
  const usage = p.Usage ?? p.usage ?? p.result?.Usage ?? p.result?.usage;
  if (Array.isArray(checklist)) state.updateChecklist(checklist);
  if (typeof iter === "number" && iter > 0) state.updateIteration(iter);
  if (usage && typeof usage === "object") state.updateUsage(usage);

  const dividers = {
    plan: () => `Plan (${(checklist || []).length} items)`,
    iteration_start: () => `Iteration ${iter || 1} started`,
    verdict: () => `Verdict: ${(checklist || []).filter((i) => i.done).length}/${(checklist || []).length} ✓ — ${p.Message ?? p.message ?? ""}`,
    operator_message: () => `Operator: ${p.text || ""}`,
    run_interrupted: () => `Interrupted: ${p.reason || p.message || "run interrupted"}`,
    job_resumed: () => `Resumed at iteration ${p.from_iteration || iter || 1} (${p.reason || "resumed"})`,
    job_done: () => {
      const st = p.status || "achieved";
      const summary = p.result?.Summary ?? p.result?.summary ?? p.error ?? "";
      for (const c of state.permissionCards.values()) {
        if (c.stateBadge.textContent === "pending") { c.stateBadge.className = "badge status-expired"; c.stateBadge.textContent = "expired"; }
      }
      state.updateStatus(st);
      state.finished = true;
      return `Job ${st}${summary ? " — " + summary : ""}`;
    },
  };
  if (dividers[env.type]) {
    state.transcriptEl.appendChild(el("div", { className: "transcript-item goal-divider", "data-testid": "goal-divider", "data-event-type": env.type, "data-seq": seq }, dividers[env.type]()));
  }
  return true;
}

async function renderJobDetail(root, jobId) {
  stopBackground();
  const detail = await apiRequest(`/v1/jobs/${encodeURIComponent(jobId)}`);
  const maxIter = detail.spec?.budgets?.max_iterations || 10;
  const statusEl = el("span", { className: `badge status-${detail.status}`, "data-testid": "detail-status" }, detail.status);
  const iterEl = el("span", { "data-testid": "detail-iteration" }, `iter ${detail.iteration ?? 0}/${maxIter}`);
  const usageEl = el("span", { "data-testid": "detail-usage" }, formatUsage(detail.usage));
  const streamEl = el("span", { className: "muted", "data-testid": "stream-status" }, "connecting…");
  const checklistEl = el("ul", { className: "checklist", "data-testid": "job-checklist" });
  const transcriptEl = el("div", { className: "transcript", "data-testid": "job-transcript" });

  const state = createTranscriptState({
    transcriptEl,
    updateStatus: (st) => { statusEl.className = `badge status-${st}`; statusEl.textContent = st; },
    updateIteration: (n) => { iterEl.textContent = `iter ${n}/${maxIter}`; },
    updateUsage: (u) => { usageEl.textContent = formatUsage(u); },
    updateChecklist: (items) => {
      if (!items?.length) {
        checklistEl.replaceChildren(el("li", { className: "muted" }, "No checklist items yet."));
        return;
      }
      checklistEl.replaceChildren(...items.map((item) =>
        el("li", { className: `checklist-item ${item.done ? "checklist-done" : ""}`, "data-testid": "checklist-item", "data-done": String(Boolean(item.done)) },
          el("span", { className: "checklist-mark", "data-testid": "checklist-mark" }, item.done ? "[x]" : "[ ]"),
          el("div", {}, el("span", { "data-testid": "checklist-title" }, item.title || ""), item.evidence ? el("span", { className: "checklist-evidence" }, item.evidence) : null)
        )
      ));
    },
  });
  state.updateChecklist(detail.checklist || []);

  root.replaceChildren(el("section", { "data-testid": "job-detail-view" },
    el("a", { className: "back-link", href: "#/", "data-testid": "back-to-jobs" }, "← All jobs"),
    el("div", { className: "card", "data-testid": "job-header" },
      el("div", { className: "detail-header-top" }, statusEl, el("h2", { className: "detail-objective", "data-testid": "detail-objective" }, detail.spec?.objective || jobId)),
      el("div", { className: "detail-meta" }, iterEl, el("span", { "data-testid": "detail-model" }, formatModel(detail.spec)), el("span", { "data-testid": "detail-repo" }, formatRepo(detail.spec)), usageEl, streamEl)
    ),
    el("div", { className: "detail-grid" },
      el("div", { className: "card" }, el("h3", {}, "Checklist"), checklistEl),
      el("div", { className: "card" }, el("h3", {}, "Transcript"), transcriptEl)
    )
  ));

  let es = null;
  let retryTimer = null;
  function openSSE(useAfter = false) {
    if (state.finished) return;
    if (es) es.close();
    const suffix = useAfter && state.lastSeq > 0 ? `?after=${state.lastSeq}` : "";
    es = new EventSource(`/v1/jobs/${encodeURIComponent(jobId)}/events${suffix}`);
    es.onopen = () => { streamEl.textContent = "live"; };
    const handle = (ev) => {
      try {
        const env = JSON.parse(ev.data);
        if (!env.seq && ev.lastEventId) env.seq = Number(ev.lastEventId);
        applyJobEvent(state, env);
        if (state.finished) { streamEl.textContent = "complete"; es.close(); }
      } catch { /* ignore malformed frame */ }
    };
    for (const t of SSE_EVENT_TYPES) es.addEventListener(t, handle);
    es.onerror = () => {
      if (state.finished) { es.close(); return; }
      streamEl.textContent = "reconnecting…";
      if (es.readyState === EventSource.CLOSED && !retryTimer) {
        retryTimer = setTimeout(() => { retryTimer = null; openSSE(true); }, 1000);
      }
    };
  }
  const onOnline = () => { if (!state.finished && (!es || es.readyState === EventSource.CLOSED)) openSSE(true); };
  window.addEventListener("online", onOnline);
  activeStream = {
    close: () => {
      state.finished = true;
      if (retryTimer) clearTimeout(retryTimer);
      window.removeEventListener("online", onOnline);
      if (es) es.close();
    },
  };
  openSSE(false);
}

async function route() {
  const root = document.getElementById("app");
  if (!root) return;
  const session = csrfToken ? { authenticated: true } : await fetchSession();
  if (!session) { renderLogin(); return; }
  setSessionBanner("Authenticated");
  const match = (window.location.hash || "#/").match(/^#\/jobs\/([^/?#]+)/);
  if (match) {
    await renderJobDetail(root, decodeURIComponent(match[1])).catch((err) => {
      if (csrfToken) root.replaceChildren(el("p", { className: "error-text" }, err.message));
    });
  } else {
    await renderJobsList(root);
  }
}

if (typeof window !== "undefined" && typeof document !== "undefined") {
  window.addEventListener("hashchange", () => route());
  if (document.readyState === "loading") document.addEventListener("DOMContentLoaded", () => route());
  else route();
}
