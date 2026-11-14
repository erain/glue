# Remote Mode Phase 4: Logged-In Browser Workspace, Closed-Laptop Control, and Bidirectional Continuity

Status: Planned, 2026-11-14 · tracker [#402](https://github.com/erain/glue/issues/402) · issues [#624](https://github.com/erain/glue/issues/624)–[#630](https://github.com/erain/glue/issues/630), [#632](https://github.com/erain/glue/issues/632)–[#638](https://github.com/erain/glue/issues/638) (design [#623](https://github.com/erain/glue/issues/623), parent [#392](https://github.com/erain/glue/issues/392)). Builds on Phase 3 ([remote-mode-phase3-design.md](remote-mode-phase3-design.md), [remote-quickstart.md](remote-quickstart.md)) and scopes an opt-in amendment to [ADR-0020](adr/0020-ax-first-direct-execution.md) (ADR-0021 planned in [#636](https://github.com/erain/glue/issues/636)).

Phase 3 made direct CLI-to-AX remote jobs fast and trustworthy (`scripts/demo/aha.sh`). Phase 4
turns a developer's own [AX](https://github.com/google/ax) / Agent Substrate cluster into a
**bookmarkable, logged-in browser remote workspace** (`https://glue.<domain>` behind Cloudflare
Access) with serverless idle suspend/resume (`0` AX workers when idle), conversational and
autonomous execution on the same `/workspace`, unified browser diff and 1-tap Draft PRs, and
two-way continuity between terminal, browser, and phone.

```text
# One-time setup (0 AX worker slots, zero DIY OAuth App, zero separate VM):
$ glue remote setup --public-url https://glue.example.com --cf-team-domain https://myteam.cloudflareaccess.com --cf-aud <aud>
$ glue job init                                  # writes ~/.config/glue/config.yaml (0600) + ntfy QR

# Closed laptop — open https://glue.example.com on phone/tablet (already signed in via Cloudflare Access):
  #/new -> start interactive workspace on owner/repo -> chat 2 turns -> idle auto-suspends (0 workers)
  next message wakes sandbox in ~0.2s -> tap "Run Autonomously" -> unified diff -> tap "Open Draft PR"
  phone lock screen (ntfy): tap action button to answer ask_user without opening a laptop

# Back at desk — pull git branch/patch + remote session briefing into local coding TUI:
$ glue job teleport latest
```

## 1. Competitive UX Synthesis

| Tool | Best UX pattern | Structural gap for self-hosted remote coding | What Glue Phase 4 adopts |
|---|---|---|---|
| **`pi`** | Bare command launches an interactive coding TUI immediately; minimal flag surface | Local-only terminal loop; no durable remote sandboxes | Bare `glue` opens the coding TUI on a TTY; concise `--help` ([#625](https://github.com/erain/glue/issues/625)) |
| **`OpenCode`** | `opencode serve` attaches a web or second terminal client to a live session | User still manages host uptime, reverse proxies, and auth | Zero-YAML Day-0 `glue job ui` / `glue-gateway --quick` ([#628](https://github.com/erain/glue/issues/628)) |
| **`Claude Code`** (`/rc`, `--teleport`) | `/rc` hands a live session to phone/web; `--teleport` pulls remote branch + context back to a local terminal | Tied to Anthropic-hosted cloud sessions | `/remote` browser permalink + `glue job teleport` / TUI `/teleport` ([#633](https://github.com/erain/glue/issues/633)) |
| **`OpenAI Codex`** (Handoff) | Browser task launcher, inline unified diff viewer, and 1-click PR creation | Vendor-hosted cloud only; goal-batch rather than live cluster workspace | Browser `#/new` launcher, `/patch` unified diff viewer, and 1-tap Draft PR ([#627](https://github.com/erain/glue/issues/627), [#629](https://github.com/erain/glue/issues/629), [#632](https://github.com/erain/glue/issues/632)) |
| **`Cursor`** (Background Agents) | Conversational follow-ups and issue-driven background runs with PR output | Coupled to a proprietary IDE and vendor pet VMs | Multi-turn browser composer + `--issue` runs on self-hosted AX ([#632](https://github.com/erain/glue/issues/632), [#637](https://github.com/erain/glue/issues/637), [#638](https://github.com/erain/glue/issues/638)) |
| **`slopus/happy`** | One-time QR phone pairing and actionable mobile push notifications when an agent pauses | Requires keeping the local machine awake or trusting a third-party relay | `~/.config/glue/config.yaml` + `glue job init` QR pairing and two-way `ntfy` HTTP actions ([#624](https://github.com/erain/glue/issues/624), [#626](https://github.com/erain/glue/issues/626)) |
| **Antigravity** (logged-in browser workspace) | Permanent, edge-authenticated browser URL opening directly into a conversational workspace that escalates to autonomous goals | Hosted platform; not packaged for a user's own Kubernetes/AX cluster | `https://glue.<domain>` behind Cloudflare Access + serverless AX `mode: "interactive"` ↔ `mode: "goal"` ([#635](https://github.com/erain/glue/issues/635)–[#638](https://github.com/erain/glue/issues/638)) |

**Product epiphany:** Developers do not want to register DIY GitHub OAuth Apps, babysit a
separate pet VM for `glue-gateway`, or scan a fresh ephemeral tunnel QR code per job. A one-time
`glue remote setup` placing a tiny `glue-gateway` + `cloudflared` Deployment on the AX cluster
behind **Cloudflare Access** gives a permanent, already-logged-in URL (`https://glue.<domain>`)
while AX's gVisor suspend/resume drops idle workspaces to **zero worker slots** and wakes them
in ~0.2 s.

## 2. Seven UX Friction Points on `main` (`156eac5`)

1. **High-ceremony public browser auth and separate host requirement:** `AuthConfig` and
   `Config.Validate` in `gateway/config.go` (`authOK` at lines 169–175) only permit
   `auth.mode: "github"` (requiring a manual GitHub OAuth App `client_id`, `client_secret`, and
   callback URL) or `dev-token` (rejected on `https://` without `--insecure-dev`), while
   [ADR-0020](adr/0020-ax-first-direct-execution.md) item 4 assumes an out-of-cluster host/VM
   (`deploy/cloudflared/glue-gateway.service`). Addressed by [#635](https://github.com/erain/glue/issues/635),
   [#636](https://github.com/erain/glue/issues/636), and local Day-0 [#628](https://github.com/erain/glue/issues/628).
2. **Batch-goal-only remote jobs:** `Manager.execute` and `Manager.pursue` in `jobs/runtime.go`
   (lines 169–298) unconditionally run `agent.PursueGoal(ctx, spec)` (planner → maker → audit
   checker) and finalize into a terminal state. A developer cannot open a remote repo on
   `/workspace`, chat back-and-forth for a few turns, and then escalate to an autonomous
   verified goal. Addressed by [#637](https://github.com/erain/glue/issues/637) and
   [#638](https://github.com/erain/glue/issues/638).
3. **Watch-only browser UI without diff review, creation, or interrupt:** `route()` in
   `webui/static/app.js` (lines 636–654) only serves `#/`, `#/tokens`, and `#/jobs/<id>` (no
   `#/new` launcher); `applyJobEvent` (`tool_start`, lines 331–345) renders raw tool names and
   JSON arguments; `steerCardEl` (lines 486–533) lacks an `--interrupt` toggle and hides once
   `st === "achieved"` (line 528); and `renderResultPanel` (lines 422–453) displays
   `res.diffstat` without a unified `/patch` diff viewer or **Open Draft PR** button. Addressed
   by [#629](https://github.com/erain/glue/issues/629), [#632](https://github.com/erain/glue/issues/632),
   and [#638](https://github.com/erain/glue/issues/638).
4. **No post-hoc Draft PR command for branch jobs, and `jobPick` drops `pr_url`:** Default
   `delivery: branch` jobs have no `glue job pr` CLI or `POST /v1/jobs/{id}/pr` API to open a PR
   after diff review. Worse, `jobPick` in `cmd/glue/jobattempts.go` (lines 161–183) calls
   `jobs.OpenDraftPR` without persisting `Result.PRURL`, so `continueFromPR` in
   `cmd/glue/jobprfeedback.go` (lines 16–26) later fails with `job <id> has no pull request`.
   Addressed by [#627](https://github.com/erain/glue/issues/627).
5. **Stateless, one-way phone notifications:** `Manager.notifyURLs` and `postNotification` in
   `jobs/notify.go` (lines 63–73, 154–165) require `--notify` or `GLUE_NOTIFY_URL` on every
   submit (no user-level config) and send plain-text `ntfy` messages that set `Click: n.PRURL`
   only when a PR exists — offering neither a browser deep-link on `waiting_for_input` nor
   `ntfy` HTTP `Actions` buttons to answer `ask_user` or permission prompts from a lock screen.
   Addressed by [#624](https://github.com/erain/glue/issues/624) and [#626](https://github.com/erain/glue/issues/626).
6. **Flag-wall CLI entry on bare `glue`:** `runCLIWithDeps` in `cmd/glue/main.go` (lines
   106–109) invokes `printUsage(stdout)` whenever `len(args) == 0`, printing ~180 lines of
   daemon/connect/MCP flags (`cmd/glue/main.go` lines 225–403) on an interactive TTY instead of
   starting the coding TUI. Addressed by [#625](https://github.com/erain/glue/issues/625).
7. **One-way TUI handoff and passive `/jobs` panel:** `runSlashRemote` in `cmd/glue/tui/remote.go`
   (lines 68–118) pushes `remoteContext` to AX without showing a browser permalink, while
   `pullJob` in `cmd/glue/jobpull.go` (lines 26–93) pulls git branches or patches without
   hydrating remote session context back into a local coding session. Inside the TUI,
   `JobsBridge` and `handleJobsKey` in `cmd/glue/tui/jobs.go` (lines 23–39, 175–219) expose only
   an unscrollable 18-line tail (`jobsTranscriptLines = 18`) and `p` to pull — with no scroll,
   diff, answer, steer, PR, or cancel actions. Addressed by [#630](https://github.com/erain/glue/issues/630)
   and [#633](https://github.com/erain/glue/issues/633).

## 3. Architectural Decisions

### 3.1 Rejecting a Multi-Job Portal Inside an AX `Task` (gVisor `Actor`)

Running `glue-gateway` or per-job public tunnels (`#631`) inside an AX `Task` was evaluated and
rejected for three structural reasons:

- **Sandbox trust boundary vs. cluster control credentials:** An AX `Task` runs untrusted
  model-generated shell commands (`shell_exec`) inside gVisor. Mounting cluster-wide `ax-server`
  gRPC credentials, `atenet-router` client certs, or `ax-system/glue-ax-task-hmac` inside a task
  violates the Phase 3 sandbox boundary ([#544](https://github.com/erain/glue/issues/544)).
- **25–60 minute `PodCertificateRequest` expiry:** Outside in-cluster RBAC, `credSource.get` and
  `mintPodCert` in `internal/axbackend/tunnel.go` (lines 408–503) mint short-lived router mTLS
  certificates (`maxExpirationSeconds: 3600`, cached at `min(now+25m, notAfter-1m)`) and 3600 s
  `ax-server` tokens. A task sandbox has no Kubernetes API access to renew them once the laptop
  closes.
- **Worker-slot starvation vs. suspend tunnel death:** Keeping a portal or `--share`
  `cloudflared` task awake permanently consumes a scarce AX gVisor worker slot; suspending the
  task kills the in-sandbox tunnel process so mobile requests cannot wake it (`#631` closed as
  `not planned`).

### 3.2 Opt-In In-Cluster `glue-gateway` + Cloudflare Tunnel + Cloudflare Access ([#635](https://github.com/erain/glue/issues/635), [#636](https://github.com/erain/glue/issues/636))

```text
phone / closed-laptop browser ──HTTPS──► Cloudflare Access (SSO/OTP edge)
                                                │ Cf-Access-Jwt-Assertion (JWKS verified)
                                                ▼
                      AX / K8s cluster: Deployment/glue-gateway (+ cloudflared, 0 AX worker slots)
                                                │ in-cluster mTLS (atenet-router + ax-server)
laptop CLI: glue --remote / glue job ───────────┴──► AX Tasks (gVisor): suspend to 0 workers when idle
```

- **Zero worker slots, 24/7 lifecycle:** `glue remote setup` ([#636](https://github.com/erain/glue/issues/636))
  deploys a tiny standard Kubernetes `Deployment` (`glue-gateway` + `cloudflared` sidecar) on
  the same cluster that already runs AX. It consumes **0 AX gVisor worker slots**, needs **0
  extra VMs**, renews router mTLS natively in-cluster (`credSource.inCluster` in
  `internal/axbackend/tunnel.go` lines 409–415), auto-suspends idle/waiting tasks after
  `suspend_after`, and wakes them on browser or `ntfy` actions.
- **Zero-login-prompt Zero Trust auth:** `auth.mode: "cloudflare-access"`
  ([#635](https://github.com/erain/glue/issues/635)) verifies Cloudflare's
  `Cf-Access-Jwt-Assertion` header against `<team_domain>/cdn-cgi/access/certs`
  (`RS256`/`ES256`, strict `iss`/`aud`/`exp`, key-rotation cache, optional email allowlist) and
  mints Glue's CSRF-protected session seamlessly — no GitHub OAuth App setup and no second
  sign-in screen.
- **Scoped amendment to [ADR-0020](adr/0020-ax-first-direct-execution.md):** CLI `glue --remote`
  and `glue job` remain **100% direct-to-AX** with zero gateway required. In-cluster manifests
  are never required for CLI use and are managed solely by opt-in `glue remote setup` / `status`
  / `down` (recorded as ADR-0021 in [#636](https://github.com/erain/glue/issues/636)).

### 3.3 Unified Interactive (`mode: "interactive"`) and Goal (`mode: "goal"`) Workspaces ([#637](https://github.com/erain/glue/issues/637), [#632](https://github.com/erain/glue/issues/632), [#638](https://github.com/erain/glue/issues/638))

Interactive remote coding and autonomous goals share one `jobs.Job` model on `/workspace`:

- **`mode: "interactive"`:** Prepared on the same repo clone/snapshot and `/workspace/.glue`
  session store, each `POST /v1/jobs/{id}/messages` runs one conversational coding turn
  (`agent.Session`) and parks the job in `awaiting_turn` (no terminal finalization, no `notify`
  spam). After `suspend_after` of inactivity, `glue-gateway` suspends the AX task (`0` workers);
  the next message wakes it in ~0.2 s with full file and session continuity.
- **In-place escalation (`POST /v1/jobs/{id}/goal`):** From `#/new`
  ([#632](https://github.com/erain/glue/issues/632)) or mid-conversation on `#/jobs/<id>`
  ([#638](https://github.com/erain/glue/issues/638)), clicking **Run Autonomously** promotes the
  live `/workspace` dirty tree and session context into `PursueGoal` (planner → maker → audit
  checker + `verify` commands), ending with a unified diff and 1-tap Draft PR.

## 4. Ranked Critical User Journeys (`P0`–`P3`)

| Rank | Critical User Journey | Issues |
|---|---|---|
| **`P0.1`** | **Bookmarkable logged-in browser workspace (`https://glue.<domain>`):** One-time `glue remote setup` deploys in-cluster `glue-gateway` + `cloudflared` (`0` AX workers) protected by `auth.mode: "cloudflare-access"` (`Cf-Access-Jwt-Assertion`). | [#635](https://github.com/erain/glue/issues/635), [#636](https://github.com/erain/glue/issues/636) |
| **`P0.2`** | **Interactive remote coding + seamless autonomous escalation:** Start or resume `mode: "interactive"` workspaces from `#/new`, chat across idle AX suspend/resume cycles, and click **Run Autonomously** (`POST /v1/jobs/{id}/goal`) on the same `/workspace`. | [#637](https://github.com/erain/glue/issues/637), [#632](https://github.com/erain/glue/issues/632), [#638](https://github.com/erain/glue/issues/638) |
| **`P0.3`** | **Unified diff review & 1-tap Open Draft PR:** Inspect live/finished `/patch` unified diffs, readable tool summaries, and `--interrupt` steering in `webui`, then open a Draft PR via `POST /v1/jobs/{id}/pr` / `glue job pr` (also persisting `pr_url` in `jobPick`). | [#627](https://github.com/erain/glue/issues/627), [#629](https://github.com/erain/glue/issues/629) |
| **`P1.1`** | **Bidirectional terminal ↔ browser handoff:** Bare `glue` opens the coding TUI on a TTY; `/remote` prints the browser permalink; `glue job teleport` / `/teleport` pulls both git changes and a remote session briefing into local TUI. | [#625](https://github.com/erain/glue/issues/625), [#633](https://github.com/erain/glue/issues/633) |
| **`P1.2`** | **Pocket push tap-back:** `~/.config/glue/config.yaml` (`0600`) + `glue job init` QR pairing persist notify settings across AX wakes; `ntfy` notifications gain HTTPS lock-screen Action Buttons and browser `Click` deep-links. | [#624](https://github.com/erain/glue/issues/624), [#626](https://github.com/erain/glue/issues/626) |
| **`P1.3`** | **Zero-setup Day-0 local mode:** `glue job ui` (`127.0.0.1` + `#token=` fragment QR magic link, optional `--tunnel`) and interactive TUI `/jobs` mission control (scroll, diff, answer, steer, PR, cancel). | [#628](https://github.com/erain/glue/issues/628), [#630](https://github.com/erain/glue/issues/630) |
| **`P2`** | **Post-Phase-4 polish:** Browser multi-attempt compare/pick UI, 1-tap `continue --from-pr` in browser, and `CF-Access-Client-Id` service-token CLI mode. | Deferred |
| **`P3`** | **Demoted / superseded:** Per-job `--share` tunnels inside AX tasks ([#631](https://github.com/erain/glue/issues/631) closed as `not planned` — superseded by permanent [#635](https://github.com/erain/glue/issues/635)/[#636](https://github.com/erain/glue/issues/636) gateway), sandbox port previews, and GitHub webhook/cron triggers. | Superseded / deferred |

## 5. Wave 1–4 Execution Plan & Gatekeeper Verification

Every implementation PR is sized `< 500` changed lines and must pass standard `gofmt`, `go vet`,
root + `gateway` `-race` tests, `scripts/check-tracker.sh --strict`, and its named Gatekeeper
checks (which **fail rather than skip** under `CI=true` when `GLUE_E2E_AX=1` or
`GLUE_E2E_BROWSER=1` is set). Secrets (`Cf-Access-Jwt-Assertion`, capability tokens, `ntfy`
URLs) must never appear in `events.jsonl`, `result.json`, `.glue/remote.yaml`, logs, or URL
query strings (`#token=` fragments only for local magic links, scrubbed via
`history.replaceState` after `HttpOnly` cookie exchange).

| Wave | Issue | CUJ | Depends on | Named Gatekeeper Verification |
|---|---|---|---|---|
| **1** | [#635](https://github.com/erain/glue/issues/635) `cloudflare-access` JWT auth in `glue-gateway` | `P0.1a` | — | `gateway` `-race` (`TestCloudflareAccessValidRS256AndES256`, `TestCloudflareAccessRejectsBadAudIssExpAndAlgNone`) + `GLUE_E2E_BROWSER=1` |
| **1** | [#637](https://github.com/erain/glue/issues/637) `mode: "interactive"` + `/goal` escalation | `P0.2a` | — | `-race` (`TestInteractiveJobMultiTurnPreservesSessionAndWorkspace`, `TestInteractiveJobPromoteToGoalRunsVerifyOnSameWorktree`) |
| **1** | [#627](https://github.com/erain/glue/issues/627) `glue job pr`, `POST /v1/jobs/{id}/pr`, `jobPick` `pr_url` | `P0.3a` | — | `-race` (`TestJobPROpensDraftPRForSingleJob`, `TestJobPickPersistsPRURLForContinueFromPR`, `TestJobPRDoesNotWakeSuspendedAXTask`) |
| **1** | [#624](https://github.com/erain/glue/issues/624) `~/.config/glue/config.yaml` + `glue job init` QR | `P1.2a` | — | `-race` (`TestUserConfigPrecedence`, `TestUserConfigPermissions0600`) + `GLUE_E2E_AX=1` (`TestE2EAXUserConfigNotifyWithoutFlag`) |
| **1** | [#625](https://github.com/erain/glue/issues/625) Bare `glue` coding TUI + concise `--help` | `P1.1a` | — | `-race` (`TestBareGlueTTYLaunchesCodingTUI`, `TestConciseTopLevelHelp`, `TestSubcommandHelpFlags`) |
| **2** | [#636](https://github.com/erain/glue/issues/636) `glue remote setup` / `status` / `down` + ADR-0021 | `P0.1b` | #624, #635 | `-race` (`TestRemoteSetupManifestsAndConfigValidate`, `TestRemoteSetupRedactsSecrets`) + `GLUE_E2E_AX=1` (`TestE2EClusterGatewaySetupAndCloudflareAccess`) |
| **2** | [#629](https://github.com/erain/glue/issues/629) `webui` unified diff, tool summaries, `--interrupt`, Draft PR | `P0.3b` | #627 | `GLUE_E2E_BROWSER=1` (`TestE2EDiffViewerAndOpenPR`, `TestE2EInterruptSteeringAndToolSummaries` at desktop + `390x844`) |
| **2** | [#632](https://github.com/erain/glue/issues/632) `webui` `#/new` workspace, job & `--issue` launcher | `P0.2c` | #629, #637 | `GLUE_E2E_BROWSER=1` (`TestE2ENewJobSubmitFromBrowser`, `TestE2ENewJobFromGitHubIssue` at `390x844`) |
| **2** | [#626](https://github.com/erain/glue/issues/626) Two-way `ntfy` HTTP Action Buttons + `Click` URL | `P1.2b` | #624 | `-race` (`TestNtfyQuestionActionButtonsUnblockJob`) + `GLUE_E2E_AX=1` (`TestE2EAXNtfyTwoWayQuestionReply`) |
| **2** | [#628](https://github.com/erain/glue/issues/628) `glue job ui` + `glue-gateway --quick` `#token=` QR | `P1.3a` | #624 | `-race` (`TestQueryTokenRejected`, `TestJobUIServesAXJobsWithoutWakeup`) + `GLUE_E2E_BROWSER=1` (`TestE2EFragmentMagicLinkLoginAndScrub`) |
| **3** | [#638](https://github.com/erain/glue/issues/638) `webui` conversational composer & **Run Autonomously** | `P0.2b` | #629, #632, #637 | `GLUE_E2E_BROWSER=1` (`TestE2EInteractiveWorkspaceChatAndPromoteToGoal` at desktop + `390x844`) |
| **3** | [#630](https://github.com/erain/glue/issues/630) TUI `/jobs` mission control (scroll/diff/answer/steer/PR) | `P1.3b` | #627 | `-race` (`TestJobsPanelScrollAndDiffToggle`, `TestJobsPanelAnswerQuestionOptionsAndText`, `TestJobsPanelOpenPRAndCancelConfirm`) |
| **3** | [#633](https://github.com/erain/glue/issues/633) `glue job teleport` + TUI `/teleport` | `P1.1b` | #625, #630, #637 | `-race` (`TestJobTeleportCleanCheckoutAndSessionBriefing`, `TestJobTeleportSnapshotAutoAppliesToDirtyTree`, `TestTUISlashTeleportAndJobsKeyTSwitchesSession`) |
| **4** | [#634](https://github.com/erain/glue/issues/634) `scripts/demo/anywhere.sh` + quickstart (**exit gate**) | `U1` | #624–#630, #632–#638 | `scripts/demo/aha.sh` + `scripts/demo/anywhere.sh` on `kind-glue-ax` + headless Chrome |
