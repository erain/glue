# Runbook: Public `glue-gateway` via Cloudflare Tunnel (`deploy/cloudflared`)

CLI users run coding jobs directly on AX via `glue --remote "<objective>"` without deploying a gateway ([ADR-0020](../adr/0020-ax-first-direct-execution.md)). Deploy `glue-gateway` when you want the mobile/desktop browser UI (`webui`), zero-prompt **Cloudflare Access** Zero Trust sign-in, personal API tokens, or background idle-wait AX task suspension exposed at `https://<your-host>` through a named [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) — either as an opt-in 1-pod `Deployment` in `glue-gateway` via `glue remote setup` ([ADR-0021](../adr/0021-opt-in-cluster-gateway-cloudflare-access.md), 0 AX worker slots consumed while idle) or out-of-cluster on a host/VM.

## 1. Create the Cloudflare Tunnel and DNS Record

```sh
export GLUE_PUBLIC_HOST="glue.example.com"
cloudflared tunnel login
cloudflared tunnel create glue-gateway
cloudflared tunnel route dns glue-gateway "${GLUE_PUBLIC_HOST}"
export TUNNEL_TOKEN="$(cloudflared tunnel token glue-gateway)"
```

## 2. Create the Cloudflare Access Application (Zero OAuth App Setup)

In **Cloudflare Zero Trust -> Access -> Applications -> Add an application -> Self-hosted**, protect `glue.example.com` with an **Allow** policy restricted to your email or identity provider group (Google, GitHub, or Passkey SSO), and record your **Team domain** (`<team>.cloudflareaccess.com` -> `GLUE_CF_ACCESS_TEAM_DOMAIN`) and **Application Audience (AUD) Tag** (`GLUE_CF_ACCESS_AUD`).

## 3a. One-Command In-Cluster Deployment (`glue remote setup`, Recommended)
```sh
glue remote setup --public-url "https://${GLUE_PUBLIC_HOST}" --cf-team myteam.cloudflareaccess.com --cf-aud "${GLUE_CF_ACCESS_AUD}" --allow-email you@example.com --tunnel-token "${TUNNEL_TOKEN}"
glue remote status && glue remote down --purge-pvc   # inspect health or tear down cleanly
```
## 3b. Alternative: Host or VM Deployment (`systemd` + Pure `type: ax`)

1. Write `/etc/glue-gateway/gateway.yaml` (`KnownFields(true)` strict YAML; omit `kube_config`/`kube_context` to use ambient host configuration, or add a `type: local` entry when fronting a local `glue serve` daemon):
   ```yaml
   listen: 127.0.0.1:8800
   public_url: https://glue.example.com
   trust_forwarded_proto: true
   state_dir: /var/lib/glue-gateway
   auth:
     mode: cloudflare-access
     allowed_emails: [you@example.com]
   default_backend: ax
   backends:
     - name: ax
       type: ax
       atespace: default
       image: ghcr.io/your-org/glue-task@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
       kube_config: /etc/glue-gateway/kubeconfig
       kube_context: your-ax-context
   ```
2. Store Cloudflare Access and runtime task credentials in `/etc/glue-gateway/gateway.env` (`chmod 0600`, never stored in AX `Task.spec.env`):
   ```sh
   GLUE_CF_ACCESS_TEAM_DOMAIN=myteam.cloudflareaccess.com
   GLUE_CF_ACCESS_AUD=...
   GLUE_GITHUB_TOKEN=...
   GEMINI_API_KEY=...
   ```
3. Install [`deploy/cloudflared/glue-gateway.service`](../../deploy/cloudflared/glue-gateway.service) to `/etc/systemd/system/glue-gateway.service`, configure the host `cloudflared` service to forward `https://<your-host>` to `http://127.0.0.1:8800`, and start both services:
   ```sh
   sudo install -m 0644 deploy/cloudflared/glue-gateway.service /etc/systemd/system/glue-gateway.service
   sudo systemctl daemon-reload
   sudo systemctl enable --now glue-gateway cloudflared
   ```

## 4. Security Verification Checklist

Run from an external network (for example, a phone on cellular or a remote VM):

```sh
curl -sI "https://${GLUE_PUBLIC_HOST}/" | head -1                        # HTTP/2 200 (login shell) or 302
curl -s -o /dev/null -w '%{http_code}\n' "https://${GLUE_PUBLIC_HOST}/v1/jobs"  # 401
curl -s "https://${GLUE_PUBLIC_HOST}/healthz"                            # {"ok":true} (no version or internal info)
curl -sI "https://${GLUE_PUBLIC_HOST}/" | grep -i strict-transport-security
```

On the host machine, verify no listener is bound to a public interface (`glue-gateway` listens only on `127.0.0.1:8800`):

```sh
ss -tlnp
```

In a browser:

1. Open `https://<your-host>` while signed in to Cloudflare Access -> lands directly on **Jobs** (`#/`, `Signed in as <email>`) with zero second login screen and an `HttpOnly; Secure; SameSite=Lax` `glue_session` cookie.
2. Sign in with an identity outside `allowed_emails` -> receives `403 Forbidden` and no session cookie.
3. Restart recovery: `sudo systemctl restart cloudflared glue-gateway` reloads `/var/lib/glue-gateway/events.sqlite` and reconnects the tunnel automatically.

## 5. Rotate Secrets

- **GitHub OAuth client secret:** Generate a new secret in the GitHub OAuth App settings, update `GLUE_GITHUB_CLIENT_SECRET` in `/etc/glue-gateway/gateway.env`, run `sudo systemctl restart glue-gateway`, then delete the old secret in GitHub.
- **Cloudflare Tunnel token:** Run `cloudflared tunnel token glue-gateway` (or rotate credentials in the Cloudflare dashboard), update the host `cloudflared` service token, and run `sudo systemctl restart cloudflared`.
- **Personal API tokens & browser sessions:** Revoke individual CLI tokens from `#/tokens` (`DELETE /v1/tokens/{id}`), or clear all active browser sessions with `sqlite3 /var/lib/glue-gateway/events.sqlite 'DELETE FROM sessions;'`.

## 6. Take It Offline Quickly (One Command)

```sh
sudo systemctl stop cloudflared
```

## 7. Alternative Without Cloudflare Access: GitHub OAuth (`auth.mode: github`)

When exposing `glue-gateway` without Cloudflare Access, register a GitHub OAuth App with callback `https://<your-host>/auth/github/callback` and set `auth.mode: github`, `allowed_logins`, `GLUE_GITHUB_CLIENT_ID`, and `GLUE_GITHUB_CLIENT_SECRET`.
