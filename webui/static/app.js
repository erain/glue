import { fetchSession, login, logout, getCSRFToken, getAuthMode, setUnauthorizedHandler, apiRequest, apiText, apiPost, apiDelete } from "./api.js";
export { fetchSession, login, logout, getCSRFToken, getAuthMode, apiRequest, apiText, apiPost, apiDelete };

const TRUNCATE_LIMIT = 4096;
const REFRESH_MS = 10000;
const STATUSES = ["", "created", "running", "waiting_for_input", "suspended", "achieved", "blocked", "budget_limited", "max_iterations", "errored", "canceled", "interrupted"];
const SSE_EVENT_TYPES = [
  "message", "job_created", "job_started", "job_waiting", "job_resumed", "job_done", "mode_promoted", "task_suspended", "task_resumed",
  "plan", "iteration_start", "maker_done", "verdict", "done", "blocked", "budget_limited", "max_iterations",
  "operator_message", "operator_message_delivered", "run_interrupted",
  "question", "answer", "question_timeout", "question_assumed", "turn_interrupted",
  "permission_request", "permission_decision", "permission_expired", "permission_auto",
  "loop_start", "loop_end", "turn_start", "turn_end", "turn_complete", "message_start", "message_end",
  "text_delta", "tool_start", "tool_end", "error", "retry", "guardrail", "auto_continue",
];
const GOAL_PHASES = new Set(["planner", "maker", "checker"]);

let listTimer = null;
let activeStream = null;
let statusFilter = "";

const isActiveStatus = (st) => st === "created" || st === "running" || st === "waiting_for_input" || st === "suspended";
const formatStatusLabel = (st) => (st === "suspended" ? "Suspended – waiting for you" : st);
export function formatWorkspaceStatus(st, reason, mode, waking = false) {
  if (waking) return "Waking workspace…";
  if (st === "waiting_for_input" && reason === "awaiting_turn") return "Ready for your message";
  if (st === "suspended" && (reason === "awaiting_turn" || (mode === "interactive" && (!reason || reason === "waiting_for_input")))) return "Suspended (wakes on message)";
  return "";
}
const makePhaseBadge = (phase) => (GOAL_PHASES.has(phase) ? el("span", { className: `badge phase-badge phase-${phase}`, "data-testid": "phase-badge", "data-phase": phase }, phase) : null);
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

let sessionLogin = "";
function setSessionBanner(text) {
  const s = document.getElementById("session-status");
  if (s) s.textContent = text;
}

function renderLogin() {
  stopBackground();
  sessionLogin = "";
  setSessionBanner("Sign in required");
  const root = document.getElementById("app");
  if (!root) return;
  if (getAuthMode() === "cloudflare-access") {
    root.replaceChildren(el("section", { className: "card login-card", "data-testid": "login-view" }, el("h2", {}, "Cloudflare Access Required"), el("p", { className: "muted", "data-testid": "cf-access-notice" }, "Sign in through Cloudflare Access at your gateway public URL to open this workspace.")));
    return;
  }
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
  const newJobBtn = el("a", { className: "btn", href: "#/new", "data-testid": "new-job-btn" }, "New job");
  const refreshBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "refresh-jobs", onclick: () => load() }, "Refresh");
  const tokensLink = el("a", { className: "btn btn-secondary", href: "#/tokens", "data-testid": "tokens-link" }, "Tokens");
  const logoutBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "logout-btn", onclick: async () => { await logout(); renderLogin(); } }, "Sign out");
  root.replaceChildren(el("section", { "data-testid": "jobs-view" },
    el("div", { className: "toolbar" }, el("h2", {}, "Jobs"), el("div", { className: "toolbar-controls" }, newJobBtn, select, refreshBtn, tokensLink, logoutBtn, el("span", { className: "muted", "data-testid": "auto-refresh-label" }, "Auto-refresh: 10s"))),
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
            j.status === "waiting_for_input" && pendingCount === 0 && j.status_reason !== "awaiting_turn" ? el("span", { className: "badge status-waiting_for_input", "data-testid": "job-question" }, "question") : null,
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

export function formatToolSummary(name, rawArgs) {
  let args = rawArgs, arg = "";
  if (typeof rawArgs === "string") { try { args = JSON.parse(rawArgs); } catch { args = null; } }
  if (args && typeof args === "object") {
    if (Array.isArray(args.argv) && args.argv.length) arg = args.argv.map(String).join(" ");
    else for (const k of ["path", "pattern", "query", "command", "url", "name", "question"]) if (typeof args[k] === "string" && args[k]) { arg = args[k]; break; }
  }
  arg = truncate(arg.replace(/\s+/g, " ").trim(), 90);
  return arg ? `${name} ${arg}` : name;
}

export const formatActivity = (a) => [a?.phase, a?.model_turns > 0 ? `turn ${a.model_turns}` : "", a?.last_tool].filter(Boolean).join(" · ");

export function parseUnifiedDiff(raw) {
  const files = [];
  let cur = null, inHunk = false, inBin = false;
  for (const line of String(raw || "").replace(/\r\n/g, "\n").replace(/\n$/, "").split("\n")) {
    if (line.startsWith("diff --git ")) {
      const m = line.match(/^diff --git a\/(.+?) b\/(.+)$/);
      files.push(cur = { path: m ? m[2] : line.slice(11), additions: 0, deletions: 0, binary: false, lines: [] });
      inHunk = inBin = false;
    } else if (!cur) continue;
    else if (!inHunk && line.startsWith("--- a/") && cur.path === "/dev/null") cur.path = line.slice(6);
    else if (!inHunk && line.startsWith("+++ b/")) cur.path = line.slice(6);
    else if (!inHunk && (line.startsWith("+++ /dev/null") || line.startsWith("--- ") || line.startsWith("index "))) continue;
    else if (!inHunk && (line.startsWith("GIT binary patch") || line.startsWith("Binary files "))) cur.binary = inBin = true;
    else if (inBin) continue;
    else if (line.startsWith("@@")) { inHunk = true; cur.lines.push({ type: "hunk", text: line }); }
    else if (inHunk && line.startsWith("+")) { cur.additions++; cur.lines.push({ type: "add", text: line }); }
    else if (inHunk && line.startsWith("-")) { cur.deletions++; cur.lines.push({ type: "del", text: line }); }
    else if (inHunk) cur.lines.push({ type: line.startsWith("\\") ? "meta" : "ctx", text: line });
    else if (line) cur.lines.push({ type: "meta", text: line });
  }
  return files;
}

export function renderDiffFiles(bodyEl, summaryEl, raw) {
  const files = parseUnifiedDiff(raw);
  if (!files.length) {
    if (summaryEl) summaryEl.textContent = "0 files";
    bodyEl.replaceChildren(el("p", { className: "muted", "data-testid": "diff-empty" }, "No workspace changes yet."));
    return;
  }
  const addTot = files.reduce((n, f) => n + f.additions, 0), delTot = files.reduce((n, f) => n + f.deletions, 0);
  if (summaryEl) summaryEl.textContent = `${files.length} ${files.length === 1 ? "file" : "files"} · +${addTot} -${delTot}`;
  const sections = files.map((f, idx) => el("details", { className: "diff-file", "data-testid": "diff-file", "data-file-path": f.path, ...(idx < 10 ? { open: "open" } : {}) },
    el("summary", { className: "tool-summary diff-file-header", "data-testid": "diff-file-summary" }, el("span", { "data-testid": "diff-file-path" }, f.path), el("span", { className: "diff-counts" }, el("span", { className: "diff-stat-add" }, `+${f.additions}`), " ", el("span", { className: "diff-stat-del" }, `-${f.deletions}`))),
    el("div", { className: "diff-hunks", "data-testid": "diff-hunks" }, f.binary && !f.lines.length ? el("div", { className: "diff-line diff-meta" }, "Binary file changed") : f.lines.map((ln) => el("div", { className: `diff-line diff-${ln.type}`, "data-testid": `diff-line-${ln.type}` }, ln.text)))
  ));
  bodyEl.replaceChildren(el("div", { className: "diff-nav", "data-testid": "diff-file-nav" }, ...files.map((f, idx) => el("button", { className: "btn-link diff-chip", type: "button", "data-testid": "diff-file-chip", "data-file-path": f.path, onclick: () => { sections[idx].open = true; sections[idx].scrollIntoView({ block: "nearest" }); } }, `${f.path} (+${f.additions} -${f.deletions})`))), ...sections);
}

function renderInlineNodes(str) {
  const nodes = [], re = /`([^`\n]+)`/g;
  let last = 0, m;
  while ((m = re.exec(str))) {
    if (m.index > last) nodes.push(str.slice(last, m.index));
    nodes.push(el("code", { className: "inline-code", "data-testid": "inline-code" }, m[1]));
    last = re.lastIndex;
  }
  if (last < str.length) nodes.push(str.slice(last));
  return nodes;
}

export function renderFormattedText(container, text) {
  const src = String(text || "").replace(/\r\n/g, "\n"), out = [], re = /```([^\n`]*)\n([\s\S]*?)(?:```|$)/g;
  let last = 0, m;
  const pushProse = (s) => {
    const trimmed = s.replace(/^\n+|\n+$/g, "");
    if (trimmed) out.push(el("p", { className: "assistant-para" }, ...renderInlineNodes(trimmed)));
  };
  while ((m = re.exec(src))) {
    if (m.index > last) pushProse(src.slice(last, m.index));
    const lang = m[1].trim();
    out.push(el("pre", { className: "pre-wrap code-block", "data-testid": "code-block", ...(lang ? { "data-lang": lang } : {}) }, el("code", {}, m[2].replace(/\n$/, ""))));
    last = re.lastIndex;
  }
  if (last < src.length) pushProse(src.slice(last));
  container.replaceChildren(...out);
}

export function createTranscriptState(refs) {
  return { activity: { phase: "", model_turns: 0, last_tool: "" }, initialLastSeq: 0, statusFloorSeq: 0, inChat: false, ...refs, seenSeqs: new Set(), lastSeq: 0, finished: false, currentText: null, currentTextKey: "", toolCards: new Map(), lastTool: null, permissionCards: new Map() };
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
  const p = env?.payload || {}, isLive = !seq || seq > (state.statusFloorSeq || 0);
  const emitStatus = (st, reason = "") => { if (isLive) state.updateStatus?.(st, reason); };
  if (seq === 1) { state.activity = { phase: "", model_turns: 0, last_tool: "" }; state.initialLastSeq = 0; }
  if (env.phase) state.activity.phase = env.phase;
  if (env.type === "turn_end" && (!seq || seq > (state.initialLastSeq || 0))) state.activity.model_turns++;
  if (env.phase || env.type === "turn_end") state.updateActivity?.(state.activity);
  if (env.type === "question") state.onQuestion?.({ id: p.question_id, question: p.question, options: p.options });
  if (env.type === "answer" || env.type === "question_timeout") state.onQuestionClosed?.(p.question_id);
  if (env.type === "job_created" && p.mode === "interactive") {
    state.inChat = true;
    if (p.objective) state.transcriptEl.appendChild(el("div", { className: "transcript-item user-turn", "data-testid": "user-turn", "data-event-type": "job_created", "data-seq": seq }, el("div", { className: "turn-role" }, "You"), el("p", { className: "assistant-para", "data-testid": "user-turn-text" }, p.objective)));
    return true;
  }
  if (env.type === "job_started") { emitStatus("running", ""); return true; }
  if (env.type === "text_delta" || env.type === "turn_complete") {
    const delta = env.type === "text_delta" ? (p.delta || "") : (p.text || "");
    if (!delta || (env.phase && env.phase !== "maker" && env.phase !== "chat")) return true;
    const key = `${env.phase || "maker"}:${env.iteration ?? ""}:${env.session_id || ""}`;
    if (!state.currentText || state.currentTextKey !== key) {
      const bodyEl = el("div", { className: "assistant-text", "data-testid": "assistant-text" });
      const head = env.phase === "chat" || state.inChat ? el("div", { className: "turn-role" }, "Assistant") : makePhaseBadge(env.phase);
      state.transcriptEl.appendChild(el("div", { className: "transcript-item assistant-message", "data-testid": "transcript-item", "data-event-type": env.type, "data-seq": seq }, head, bodyEl));
      state.currentText = { bodyEl, raw: "" };
      state.currentTextKey = key;
    }
    state.currentText.raw = env.type === "turn_complete" && !state.currentText.raw ? delta : state.currentText.raw + (env.type === "text_delta" ? delta : "");
    renderFormattedText(state.currentText.bodyEl, state.currentText.raw);
    if (env.type === "turn_complete") state.currentText = null;
    return true;
  }
  state.currentText = null;

  if (env.type === "tool_start") {
    const call = p.tool_call || {};
    const name = p.tool_name || call.name || "tool";
    const id = p.tool_call_id || call.id || `${name}-${seq}`;
    state.activity.last_tool = name;
    state.updateActivity?.(state.activity);
    const nameEl = el("span", { "data-testid": "tool-name" }, formatToolSummary(name, call.arguments));
    const durEl = el("span", { className: "muted", "data-testid": "tool-duration" });
    const statusEl = el("span", { className: "badge status-running", "data-testid": "tool-status" }, "running");
    const resultEl = el("div", { className: "tool-result", "data-testid": "tool-result" });
    const details = el("details", { className: "transcript-item tool-call", "data-testid": "tool-call", "data-tool-name": name, "data-seq": seq },
      el("summary", { className: "tool-summary", "data-testid": "tool-summary" }, nameEl, el("span", { className: "tool-meta" }, makePhaseBadge(env.phase), durEl, statusEl)),
      el("div", { className: "tool-body" }, el("div", { className: "tool-label" }, "Arguments"), el("pre", { className: "pre-wrap tool-args", "data-testid": "tool-args" }, formatJSON(call.arguments) || "{}"), resultEl)
    );
    state.transcriptEl.appendChild(details);
    const entry = { details, nameEl, durEl, statusEl, resultEl, rawName: name, startedAt: env.time ? Date.parse(env.time) : NaN };
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
      if (call.arguments && card.nameEl.textContent === card.rawName) card.nameEl.textContent = formatToolSummary(card.rawName, call.arguments);
      const ms = typeof p.duration_ms === "number" ? p.duration_ms : (env.time && Number.isFinite(card.startedAt) ? Math.max(0, Date.parse(env.time) - card.startedAt) : NaN);
      if (Number.isFinite(ms)) card.durEl.textContent = `${(ms / 1000).toFixed(1)}s`;
      card.statusEl.className = `badge ${isErr ? "status-errored" : "status-achieved"}`;
      card.statusEl.textContent = isErr ? "✗" : "✓";
      if (isErr) card.details.open = true;
      renderTruncatedResult(card.resultEl, out);
    }
    return true;
  }

  if (env.type === "permission_request") {
    const permId = p.permission_id || `perm-${seq}`;
    emitStatus("waiting_for_input", "");
    upsertPermissionCard(state, permId, p.request || {}, "pending", seq);
    return true;
  }

  if (env.type === "permission_decision" || env.type === "permission_expired") {
    const nextState = env.type === "permission_expired" ? "expired" : p.decision?.allow ? "allowed" : "denied";
    setPermissionCardState(state, p.permission_id || "", nextState);
    if (p.original_permission_id) setPermissionCardState(state, p.original_permission_id, nextState);
    const anyPending = [...state.permissionCards.values()].some((c) => c.stateBadge.textContent === "pending");
    emitStatus(anyPending ? "waiting_for_input" : "running", "");
    return true;
  }

  const checklist = p.Checklist ?? p.checklist ?? p.result?.Checklist ?? p.result?.checklist;
  const iter = p.Iteration ?? p.iteration ?? p.from_iteration ?? p.result?.Iterations ?? p.result?.iterations ?? env.iteration;
  const usage = p.Usage ?? p.usage ?? p.result?.Usage ?? p.result?.usage;
  if (Array.isArray(checklist)) state.updateChecklist?.(checklist);
  if (typeof iter === "number" && iter > 0) state.updateIteration?.(iter);
  if (usage && typeof usage === "object") state.updateUsage?.(usage);

  if (env.type === "job_waiting" && p.reason === "awaiting_turn") { emitStatus("waiting_for_input", "awaiting_turn"); return true; }
  if (env.type === "operator_message" && (state.inChat || env.phase === "chat")) {
    if (env.phase === "chat") emitStatus("running", "");
    state.transcriptEl.appendChild(el("div", { className: "transcript-item user-turn", "data-testid": "user-turn", "data-event-type": "operator_message", "data-seq": seq }, el("div", { className: "turn-role" }, "You"), el("p", { className: "assistant-para", "data-testid": "user-turn-text" }, p.text || "")));
    return true;
  }

  const dividers = {
    plan: () => `Plan (${(checklist || []).length} items)`,
    iteration_start: () => `Iteration ${iter || 1} started`,
    verdict: () => `Verdict: ${(checklist || []).filter((i) => i.done).length}/${(checklist || []).length} ✓ — ${p.Message ?? p.message ?? ""}`,
    operator_message: () => `Operator: ${p.text || ""}`,
    turn_interrupted: () => "Interrupted current step — starting next turn",
    run_interrupted: () => `Interrupted: ${p.reason || p.message || "run interrupted"}`,
    job_resumed: () => {
      state.finished = false;
      emitStatus("running", "");
      return `Resumed at iteration ${p.from_iteration || iter || 1} (${p.reason || "resumed"})`;
    },
    task_suspended: () => {
      if (p.reason === "waiting_for_input") emitStatus("suspended", state.inChat ? "awaiting_turn" : "waiting_for_input");
      return `Task suspended (${p.reason || "idle"})`;
    },
    task_resumed: () => {
      state.finished = false;
      emitStatus("running", "");
      return `Task resumed (${p.reason || "on_demand"})`;
    },
    mode_promoted: () => { state.inChat = false; state.finished = false; emitStatus("running", ""); state.onPromoted?.(p.spec); return "Promoted to autonomous goal"; },
    job_done: () => {
      const st = p.status || "achieved";
      const summary = p.result?.Summary ?? p.result?.summary ?? p.error ?? "";
      for (const [id, c] of state.permissionCards.entries()) {
        if (c.stateBadge.textContent === "pending") setPermissionCardState(state, id, "expired");
      }
      state.finished = true;
      if (isLive) { state.updateStatus?.(st, ""); state.onJobDone?.(st); }
      return `Job ${st}${summary ? " — " + summary : ""}`;
    },
  };
  if (dividers[env.type]) {
    const divPhase = env.type === "plan" ? "planner" : env.type === "iteration_start" ? "maker" : env.type === "verdict" ? "checker" : "";
    state.transcriptEl.appendChild(el("div", { className: `transcript-item goal-divider${env.type === "operator_message" ? " user-turn" : ""}`, "data-testid": "goal-divider", "data-event-type": env.type, "data-seq": seq }, makePhaseBadge(divPhase), dividers[env.type]()));
  }
  return true;
}

export function renderResultPanel(panelEl, res, jobId) {
  const jid = res.job_id || jobId || "";
  const cost = formatCostUSD(res.usage, res.estimated_cost_usd ?? res.pricing);
  const prErrEl = el("p", { className: "error-text", "data-testid": "result-pr-error", hidden: true });
  const canOpenPR = !res.pr_url && Boolean(res.branch) && (!res.base_sha || !res.head_sha || res.head_sha !== res.base_sha);
  const prBtn = canOpenPR ? el("button", { className: "btn", type: "button", "data-testid": "result-open-pr-btn", onclick: async () => {
    prErrEl.hidden = prBtn.disabled = true; prBtn.textContent = "Opening PR…";
    try { renderResultPanel(panelEl, { ...res, ...(await apiPost(`/v1/jobs/${encodeURIComponent(jid)}/pr`, {})) }, jid); }
    catch (err) { prErrEl.textContent = err.message || "Failed to open PR"; prErrEl.hidden = prBtn.disabled = false; prBtn.textContent = "Open Draft PR"; }
  } }, "Open Draft PR") : null;
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
      res.pr_url ? el("a", { className: "btn-link", href: res.pr_url, target: "_blank", rel: "noopener noreferrer", "data-testid": "result-pr-link" }, `Pull Request: ${res.pr_url}`) : prBtn
    ),
    prErrEl,
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
  let currentStatus = detail.status, currentReason = detail.status_reason || "", currentMode = detail.spec?.mode === "interactive" ? "interactive" : "goal";
  let waking = false, promoteOpen = false, es = null, retryTimer = null;
  const maxIter = detail.spec?.budgets?.max_iterations || 10;
  const statusEl = el("span", { className: `badge status-${detail.status}`, "data-testid": "detail-status" }, formatStatusLabel(detail.status));
  const detailObjectiveEl = el("h2", { className: "detail-objective", "data-testid": "detail-objective" }, detail.spec?.objective || jobId);
  const suspendedNoteEl = el("p", { className: "muted suspended-note", "data-testid": "suspended-note", hidden: detail.status !== "suspended" }, "Suspended – waiting for you. The first action may take a few seconds while the task resumes.");
  const iterEl = el("span", { "data-testid": "detail-iteration" }, `iter ${detail.iteration ?? 0}/${maxIter}`);
  const activityText = formatActivity(detail.activity);
  const activityEl = el("span", { "data-testid": "job-activity", hidden: !activityText }, activityText);
  const usageEl = el("span", { "data-testid": "detail-usage" }, formatUsage(detail.usage));
  const streamEl = el("span", { className: "muted", "data-testid": "stream-status" }, "connecting…");
  const bannerEl = el("div", { className: "card pending-banner", "data-testid": "pending-permissions-banner", hidden: true });
  const questionHostEl = el("div", { "data-testid": "question-host" });
  const resultPanelEl = el("div", { className: "card", "data-testid": "job-result-panel", hidden: true });
  const checklistEl = el("ul", { className: "checklist", "data-testid": "job-checklist" });
  const checklistCardEl = el("div", { className: "card" }, el("h3", {}, "Checklist"), checklistEl);
  const transcriptEl = el("div", { className: "transcript", "data-testid": "job-transcript" });
  const detailGridEl = el("div", { className: "detail-grid" }, checklistCardEl, el("div", { className: "card" }, el("h3", {}, "Transcript"), transcriptEl));
  const diffSummaryEl = el("span", { className: "muted", "data-testid": "diff-summary" });
  const diffErrEl = el("p", { className: "error-text", "data-testid": "diff-error", hidden: true });
  const diffBodyEl = el("div", { className: "diff-body", "data-testid": "diff-body" });
  const loadDiffBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "load-diff-btn", onclick: () => loadDiff(false) }, "Load diff");
  const diffCardEl = el("div", { className: "card", "data-testid": "job-diff" }, el("div", { className: "detail-header-top" }, el("h3", { className: "detail-objective" }, "Changes"), diffSummaryEl, loadDiffBtn), diffErrEl, diffBodyEl);
  async function loadDiff(silent = false) {
    diffErrEl.hidden = loadDiffBtn.disabled = true;
    try { renderDiffFiles(diffBodyEl, diffSummaryEl, await apiText(`/v1/jobs/${encodeURIComponent(jobId)}/patch`)); loadDiffBtn.textContent = "Refresh diff"; }
    catch (err) { if (!silent) { diffErrEl.textContent = err.message || "Failed to load diff"; diffErrEl.hidden = false; } }
    finally { loadDiffBtn.disabled = false; }
  }

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

  const ensureLiveSSE = (force = false) => { if (force || !es || es.readyState === EventSource.CLOSED) openSSE(true, true); };
  const workspaceStatusEl = el("span", { className: "badge workspace-status", "data-testid": "workspace-status", hidden: true });
  const steerHeadingEl = el("h3", { className: "composer-heading", "data-testid": "steer-heading" }, "Send message");
  const promoteBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "promote-goal-btn", onclick: () => { promoteOpen = !promoteOpen; promoteErrEl.hidden = true; syncControls(currentStatus, currentReason); if (promoteOpen) promoteObjInput.focus(); } }, "Run Autonomously");
  const promoteObjInput = el("input", { className: "input", type: "text", value: detail.spec?.objective || "", placeholder: "Autonomous goal objective…", "data-testid": "promote-objective" });
  const promoteVerifyInput = el("textarea", { className: "input", rows: 2, placeholder: "Verify commands (one per line, e.g. go test ./...)", "data-testid": "promote-verify" }, (detail.spec?.verify || []).map((v) => v.join(" ")).join("\n"));
  const initDeliv = detail.spec?.delivery === "draft_pr" || detail.spec?.delivery === "pr" ? "pr" : detail.spec?.delivery === "none" ? "none" : "branch";
  const promoteDeliverySelect = el("select", { className: "select", "data-testid": "promote-delivery" },
    ...[["branch", "Push branch"], ["pr", "Open draft PR"], ["none", "Workspace only"]].map(([v, l]) => el("option", { value: v, ...(v === initDeliv ? { selected: "selected" } : {}) }, l)));
  const promoteSubmitBtn = el("button", { className: "btn", type: "submit", "data-testid": "promote-submit" }, "Run until verified");
  const promoteCancelBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "promote-cancel", onclick: () => { promoteOpen = false; syncControls(currentStatus, currentReason); } }, "Cancel");
  const promoteErrEl = el("p", { className: "error-text", "data-testid": "promote-error", hidden: true });
  const promoteFormEl = el("form", { className: "promote-form", "data-testid": "promote-goal-form", hidden: true, onsubmit: async (e) => {
      e.preventDefault();
      const objective = promoteObjInput.value.trim();
      if (!objective) { promoteErrEl.textContent = "Objective is required"; promoteErrEl.hidden = false; return; }
      promoteErrEl.hidden = true;
      promoteSubmitBtn.disabled = promoteCancelBtn.disabled = promoteBtn.disabled = true;
      const wasSuspended = currentStatus === "suspended";
      if (wasSuspended) { waking = true; syncControls(currentStatus, currentReason); }
      try {
        const verify = promoteVerifyInput.value.split("\n").map((ln) => ln.trim()).filter(Boolean).map(splitCommand).filter((a) => a.length);
        const body = { objective, delivery: promoteDeliverySelect.value, ...(verify.length ? { verify } : {}) };
        const resp = await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/goal`, body);
        waking = promoteOpen = state.inChat = state.finished = false;
        currentMode = "goal";
        detailObjectiveEl.textContent = resp?.job?.spec?.objective || objective;
        resultPanelEl.hidden = true;
        state.updateStatus("running", "");
        ensureLiveSSE(wasSuspended);
      } catch (err) {
        waking = false;
        promoteErrEl.textContent = err.message || "Failed to promote workspace";
        promoteErrEl.hidden = false;
      } finally {
        promoteSubmitBtn.disabled = promoteCancelBtn.disabled = false;
        syncControls(currentStatus, currentReason);
      }
    } }, promoteObjInput, promoteVerifyInput, el("div", { className: "steer-form" }, promoteDeliverySelect, promoteSubmitBtn, promoteCancelBtn), promoteErrEl);

  const steerInputEl = el("textarea", { className: "input steer-input composer-textarea", rows: 2, "data-testid": "steer-input", onkeydown: (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && e.keyCode !== 229) { e.preventDefault(); steerFormEl.dispatchEvent(new Event("submit", { cancelable: true })); }
  } });
  const steerSubmitBtn = el("button", { className: "btn", type: "submit", "data-testid": "steer-submit" }, "Send message");
  let steerInterrupt = false;
  const interruptBtn = el("button", { className: "btn btn-secondary", type: "button", "data-testid": "interrupt-btn", onclick: () => { steerInterrupt = true; steerFormEl.dispatchEvent(new Event("submit", { cancelable: true })); } }, "Interrupt");
  const steerErrEl = el("p", { className: "error-text", "data-testid": "steer-error", hidden: true });
  const steerFormEl = el("form", { className: "steer-form", "data-testid": "steer-form", onsubmit: async (e) => {
      e.preventDefault();
      const intr = steerInterrupt;
      steerInterrupt = false;
      const text = steerInputEl.value.trim() || (intr ? "Interrupt current step." : "");
      if (!text) return;
      steerErrEl.hidden = true;
      steerSubmitBtn.disabled = interruptBtn.disabled = steerInputEl.disabled = true;
      const wasSuspended = currentStatus === "suspended";
      if (wasSuspended) { waking = true; syncControls(currentStatus, currentReason); }
      try {
        if (isActiveStatus(currentStatus)) {
          await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/messages${intr ? "?interrupt=true" : ""}`, { text });
          steerInputEl.value = "";
          waking = false;
          if (wasSuspended || currentReason === "awaiting_turn") state.updateStatus("running", "");
          else syncControls(currentStatus, currentReason);
          ensureLiveSSE(wasSuspended);
        } else {
          await apiPost(`/v1/jobs/${encodeURIComponent(jobId)}/continue`, { text });
          steerInputEl.value = "";
          waking = state.finished = false;
          resultPanelEl.hidden = true;
          state.updateStatus("running", "");
          ensureLiveSSE(true);
        }
      } catch (err) {
        waking = false;
        syncControls(currentStatus, currentReason);
        steerErrEl.textContent = err.message || "Request failed";
        steerErrEl.hidden = false;
      } finally {
        steerSubmitBtn.disabled = interruptBtn.disabled = steerInputEl.disabled = false;
      }
    } }, steerInputEl, el("div", { className: "composer-actions" }, steerSubmitBtn, interruptBtn));
  const steerCardEl = el("div", { className: "card composer-card", "data-testid": "steer-card" }, el("div", { className: "composer-top" }, steerHeadingEl, workspaceStatusEl, promoteBtn), promoteFormEl, steerFormEl, steerErrEl);
  const composerEl = el("div", { className: "workspace-composer", "data-testid": "workspace-composer" }, steerCardEl);

  function syncControls(st, reason = currentReason) {
    currentStatus = st;
    currentReason = reason;
    const active = isActiveStatus(st), wsText = formatWorkspaceStatus(st, reason, currentMode, waking);
    workspaceStatusEl.textContent = wsText;
    workspaceStatusEl.hidden = !wsText;
    workspaceStatusEl.className = `badge workspace-status ${waking || st === "running" ? "status-running" : st === "suspended" ? "status-suspended" : "status-achieved"}`;
    suspendedNoteEl.hidden = st !== "suspended";
    cancelBtn.hidden = !active;
    interruptBtn.hidden = st !== "running";
    if (!active) cancelDialogEl.hidden = true;
    checklistCardEl.hidden = currentMode === "interactive";
    detailGridEl.className = currentMode === "interactive" ? "detail-grid detail-grid-chat" : "detail-grid";
    const canPromote = currentMode === "interactive" && (st === "waiting_for_input" || st === "suspended" || st === "created" || st === "interrupted" || st === "blocked");
    promoteBtn.hidden = currentMode !== "interactive" || st === "achieved" || st === "canceled";
    promoteBtn.disabled = !canPromote || waking;
    if (!canPromote) promoteOpen = false;
    promoteFormEl.hidden = !promoteOpen;
    composerEl.hidden = steerCardEl.hidden = st === "achieved";
    const label = active ? "Send message" : "Continue with instructions";
    steerHeadingEl.textContent = currentMode === "interactive" && active ? "Workspace chat" : label;
    steerSubmitBtn.textContent = label;
    steerInputEl.placeholder = !active ? "Instructions to continue this job…" : currentMode === "interactive" ? "Reply to workspace… (Enter to send, Shift+Enter for newline)" : "Send guidance to the next iteration…";
  }

  const loadResult = () => apiRequest(`/v1/jobs/${encodeURIComponent(jobId)}/result`).then(async (res) => {
    renderResultPanel(resultPanelEl, res, jobId);
    if (res.diffstat || (res.branch && (!res.base_sha || res.head_sha !== res.base_sha))) await loadDiff(true);
  }).catch(() => {});

  const state = createTranscriptState({
    jobId,
    transcriptEl,
    initialLastSeq: Number(detail.last_seq || 0),
    statusFloorSeq: Number(detail.last_seq || 0),
    inChat: currentMode === "interactive",
    activity: { phase: detail.activity?.phase || "", model_turns: Number(detail.activity?.model_turns || 0), last_tool: detail.activity?.last_tool || "" },
    updateActivity: (a) => { const txt = formatActivity(a); activityEl.textContent = txt; activityEl.hidden = !txt; },
    syncPermissions: () => {
      const pending = [...state.permissionCards.values()].filter((c) => c.stateBadge.textContent === "pending");
      if (!pending.length) { bannerEl.hidden = true; bannerEl.replaceChildren(); return; }
      bannerEl.hidden = false;
      bannerEl.replaceChildren(
        el("strong", { "data-testid": "pending-permissions-count" }, `${pending.length} permission ${pending.length === 1 ? "request" : "requests"} waiting for approval`),
        el("span", { className: "muted" }, " — " + pending.map((p) => `${p.toolLabel}${p.target ? " (" + p.target + ")" : ""}`).join(", "))
      );
    },
    updateStatus: (st, reason = "") => { waking = false; statusEl.className = `badge status-${st}`; statusEl.textContent = formatStatusLabel(st); syncControls(st, reason); if (st === "waiting_for_input") loadDiff(true); },
    onPromoted: (spec) => { currentMode = "goal"; promoteOpen = false; if (spec?.objective) detailObjectiveEl.textContent = spec.objective; syncControls(currentStatus, currentReason); },
    onAction: () => { ensureLiveSSE(true); },
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
  syncControls(detail.status, currentReason);
  for (const perm of pendingResp.permissions || []) upsertPermissionCard(state, perm.id, perm.request || {}, perm.state || "pending", 0);
  let openQuestionId = "";
  state.onQuestion = (q) => {
    if (!q?.id || q.id === openQuestionId) return;
    openQuestionId = q.id;
    questionHostEl.replaceChildren(renderQuestionCard(jobId, q, () => { openQuestionId = ""; questionHostEl.replaceChildren(); state.updateStatus?.("running", ""); }));
  };
  state.onQuestionClosed = (id) => { if (id === openQuestionId) { openQuestionId = ""; questionHostEl.replaceChildren(); } };
  if (detail.question) state.onQuestion(detail.question);
  if (!isActiveStatus(detail.status)) await loadResult();
  else if (detail.status === "waiting_for_input") await loadDiff(true);

  root.replaceChildren(el("section", { "data-testid": "job-detail-view" },
    el("a", { className: "back-link", href: "#/", "data-testid": "back-to-jobs" }, "← All jobs"),
    bannerEl,
    questionHostEl,
    el("div", { className: "card", "data-testid": "job-header" },
      el("div", { className: "detail-header-top" }, statusEl, detailObjectiveEl, cancelBtn),
      suspendedNoteEl,
      cancelDialogEl,
      el("div", { className: "detail-meta" }, iterEl, activityEl, el("span", { "data-testid": "detail-model" }, formatModel(detail.spec)), el("span", { "data-testid": "detail-repo" }, formatRepo(detail.spec)), usageEl, streamEl)
    ),
    resultPanelEl,
    diffCardEl,
    detailGridEl,
    composerEl
  ));

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

export function splitCommand(line) {
  const args = [];
  let cur = "", inArg = false, q = "";
  for (let i = 0; i < line.length; i++) {
    const c = line[i];
    if (q === "'") { if (c === "'") q = ""; else cur += c; }
    else if (q === '"') {
      if (c === '"') q = "";
      else if (c === "\\" && i + 1 < line.length && (line[i + 1] === '"' || line[i + 1] === "\\")) cur += line[++i];
      else cur += c;
    } else if (c === "'" || c === '"') { q = c; inArg = true; }
    else if (c === "\\" && i + 1 < line.length) { cur += line[++i]; inArg = true; }
    else if (/\s/.test(c)) { if (inArg) { args.push(cur); cur = ""; inArg = false; } }
    else { cur += c; inArg = true; }
  }
  if (q) throw new Error(`Unterminated ${q} quote in verify command`);
  if (inArg) args.push(cur);
  return args;
}

async function renderNewJobView(root) {
  stopBackground();
  const [jobsResp, catalog] = await Promise.all([apiRequest("/v1/jobs").catch(() => ({ jobs: [] })), apiRequest("/v1/models").catch(() => ({}))]);
  const recentRepos = [...new Set((jobsResp.jobs || []).map((j) => j.spec?.repo?.url || j.spec?.repo?.path || "").filter(Boolean))].slice(0, 8);
  const providers = catalog.providers || [], codingDef = catalog.defaults?.coding || {};
  let mode = "goal", deliveryTouched = false;
  const repoInput = el("input", { className: "input", type: "text", list: "recent-repos-list", "data-testid": "new-job-repo" });
  const baseInput = el("input", { className: "input", type: "text", placeholder: "Base branch (optional, defaults to repo default)", "data-testid": "new-job-base" });
  const promptLabel = el("label", { className: "tool-label", "data-testid": "new-job-prompt-label" });
  const objectiveInput = el("textarea", { className: "input", rows: 4, "data-testid": "new-job-objective" });
  const issueInput = el("input", { className: "input", type: "text", placeholder: "#42, owner/repo#42, or GitHub issue URL", "data-testid": "new-job-issue", hidden: true });
  const verifyInput = el("textarea", { className: "input", rows: 2, placeholder: "One command per line (e.g. go test ./...)", "data-testid": "new-job-verify" });
  const deliverySelect = el("select", { className: "select", "data-testid": "new-job-delivery", onchange: () => { deliveryTouched = true; } },
    el("option", { value: "branch" }, "Push branch"), el("option", { value: "pr" }, "Open draft PR"), el("option", { value: "none" }, "Workspace only"));
  const providerSelect = el("select", { className: "select", "data-testid": "new-job-provider", onchange: () => syncModels("") },
    el("option", { value: "" }, "Default"), ...providers.map((p) => el("option", { value: p.provider }, p.provider)));
  const modelSelect = el("select", { className: "select", "data-testid": "new-job-model", onchange: () => syncThinking() });
  const thinkingSelect = el("select", { className: "select", "data-testid": "new-job-thinking" });
  function syncThinking() {
    const p = providers.find((x) => x.provider === providerSelect.value);
    const m = (p?.models || []).find((x) => x.id === modelSelect.value);
    thinkingSelect.replaceChildren(el("option", { value: "" }, "Default"), ...(m?.thinking_levels || []).map((lv) => el("option", { value: lv }, lv)));
  }
  function syncModels(preferred) {
    const p = providers.find((x) => x.provider === providerSelect.value);
    const models = p?.models || [], target = preferred || p?.default_model || "";
    modelSelect.replaceChildren(el("option", { value: "" }, "Default"), ...models.map((m) => el("option", { value: m.id }, m.id)));
    if (models.some((m) => m.id === target)) modelSelect.value = target;
    syncThinking();
  }
  if (codingDef.provider && providers.some((p) => p.provider === codingDef.provider)) providerSelect.value = codingDef.provider;
  syncModels(codingDef.model || "");
  const modeBtns = [["interactive", "Interactive Chat"], ["goal", "Autonomous Goal"], ["issue", "GitHub Issue"]].map(([id, label]) =>
    el("button", { type: "button", "data-mode": id, "data-testid": `mode-${id}`, onclick: () => setMode(id) }, label));
  function setMode(next) {
    mode = next;
    for (const b of modeBtns) {
      const active = b.getAttribute("data-mode") === mode;
      b.className = active ? "btn mode-btn" : "btn btn-secondary mode-btn";
      b.setAttribute("aria-pressed", String(active));
    }
    promptLabel.textContent = mode === "issue" ? "GitHub Issue" : mode === "interactive" ? "Initial prompt" : "Objective";
    objectiveInput.hidden = mode === "issue";
    issueInput.hidden = mode !== "issue";
    objectiveInput.placeholder = mode === "interactive" ? "Ask a question or describe what to explore or change…" : "Describe the goal to implement and verify…";
    repoInput.placeholder = mode === "issue" ? "https://github.com/owner/repo (optional for owner/repo#N)" : "https://github.com/owner/repo";
    if (!deliveryTouched) deliverySelect.value = mode === "issue" ? "pr" : "branch";
  }
  setMode("goal");
  const field = (lbl, ...kids) => el("div", { className: "field-group" }, typeof lbl === "string" ? el("label", { className: "tool-label" }, lbl) : lbl, ...kids);
  const errEl = el("p", { className: "error-text", "data-testid": "new-job-error", hidden: true });
  const submitBtn = el("button", { className: "btn", type: "submit", "data-testid": "new-job-submit" }, "Start workspace");
  const form = el("form", { className: "new-job-form", "data-testid": "new-job-form", onsubmit: async (e) => {
    e.preventDefault();
    errEl.hidden = submitBtn.disabled = true;
    try {
      const verify = verifyInput.value.split("\n").map((ln) => ln.trim()).filter(Boolean).map(splitCommand).filter((a) => a.length);
      const rawRepo = repoInput.value.trim(), baseRef = baseInput.value.trim(), repo = {};
      if (rawRepo) { if (rawRepo.startsWith("/")) repo.path = rawRepo; else repo.url = rawRepo; }
      if (baseRef) repo.base_ref = baseRef;
      const spec = mode === "issue" ? { mode: "goal", issue: issueInput.value.trim(), repo, delivery: deliverySelect.value } : { mode, objective: objectiveInput.value.trim(), repo, delivery: deliverySelect.value };
      if (verify.length) spec.verify = verify;
      if (providerSelect.value) spec.provider = providerSelect.value;
      if (modelSelect.value) spec.model = modelSelect.value;
      if (thinkingSelect.value) spec.thinking = thinkingSelect.value;
      window.location.hash = `#/jobs/${encodeURIComponent((await apiPost("/v1/jobs?start=true", spec)).job.id)}`;
    } catch (err) { errEl.textContent = err.message || "Failed to submit job"; errEl.hidden = false; }
    finally { submitBtn.disabled = false; }
  } },
    field("Workspace mode", el("div", { className: "mode-tabs", "data-testid": "new-job-mode" }, ...modeBtns)),
    field("Repository", repoInput, el("datalist", { id: "recent-repos-list" }, ...recentRepos.map((r) => el("option", { value: r }))),
      recentRepos.length ? el("div", { className: "repo-chips", "data-testid": "recent-repo-chips" }, ...recentRepos.map((r) => el("button", { className: "btn-link repo-chip", type: "button", "data-testid": "recent-repo-chip", onclick: () => { repoInput.value = r; } }, r))) : null),
    field("Base branch", baseInput),
    field(promptLabel, objectiveInput, issueInput),
    el("details", { className: "verify-item", "data-testid": "new-job-options" },
      el("summary", { className: "tool-summary", "data-testid": "new-job-options-summary" }, "Options"),
      el("div", { className: "tool-body" }, field("Verify commands", verifyInput),
        el("div", { className: "options-grid" }, field("Delivery", deliverySelect), field("Provider", providerSelect), field("Model", modelSelect), field("Thinking", thinkingSelect)))),
    errEl, submitBtn
  );
  root.replaceChildren(el("section", { "data-testid": "new-job-view" },
    el("a", { className: "back-link", href: "#/", "data-testid": "back-to-jobs" }, "← All jobs"),
    el("div", { className: "card" }, el("h2", {}, "New Workspace"), form)
  ));
}

export function consumeHashToken() {
  const raw = window.location.hash.replace(/^#/, ""), sep = raw.startsWith("/") ? raw.search(/[?&]/) : 0;
  if (!raw.includes("token=") || sep < 0) return "";
  const routePart = sep > 0 ? raw.slice(0, sep) : "", params = new URLSearchParams(sep > 0 ? raw.slice(sep + 1) : raw);
  const tok = (params.get("token") || "").trim(), next = (params.get("next") || "").trim().replace(/^#/, "");
  if (!tok) return "";
  params.delete("token"); params.delete("next");
  const base = next ? (next.startsWith("/") ? next : `/${next}`) : (routePart || "/"), rest = params.toString();
  window.history.replaceState(null, "", `${window.location.pathname}${window.location.search}#${base}${rest ? (base.includes("?") ? "&" : "?") + rest : ""}`);
  return tok;
}

async function route() {
  const magicToken = consumeHashToken();
  const root = document.getElementById("app");
  if (!root) return;
  if (magicToken) try { await login(magicToken); } catch { renderLogin(); return; }
  const session = getCSRFToken() ? { authenticated: true } : await fetchSession();
  if (!session) { renderLogin(); return; }
  if (session.login) sessionLogin = session.login;
  setSessionBanner(sessionLogin && sessionLogin !== "dev-token" ? `Signed in as ${sessionLogin}` : "Authenticated");
  if (window.location.hash === "#/new") {
    await renderNewJobView(root);
    return;
  }
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
