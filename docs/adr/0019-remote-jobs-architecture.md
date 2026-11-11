# ADR-0019: Remote Jobs Architecture

## Status

Accepted, 2026-11-07. Implements the remote-mode direction of
[ADR-0018](0018-framework-coding-and-ax-direction.md). Tracked in
[#402](https://github.com/erain/glue/issues/402); the step-by-step plan is
[remote-mode-roadmap.md](../remote-mode-roadmap.md). Refined by
[ADR-0020](0020-ax-first-direct-execution.md), which adds direct Laptop-to-AX
CLI execution via `internal/axbackend`, defaults jobs to autonomous `auto`
policy, removes `glue-system` Kubernetes manifests, and makes `glue-gateway` an
optional out-of-cluster browser portal.

## Context

The owner wants to start a Glue coding job, disconnect, and pick it up later
from another machine or a browser, with jobs running either on the local
machine or as tasks on [AX](https://github.com/google/ax). The first showcase
is "use AX to develop AX"; the mechanism must work for any git repository.

Facts that shape the design (AX `241bc4d`, Substrate `v1.0.0-rc.1`, checked
2026-11-07):

- Glue's daemon keeps runs, events, and pending permissions in memory only;
  dropping the event stream cancels the run; goals are not exposed over HTTP;
  the coding shell runs with an empty environment.
- AX tasks have default-deny egress until a per-actor egress policy exists,
  and AX creates none. AX has no secret references: task env values are stored
  and displayed in plain text.
- Suspend keeps only the `/workspace` volume, may freeze the process without a
  signal, and resume restarts the task command. AX also runs the command once
  in a no-network "golden" pre-boot.
- The AX router accepts only mTLS from allowlisted service identities, wakes a
  suspended task on any request, and cuts idle streams after five minutes.
  Nothing suspends tasks automatically, and the command's exit status is not
  surfaced.

## Decision

1. **Jobs are the unit.** A job is a coding objective against a repository at
   a known base revision, run by the goal loop in its own workspace, with a
   permission policy, budgets, verify commands, and a delivery mode. Jobs live
   in a new `jobs` package and are hosted by `glue serve`.
2. **The whole job runs where the code is.** In remote mode, `glue serve`
   runs inside the AX task as the task command (port 8080) under the stock AX
   runner. Glue's core module does not import AX packages.
3. **Durable, detachable protocol (daemon protocol v2).** Runs, events, jobs,
   permissions, and results are written to SQLite in a state directory before
   they are visible. SSE `id:` is the sequence number; clients resume with
   `Last-Event-ID`; disconnecting never cancels. v1 is replaced without a
   compatibility shim (pre-1.0, ADR-0013).
4. **Crash-only state.** Job state is written continuously; a restarted
   daemon resumes started jobs from disk at iteration granularity. Startup is
   side-effect free; jobs start only through an explicit API call.
5. **One front door.** A separate Go module, `gateway/` (`glue-gateway`), is
   the only AX-aware component. It owns the job registry and event cache,
   serves the browser UI, routes the job API to a local daemon or an AX task
   (mTLS `CONNECT` through the router), creates per-task egress policies,
   suspends tasks that wait for a human, and resumes them on demand. Read-only
   views are served from its cache so they never wake a task.
6. **Reviewable results.** The job itself runs verify commands, commits,
   pushes `glue/<job-id>`, and opens a draft PR against the repository's
   upstream (the parent when `origin` is a fork). A completion record is
   stored by the job and cached by the gateway. Glue never merges.
7. **Credentials.** Development on a local single-user kind cluster may use
   plain task env. Before public exposure, the gateway pushes credentials to
   the task at runtime (memory only) and Glue redacts secrets before
   persisting anything. Switch to AX-native secrets once AX provides them.
8. **Access.** Public access goes through Cloudflare Tunnel to the gateway;
   browsers sign in with GitHub OAuth and a login allowlist; the CLI uses
   personal API tokens.
9. **Defaults.** Coding work defaults to `gemini` / `gemini-3.8-flash`; all
   other use defaults to `openrouter` / `openrouter/free`.

## Alternatives considered

- **Remote shell executor only:** leaves files, git, and the agent local;
  rejected in ADR-0018.
- **Embed the AX runner in Glue (serve port 80):** possible because the runner
  port is configurable, but couples the core module to AX's Go version and
  internals. The stock runner plus arbitrary-port ingress needs no AX code.
- **Talk to tasks directly from the CLI and browser:** browsers cannot present
  the router's mTLS identity or set its routing header, and every request
  would wake suspended tasks.
- **Wait for AX-native gateways, secrets, and idle suspend:** tracked upstream
  ([google/ax#549](https://github.com/google/ax/issues/549), [google/ax#449](https://github.com/google/ax/issues/449), [google/ax#420](https://github.com/google/ax/issues/420)); the gateway keeps these
  concerns in one replaceable place until then.

## Consequences

- The daemon gains a state directory and SQLite schema; `glue connect` and
  all clients move to protocol v2.
- The gateway is a second Go module with its own Go version; CI builds both.
- Local mode keeps working with no cluster: `glue serve` hosts jobs and the
  same browser UI.
- Glue's needs become concrete feedback for AX (interactive clients, task
  outcome, auto-suspend, secrets, gateways).
