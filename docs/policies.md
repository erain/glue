# Permission Policy Profiles

Glue permission policies classify tool permission requests into `allow`, `deny`, or `ask`:

- `allow` executes the tool call without prompting.
- `deny` rejects the tool call immediately with a permission error.
- `ask` pauses the run or job (`waiting_for_input`) and waits for an operator decision.

## Built-in Profiles

| Profile | Default | File Writes (`write_file`, `edit_file`) | Shell Execution (`shell_exec` / `exec`) | Other Permission-Requiring Tools |
| :--- | :--- | :--- | :--- | :--- |
| `ask-all` | `ask` | `ask` | `ask` | `ask` |
| `workspace-auto` | `ask` | `allow` when confined to the workspace (`SafeJoin` + symlink check); `ask` outside or via symlink escape | `ask` for `git push`, `git remote`, `git config --global`, `curl`, `wget`, `ssh`, `scp`; `allow` for allowlisted binaries inside the workspace; `ask` otherwise | `ask` |
| `yolo` | `allow` | `allow` | `allow` | `allow` |

## `workspace-auto` Rule Order

Rules in `workspace-auto` are evaluated top-to-bottom; the first matching rule wins:

| Order | Rule Name | Tool / Action | Target / `argv_prefix` | Decision |
| :--- | :--- | :--- | :--- | :--- |
| 1 | `workspace-write` | `write_file` | `*` (confined to workspace) | `allow` |
| 2 | `workspace-edit` | `edit_file` | `*` (confined to workspace) | `allow` |
| 3 | `ask-git-push` | `exec` | `["git", "push"]` | `ask` |
| 4 | `ask-git-remote` | `exec` | `["git", "remote"]` | `ask` |
| 5 | `ask-git-config---global` | `exec` | `["git", "config", "--global"]` | `ask` |
| 6 | `ask-curl` | `exec` | `["curl"]` | `ask` |
| 7 | `ask-wget` | `exec` | `["wget"]` | `ask` |
| 8 | `ask-ssh` | `exec` | `["ssh"]` | `ask` |
| 9 | `ask-scp` | `exec` | `["scp"]` | `ask` |
| 10+ | `allow-exec-<bin>` | `exec` | Allowlisted binary prefix (`git`, `go`, `make`, `rg`, `sed`, `awk`, `python3`, `pytest`, `node`, `npm`, `cargo`, `sh`, `bash`, `ls`, `cat`, `head`, `tail`, `wc`, `find`, `grep`, `sort`, `uniq`, `diff`, `patch`, `jq`, `yq`) | `allow` |
| Fallback | `default` | Any | Any | `ask` |

## Custom JSON Policy Files

Custom policies use strict JSON decoding (`DisallowUnknownFields`):

```json
{
  "name": "repo-ci",
  "default": "ask",
  "rules": [
    {"name": "deny-secrets", "tool": "write_file", "target": "secrets/*", "decision": "deny"},
    {"name": "allow-writes", "tool": "write_file", "target": "*", "decision": "allow"},
    {"name": "deny-push", "action": "exec", "argv_prefix": ["git", "push"], "decision": "deny"},
    {"name": "allow-go-test", "action": "exec", "argv_prefix": ["go", "test"], "decision": "allow"}
  ]
}
```

### Validation and Workspace Confinement

- `default` and every rule `decision` must be `"allow"`, `"deny"`, or `"ask"`.
- Unknown JSON fields, trailing JSON values, malformed `target` globs (`path.Match`), and empty `argv_prefix` tokens are rejected at load time.
- File targets and `exec` working directories are resolved relative to the workspace with `tools/fs.SafeJoin` and `filepath.EvalSymlinks` across existing ancestor directories; paths outside the workspace or traversing symlinks outside the workspace never match workspace `allow` rules.

## Runtime Enforcement and `permission_auto` Auditing

- `glue serve` accepts `--policy <ask-all|workspace-auto|yolo>` (default `workspace-auto`) or `--policy-file <path>` (mutually exclusive) and applies the selected policy to both `/v1/sessions/{id}/runs` (`daemon.NewPermissionPolicy`) and hosted jobs (`jobs.Options.Policy`). Selecting `yolo` logs a loud warning to `stderr`.
- Jobs default empty `Spec.Policy` to `workspace-auto` (or the manager's configured policy name) and reject unknown policy names at creation time (`400 invalid_request`).
- Every automatic `allow` or `deny` decision emits a durable `permission_auto` event (`{tool, target, decision, rule}`) without transitioning the job to `waiting_for_input`; `ask` emits `permission_request` and `job_waiting`.
