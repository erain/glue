import { fetchSession, login, logout, getCSRFToken, getAuthMode, setUnauthorizedHandler, apiRequest, apiPost, apiDelete } from "./api.js";
export { fetchSession, login, logout, getCSRFToken, getAuthMode, apiRequest, apiPost, apiDelete };

const TRUNCATE_LIMIT = 4096;
const REFRESH_MS = 10000;
const STATUSES = ["", "created", "running", "waiting_for_input", "suspended", "achieved", "blocked", "budget_limited", "max_iterations", "errored", "canceled", "interrupted"];
const SSE_EVENT_TYPES = [
  "message", "job_created", "job_started", "job_waiting", "job_resumed", "job_done", "task_suspended", "task_resumed",
  "plan", "iteration_start", "maker_done", "verdict", "done", "blocked", "budget_limited", "max_iterations",
  "operator_message", "operator_message_delivered", "run_interrupted",
  "question", "answer", "question_timeout", "question_assumed", "turn_interrupted",
  "permission_request", "permission_decision", "permission_expired", "permission_auto",
  "loop_start", "loop_end", "turn_start", "turn_end", "message_start", "message_end",
  "text_delta", "tool_start", "tool_end", "error", "retry", "guardrail", "auto_continue",
];

let listTimer = null;
let activeStream = null;
let statusFilter = "";

const isActiveStatus = (st) => st === "created" || st === "running" || st === "waiting_for_input" || st === "suspended";
const formatStatusLabel = (st) => (st === "suspended" ? "Suspended – waiting for you" : st);
const shortSHA = (sha) => (sha ? String(sha).slice(0, 12) : "—");

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

export function formatCostUSD(usage, pricingOverride) {
  if (typeof pricingOverride === "number" && pricingOverride >= 0) return `$${pricingOverride.toFixed(4)}`;
  let p = pricingOverride || (typeof window !== "undefined" ? window.GLUE_PRICING : null);
  if (!p) {
    try { p = JSON.parse(window.localStorage.getItem("glue_pricing") || "null"); } catch { p = null; }
  }
  if (!p || typeof p !== "object") return "";
  const inR = Number(p.input_usd_per_million ?? p.input ?? 0), outR = Number(p.output_usd_per_million ?? p.output ?? 0);
  const crR = Number(p.cache_read_usd_per_million ?? p.cache_read ?? 0), cwR = Number(p.cache_write_usd_per_million ?? p.cache_write ?? 0);
  if (!inR && !outR && !crR && !cwR) return "";
  const cost = ((usage?.input_tokens || 0) * inR + (usage?.output_tokens || 0) * outR + (usage?.cache_read_tokens || 0) * crR + (usage?.cache_write_tokens || 0) * cwR) / 1e6;
  return `$${cost.toFixed(4)}`;
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
  const ghLink = el("a", { className: "btn", href: "/auth/github/login", "data-testid": "github-login-btn", hidden: getAuthMode() !== "github" }, "Sign in with GitHub");
  root.replaceChildren(el("section", { className: "card login-card", "data-testid": "login-view" }, el("h2", {}, "Sign in to Glue"), el("p", { className: "muted" }, "Sign in with GitHub or enter the daemon token to inspect and watch jobs."), ghLink, form));
}
setUnauthorizedHandler(renderLogin);

async function renderTokensView(root) {
  stopBackground();
  const listEl = el("div", { className: "jobs-list", "data-testid": "tokens-list" });
  const bannerVal = el("pre", { className: "pre-wrap", "data-testid": "token-created-value" });
  const bannerEl = el("div", { className: "card pending-banner", "data-testid": "token-created-banner", hidden: true }, el("strong", {}, "Copy this personal API token now (shown once):"), bannerVal);
  const nameInput = el("input", { className: "input steer-input", type: "text", name: "name", placeholder: "Token name (e.g. cli)", required: "true", "data-testid": "token-name-input" });
  const submitBtn = el("button", { className: "btn", type: "submit", "data-testid": "token-create-submit" }, "Create token");
  const form = el("form", { className: "steer-form", "data-testid": "token-create-form", onsubmit: async (e) => {
    e.preventDefault();
    submitBtn.disabled = true;
    try {
      const created = await apiPost("/v1/tokens", { name: nameInput.value.trim() });
      nameInput.value = "";
      bannerVal.textContent = created.token || "";
      bannerEl.hidden = false;
      await load();
    } finally { submitBtn.disabled = false; }
  } }, nameInput, submitBtn);
  root.replaceChildren(el("section", { "data-testid": "tokens-view" },
    el("a", { className: "back-link", href: "#/", "data-testid": "back-to-jobs" }, "← All jobs"),
    el("div", { className: "card" }, el("h2", {}, "Personal API Tokens"), form),
    bannerEl,
    listEl
  ));
  async function load() {
    const data = await apiRequest("/v1/tokens").catch(() => ({ tokens: [] }));
    const toks = data.tokens || [];
    if (!toks.length) { listEl.replaceChildren(el("p", { className: "muted", "data-testid": "tokens-empty" }, "No personal API tokens.")); return; }
    listEl.replaceChildren(...toks.map((t) => el("div", { className: "card job-row", "data-testid": "token-row", "data-token-id": t.id },
      el("span", { className: "badge", "data-testid": "token-prefix" }, t.prefix || "glue_pat_"),
      el("div", { className: "job-objective", "data-testid": "token-name" }, t.name || t.id),
      el("button", { className: "btn btn-danger", type: "button", "data-testid": "token-revoke-btn", onclick: async () => { await apiDelete(`/v1/tokens/${encodeURIComponent(t.id)}`); await load(); } }, "Revoke")
    )));
  }
  await load();
}

async function renderJobsList(root) {
  stopBackground();
  const listEl = el("div", { className: "jobs-list", "data-testid": "jobs-list" });
  const select = el("select", {
    className: "select", "data-testid": "status-filter",
    onchange: (e) => { statusFilter = e.target.value; load(); },
  }, STATUSES.map((st) => el("option", { value: st, ...(st === statusFilter ? { selected: "selected" } : {}) }, st || "All statuses")));
  const refreshBtn = el("button", { className: "btn", type: "button", "data-testid": "refresh-jobs", onclick: () => load() }, "Refresh");
  const tokensLink = el("a", { className: "btn btn-secondary", href: "#/tokens", "data-testid": "tokens-link" }, "Tokens");
  const logoutBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "logout-btn", onclick: async () => { await logout(); renderLogin(); } }, "Sign out");
  root.replaceChildren(el("section", { "data-testid": "jobs-view" },
    el("div", { className: "toolbar" }, el("h2", {}, "Jobs"), el("div", { className: "toolbar-controls" }, select, refreshBtn, tokensLink, logoutBtn, el("span", { className: "muted", "data-testid": "auto-refresh-label" }, "Auto-refresh: 10s"))),
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
        const pendingCount = Number(j.pending_permission_count || 0);
        return el("a", { className: "card job-row", href: `#/jobs/${encodeURIComponent(j.id)}`, "data-testid": "job-row", "data-job-id": j.id },
          el("span", { className: `badge status-${j.status}`, "data-testid": "job-status" }, formatStatusLabel(j.status)),
          el("div", {},
            el("div", { className: "job-objective", "data-testid": "job-objective", title: j.spec?.objective || "" }, truncate(j.spec?.objective || j.id, 80)),
            el("div", { className: "job-meta" }, el("span", { "data-testid": "job-repo" }, formatRepo(j.spec)), el("span", { "data-testid": "job-model" }, formatModel(j.spec)))
          ),
          el("div", { className: "job-stats" },
            pendingCount > 0 ? el("span", { className: "badge status-waiting_for_input", "data-testid": "job-pending-permissions" }, `${pendingCount} pending`) : null,
            // Waiting with no pending permission means the agent asked a question.
            j.status === "waiting_for_input" && pendingCount === 0 ? el("span", { className: "badge status-waiting_for_input", "data-testid": "job-question" }, "question") : null,
            el("span", { "data-testid": "job-iteration" }, `iter ${j.iteration ?? 0}/${maxIter}`),
            el("time", { "data-testid": "job-updated", datetime: j.updated_at || "" }, formatTime(j.updated_at))
          )
        );
      }));
    } catch (err) {
      if (getCSRFToken()) listEl.replaceChildren(el("p", { className: "error-text" }, err.message));
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

export function setPermissionCardState(state, permId, nextState) {
  const entry = state.permissionCards.get(permId);
  if (!entry) return;
  entry.stateBadge.className = `badge status-${nextState === "pending" ? "waiting_for_input" : nextState}`;
  entry.stateBadge.textContent = nextState;
  if (entry.controlsEl) entry.controlsEl.hidden = nextState !== "pending";
  state.syncPermissions?.();
}

export function upsertPermissionCard(state, permId, req = {}, permState = "pending", seq = 0) {
  if (state.permissionCards.has(permId)) {
    const existing = state.permissionCards.get(permId);
    if (seq > 0 && Number(existing.card.getAttribute("data-seq") || 0) === 0) {
      existing.card.setAttribute("data-seq", String(seq));
      state.transcriptEl.appendChild(existing.card);
    }
    setPermissionCardState(state, permId, permState);
    return existing;
  }
  const stateBadge = el("span", { className: `badge status-${permState === "pending" ? "waiting_for_input" : permState}`, "data-testid": "permission-state" }, permState);
  const toolLabel = req.action && req.action !== req.tool ? `${req.tool || "tool"} (${req.action})` : (req.tool || req.action || "tool");
  const reasonInput = el("input", { className: "input permission-reason", type: "text", placeholder: "Reason (optional, used when denying)", "data-testid": "permission-reason" });
  const errEl = el("p", { className: "error-text", "data-testid": "permission-error", hidden: true });
  const allowOnceBtn = el("button", { className: "btn", type: "button", "data-testid": "permission-allow-once", onclick: () => decide({ allow: true }, "allowed") }, "Allow once");
  const allowSessionBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "permission-allow-session", onclick: () => decide({ allow: true, remember_for: "session" }, "allowed") }, "Allow for session");
  const denyBtn = el("button", { className: "btn btn-danger", type: "button", "data-testid": "permission-deny", onclick: () => decide({ allow: false, reason: reasonInput.value.trim() || "permission denied by user" }, "denied") }, "Deny");
  const controlsEl = el("div", { className: "permission-controls", "data-testid": "permission-controls", hidden: permState !== "pending" }, reasonInput, el("div", { className: "permission-buttons" }, allowOnceBtn, allowSessionBtn, denyBtn), errEl);
  async function decide(payload, nextState) {
    if (!state.jobId) return;
    errEl.hidden = true;
    allowOnceBtn.disabled = allowSessionBtn.disabled = denyBtn.disabled = reasonInput.disabled = true;
    try {
      await apiPost(`/v1/jobs/${encodeURIComponent(state.jobId)}/permissions/${encodeURIComponent(permId)}/decision`, payload);
      setPermissionCardState(state, permId, nextState);
      const anyPending = [...state.permissionCards.values()].some((c) => c.stateBadge.textContent === "pending");
      state.updateStatus?.(anyPending ? "waiting_for_input" : "running");
      state.onAction?.();
    } catch (err) {
      errEl.textContent = err.message || "Decision failed";
      errEl.hidden = false;
      allowOnceBtn.disabled = allowSessionBtn.disabled = denyBtn.disabled = reasonInput.disabled = false;
    }
  }
  const card = el("div", { className: "transcript-item permission-card", "data-testid": "permission-card", "data-permission-id": permId, "data-seq": seq },
    el("div", { className: "permission-header" }, el("span", { "data-testid": "permission-tool" }, `Permission: ${toolLabel}`), stateBadge),
    req.target ? el("div", { className: "muted", "data-testid": "permission-target" }, `Target: ${req.target}`) : null,
    req.args ? el("pre", { className: "pre-wrap", "data-testid": "permission-args" }, formatJSON(req.args)) : null,
    controlsEl
  );
  state.transcriptEl.appendChild(card);
  const entry = { card, stateBadge, controlsEl, toolLabel, target: req.target || "" };
  state.permissionCards.set(permId, entry);
  state.syncPermissions?.();
  return entry;
}

// renderQuestionCard shows the agent's open question with its options and an
// answer box that posts to /v1/jobs/{id}/answers.
export function renderQuestionCard(jobId, q, onAnswered) {
  const input = el("input", { className: "input question-input", type: "text", placeholder: "Your answer", "data-testid": "question-input" });
  const errEl = el("p", { className: "error-text", "data-testid": "question-error", hidden: true });
  const submit = el("button", { className: "btn", type: "submit", "data-testid": "question-submit" }, "Answer");
  async function answer(text) {
    if (!text) return;
    errEl.hidden = true;
    submit.disabled = input.disabled = true;
    try {
      await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/answers`, { question_id: q.id, text });
      onAnswered?.(text);
    } catch (err) {
      errEl.textContent = err.message || "Answer failed";
      errEl.hidden = false;
      submit.disabled = input.disabled = false;
    }
  }
  const options = (q.options || []).map((o) => el("button", { className: "btn btn-secondary", type: "button", "data-testid": "question-option", onclick: () => answer(o) }, o));
  return el("div", { className: "card question-card", "data-testid": "question-card", "data-question-id": q.id || "" },
    el("h3", {}, "The agent is asking"),
    el("p", { className: "pre-wrap", "data-testid": "question-text" }, q.question || ""),
    options.length ? el("div", { className: "permission-buttons" }, ...options) : null,
    el("form", { className: "steer-form", onsubmit: (e) => { e.preventDefault(); answer(input.value.trim()); } }, input, submit),
    errEl
  );
}

export function applyJobEvent(state, env) {
  const seq = Number(env?.seq || 0);
  if (seq > 0) {
    if (state.seenSeqs.has(seq)) return false;
    state.seenSeqs.add(seq);
    if (seq > state.lastSeq) state.lastSeq = seq;
    if (state.transcriptEl?.dataset) {
      state.transcriptEl.dataset.seenCount = String(state.seenSeqs.size);
      state.transcriptEl.dataset.lastSeq = String(state.lastSeq);
    }
  }
  const p = env?.payload || {};
  if (env.type === "question") state.onQuestion?.({ id: p.question_id, question: p.question, options: p.options });
  if (env.type === "answer" || env.type === "question_timeout") state.onQuestionClosed?.(p.question_id);
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
    state.updateStatus?.("waiting_for_input");
    upsertPermissionCard(state, permId, p.request || {}, "pending", seq);
    return true;
  }

  if (env.type === "permission_decision" || env.type === "permission_expired") {
    const nextState = env.type === "permission_expired" ? "expired" : p.decision?.allow ? "allowed" : "denied";
    setPermissionCardState(state, p.permission_id || "", nextState);
    if (p.original_permission_id) setPermissionCardState(state, p.original_permission_id, nextState);
    const anyPending = [...state.permissionCards.values()].some((c) => c.stateBadge.textContent === "pending");
    state.updateStatus?.(anyPending ? "waiting_for_input" : "running");
    return true;
  }

  const checklist = p.Checklist ?? p.checklist ?? p.result?.Checklist ?? p.result?.checklist;
  const iter = p.Iteration ?? p.iteration ?? p.from_iteration ?? p.result?.Iterations ?? p.result?.iterations ?? env.iteration;
  const usage = p.Usage ?? p.usage ?? p.result?.Usage ?? p.result?.usage;
  if (Array.isArray(checklist)) state.updateChecklist?.(checklist);
  if (typeof iter === "number" && iter > 0) state.updateIteration?.(iter);
  if (usage && typeof usage === "object") state.updateUsage?.(usage);

  const dividers = {
    plan: () => `Plan (${(checklist || []).length} items)`,
    iteration_start: () => `Iteration ${iter || 1} started`,
    verdict: () => `Verdict: ${(checklist || []).filter((i) => i.done).length}/${(checklist || []).length} ✓ — ${p.Message ?? p.message ?? ""}`,
    operator_message: () => `Operator: ${p.text || ""}`,
    run_interrupted: () => `Interrupted: ${p.reason || p.message || "run interrupted"}`,
    job_resumed: () => {
      state.finished = false;
      state.updateStatus?.("running");
      return `Resumed at iteration ${p.from_iteration || iter || 1} (${p.reason || "resumed"})`;
    },
    task_suspended: () => {
      if (p.reason === "waiting_for_input") state.updateStatus?.("suspended");
      return `Task suspended (${p.reason || "idle"})`;
    },
    task_resumed: () => {
      state.finished = false;
      state.updateStatus?.("running");
      return `Task resumed (${p.reason || "on_demand"})`;
    },
    job_done: () => {
      const st = p.status || "achieved";
      const summary = p.result?.Summary ?? p.result?.summary ?? p.error ?? "";
      for (const [id, c] of state.permissionCards.entries()) {
        if (c.stateBadge.textContent === "pending") setPermissionCardState(state, id, "expired");
      }
      state.finished = true;
      state.updateStatus?.(st);
      state.onJobDone?.(st);
      return `Job ${st}${summary ? " — " + summary : ""}`;
    },
  };
  if (dividers[env.type]) {
    state.transcriptEl.appendChild(el("div", { className: "transcript-item goal-divider", "data-testid": "goal-divider", "data-event-type": env.type, "data-seq": seq }, dividers[env.type]()));
  }
  return true;
}

export function renderResultPanel(panelEl, res) {
  const cost = formatCostUSD(res.usage, res.estimated_cost_usd ?? res.pricing);
  const verifyItems = (res.verify || []).map((v) => {
    const ok = Number(v.exit_code) === 0;
    return el("details", { className: "verify-item", "data-testid": "verify-item", ...(ok ? {} : { open: "open" }) },
      el("summary", { className: "tool-summary", "data-testid": "verify-summary" },
        el("span", { "data-testid": "verify-command" }, (v.argv || []).join(" ") || "(command)"),
        el("span", {}, el("span", { className: `badge ${ok ? "status-achieved" : "status-errored"}`, "data-testid": "verify-exit-code" }, `exit ${v.exit_code}`), el("span", { className: "muted" }, ` ${v.duration_ms ?? 0}ms`))
      ),
      el("pre", { className: "pre-wrap tool-output", "data-testid": "verify-output" }, v.output_tail || "(no output)")
    );
  });
  panelEl.hidden = false;
  panelEl.replaceChildren(
    el("div", { className: "detail-header-top" },
      el("h3", { className: "detail-objective" }, "Result"),
      el("span", { className: `badge status-${res.status}`, "data-testid": "result-status" }, res.status || "done"),
      res.pr_url ? el("a", { className: "btn-link", href: res.pr_url, target: "_blank", rel: "noopener noreferrer", "data-testid": "result-pr-link" }, `Pull Request: ${res.pr_url}`) : null
    ),
    res.summary ? el("p", { "data-testid": "result-summary" }, res.summary) : null,
    res.delivery_error ? el("p", { className: "error-text", "data-testid": "result-delivery-error" }, `Delivery error: ${res.delivery_error}`) : null,
    el("div", { className: "detail-meta result-meta" },
      el("span", { "data-testid": "result-branch" }, `Branch: ${res.branch || "—"}`),
      el("span", { "data-testid": "result-commits", title: `${res.base_sha || ""} -> ${res.head_sha || ""}` }, `Commits: ${shortSHA(res.base_sha)} → ${shortSHA(res.head_sha)}`),
      el("span", { "data-testid": "result-iterations" }, `Iterations: ${res.iterations ?? 0}`),
      el("span", { "data-testid": "result-usage" }, formatUsage(res.usage)),
      cost ? el("span", { "data-testid": "result-cost" }, `Est. cost: ${cost}`) : null
    ),
    res.diffstat ? el("div", { className: "result-section" }, el("div", { className: "tool-label" }, "Diffstat"), el("pre", { className: "pre-wrap", "data-testid": "result-diffstat" }, res.diffstat)) : null,
    verifyItems.length ? el("div", { className: "result-section", "data-testid": "result-verify-list" }, el("div", { className: "tool-label" }, "Verification Commands"), ...verifyItems) : null
  );
}

async function renderJobDetail(root, jobId) {
  stopBackground();
  const [detail, pendingResp] = await Promise.all([
    apiRequest(`/v1/jobs/${encodeURIComponent(jobId)}`),
    apiRequest(`/v1/jobs/${encodeURIComponent(jobId)}/permissions?state=pending`).catch(() => ({ permissions: [] })),
  ]);
  let currentStatus = detail.status;
  const maxIter = detail.spec?.budgets?.max_iterations || 10;
  const statusEl = el("span", { className: `badge status-${detail.status}`, "data-testid": "detail-status" }, formatStatusLabel(detail.status));
  const suspendedNoteEl = el("p", { className: "muted suspended-note", "data-testid": "suspended-note", hidden: detail.status !== "suspended" }, "Suspended – waiting for you. The first action may take a few seconds while the task resumes.");
  const iterEl = el("span", { "data-testid": "detail-iteration" }, `iter ${detail.iteration ?? 0}/${maxIter}`);
  const usageEl = el("span", { "data-testid": "detail-usage" }, formatUsage(detail.usage));
  const streamEl = el("span", { className: "muted", "data-testid": "stream-status" }, "connecting…");
  const bannerEl = el("div", { className: "card pending-banner", "data-testid": "pending-permissions-banner", hidden: true });
  const questionHostEl = el("div", { "data-testid": "question-host" });
  const resultPanelEl = el("div", { className: "card", "data-testid": "job-result-panel", hidden: true });
  const checklistEl = el("ul", { className: "checklist", "data-testid": "job-checklist" });
  const transcriptEl = el("div", { className: "transcript", "data-testid": "job-transcript" });

  const cancelErrEl = el("p", { className: "error-text", "data-testid": "cancel-error", hidden: true });
  const confirmCancelBtn = el("button", { className: "btn btn-danger", type: "button", "data-testid": "cancel-confirm-btn", onclick: async () => {
    cancelErrEl.hidden = true;
    confirmCancelBtn.disabled = dismissCancelBtn.disabled = true;
    try { await apiDelete(`/v1/jobs/${encodeURIComponent(jobId)}`); cancelDialogEl.hidden = true; }
    catch (err) { cancelErrEl.textContent = err.message || "Cancel failed"; cancelErrEl.hidden = false; }
    finally { confirmCancelBtn.disabled = dismissCancelBtn.disabled = false; }
  } }, "Confirm cancel");
  const dismissCancelBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "cancel-dismiss-btn", onclick: () => { cancelDialogEl.hidden = true; } }, "Keep running");
  const cancelDialogEl = el("div", { className: "cancel-confirm", "data-testid": "cancel-confirm-dialog", hidden: true }, el("span", {}, "Cancel this job?"), confirmCancelBtn, dismissCancelBtn, cancelErrEl);
  const cancelBtn = el("button", { className: "btn btn-danger", type: "button", "data-testid": "cancel-job-btn", hidden: !isActiveStatus(detail.status), onclick: () => { cancelDialogEl.hidden = false; cancelErrEl.hidden = true; } }, "Cancel job");

  const steerHeadingEl = el("h3", { "data-testid": "steer-heading" }, "Send message");
  const steerInputEl = el("input", { className: "input steer-input", type: "text", required: "true", "data-testid": "steer-input" });
  const steerSubmitBtn = el("button", { className: "btn", type: "submit", "data-testid": "steer-submit" }, "Send message");
  const steerErrEl = el("p", { className: "error-text", "data-testid": "steer-error", hidden: true });
  const steerCardEl = el("div", { className: "card", "data-testid": "steer-card" },
    steerHeadingEl,
    el("form", { className: "steer-form", "data-testid": "steer-form", onsubmit: async (e) => {
      e.preventDefault();
      const text = steerInputEl.value.trim();
      if (!text) return;
      steerErrEl.hidden = true;
      steerSubmitBtn.disabled = steerInputEl.disabled = true;
      try {
        if (isActiveStatus(currentStatus)) {
          const wasSuspended = currentStatus === "suspended";
          await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/messages`, { text });
          steerInputEl.value = "";
          if (wasSuspended) { state.updateStatus("running"); openSSE(true, true); }
        } else {
          await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/continue`, { text });
          steerInputEl.value = "";
          state.finished = false;
          resultPanelEl.hidden = true;
          state.updateStatus("running");
          openSSE(true, true);
        }
      } catch (err) {
        steerErrEl.textContent = err.message || "Request failed";
        steerErrEl.hidden = false;
      } finally {
        steerSubmitBtn.disabled = steerInputEl.disabled = false;
      }
    } }, steerInputEl, steerSubmitBtn),
    steerErrEl
  );

  function syncControls(st) {
    currentStatus = st;
    const active = isActiveStatus(st);
    suspendedNoteEl.hidden = st !== "suspended";
    cancelBtn.hidden = !active;
    if (!active) cancelDialogEl.hidden = true;
    steerCardEl.hidden = st === "achieved";
    const label = active ? "Send message" : "Continue with instructions";
    steerHeadingEl.textContent = label;
    steerSubmitBtn.textContent = label;
    steerInputEl.placeholder = active ? "Send guidance to the next iteration…" : "Instructions to continue this job…";
  }

  const loadResult = () => apiRequest(`/v1/jobs/${encodeURIComponent(jobId)}/result`).then((res) => renderResultPanel(resultPanelEl, res)).catch(() => {});

  const state = createTranscriptState({
    jobId,
    transcriptEl,
    syncPermissions: () => {
      const pending = [...state.permissionCards.values()].filter((c) => c.stateBadge.textContent === "pending");
      if (!pending.length) { bannerEl.hidden = true; bannerEl.replaceChildren(); return; }
      bannerEl.hidden = false;
      bannerEl.replaceChildren(
        el("strong", { "data-testid": "pending-permissions-count" }, `${pending.length} permission ${pending.length === 1 ? "request" : "requests"} waiting for approval`),
        el("span", { className: "muted" }, " — " + pending.map((p) => `${p.toolLabel}${p.target ? " (" + p.target + ")" : ""}`).join(", "))
      );
    },
    updateStatus: (st) => { statusEl.className = `badge status-${st}`; statusEl.textContent = formatStatusLabel(st); syncControls(st); },
    onAction: () => { openSSE(true, true); },
    onJobDone: () => { loadResult(); },
    updateIteration: (n) => { iterEl.textContent = `iter ${n}/${maxIter}`; },
    updateUsage: (u) => { usageEl.textContent = formatUsage(u); },
    updateChecklist: (items) => {
      if (!items?.length) { checklistEl.replaceChildren(el("li", { className: "muted" }, "No checklist items yet.")); return; }
      checklistEl.replaceChildren(...items.map((item) =>
        el("li", { className: `checklist-item ${item.done ? "checklist-done" : ""}`, "data-testid": "checklist-item", "data-done": String(Boolean(item.done)) },
          el("span", { className: "checklist-mark", "data-testid": "checklist-mark" }, item.done ? "[x]" : "[ ]"),
          el("div", {}, el("span", { "data-testid": "checklist-title" }, item.title || ""), item.evidence ? el("span", { className: "checklist-evidence" }, item.evidence) : null)
        )
      ));
    },
  });
  state.updateChecklist(detail.checklist || []);
  syncControls(detail.status);
  for (const perm of pendingResp.permissions || []) upsertPermissionCard(state, perm.id, perm.request || {}, perm.state || "pending", 0);
  let openQuestionId = "";
  state.onQuestion = (q) => {
    if (!q?.id || q.id === openQuestionId) return;
    openQuestionId = q.id;
    questionHostEl.replaceChildren(renderQuestionCard(jobId, q, () => { openQuestionId = ""; questionHostEl.replaceChildren(); state.updateStatus?.("running"); }));
  };
  state.onQuestionClosed = (id) => { if (id === openQuestionId) { openQuestionId = ""; questionHostEl.replaceChildren(); } };
  if (detail.question) state.onQuestion(detail.question);
  if (!isActiveStatus(detail.status)) await loadResult();

  root.replaceChildren(el("section", { "data-testid": "job-detail-view" },
    el("a", { className: "back-link", href: "#/", "data-testid": "back-to-jobs" }, "← All jobs"),
    bannerEl,
    questionHostEl,
    el("div", { className: "card", "data-testid": "job-header" },
      el("div", { className: "detail-header-top" }, statusEl, el("h2", { className: "detail-objective", "data-testid": "detail-objective" }, detail.spec?.objective || jobId), cancelBtn),
      suspendedNoteEl,
      cancelDialogEl,
      el("div", { className: "detail-meta" }, iterEl, el("span", { "data-testid": "detail-model" }, formatModel(detail.spec)), el("span", { "data-testid": "detail-repo" }, formatRepo(detail.spec)), usageEl, streamEl)
    ),
    resultPanelEl,
    steerCardEl,
    el("div", { className: "detail-grid" },
      el("div", { className: "card" }, el("h3", {}, "Checklist"), checklistEl),
      el("div", { className: "card" }, el("h3", {}, "Transcript"), transcriptEl)
    )
  ));

  let es = null;
  let retryTimer = null;
  function openSSE(useAfter = false, forceLive = false) {
    if (state.finished) return;
    if (es) es.close();
    const params = new URLSearchParams();
    if (useAfter && state.lastSeq > 0) params.set("after", String(state.lastSeq));
    if (!forceLive && currentStatus === "suspended") params.set("cached", "1");
    const q = params.toString();
    es = new EventSource(`/v1/jobs/${encodeURIComponent(jobId)}/events${q ? "?" + q : ""}`);
    es.onopen = () => { streamEl.textContent = currentStatus === "suspended" && !forceLive ? "cached" : "live"; };
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
      if (state.finished || (currentStatus === "suspended" && !forceLive)) { es.close(); return; }
      streamEl.textContent = "reconnecting…";
      if (es.readyState === EventSource.CLOSED && !retryTimer) {
        retryTimer = setTimeout(() => { retryTimer = null; openSSE(true, forceLive); }, 1000);
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
  const session = getCSRFToken() ? { authenticated: true } : await fetchSession();
  if (!session) { renderLogin(); return; }
  setSessionBanner("Authenticated");
  if (window.location.hash === "#/tokens") {
    await renderTokensView(root);
    return;
  }
  const match = (window.location.hash || "#/").match(/^#\/jobs\/([^/?#]+)/);
  if (match) {
    await renderJobDetail(root, decodeURIComponent(match[1])).catch((err) => {
      if (getCSRFToken()) root.replaceChildren(el("p", { className: "error-text" }, err.message));
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
