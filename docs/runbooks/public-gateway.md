# Runbook: Public `glue-gateway` via Cloudflare Tunnel (`deploy/cloudflared`)

This runbook covers exposing `glue-gateway` at `https://<your-host>` through a named [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) with GitHub OAuth allowlist sign-in, HSTS, request body size limits, server timeouts, and redacted access logs — without opening any inbound ports on the host.

## 1. Create the Cloudflare Tunnel and DNS Record

```sh
export GLUE_PUBLIC_HOST="glue.example.com"
cloudflared tunnel login
cloudflared tunnel create glue-gateway
cloudflared tunnel route dns glue-gateway "${GLUE_PUBLIC_HOST}"
export TUNNEL_TOKEN="$(cloudflared tunnel token glue-gateway)"
```

## 2. Create the GitHub OAuth App

In GitHub **Settings -> Developer settings -> OAuth Apps -> New OAuth App**:

- **Application name:** `Glue Gateway`
- **Homepage URL:** `https://<your-host>`
- **Authorization callback URL:** `https://<your-host>/auth/github/callback`

Record the **Client ID** (`GLUE_GITHUB_CLIENT_ID`) and generate a **Client secret** (`GLUE_GITHUB_CLIENT_SECRET`).

## 3. Configure and Deploy

### Option A: In-Cluster Deployment (`glue-system` on Kubernetes / `kind`)

1. Create or update the gateway and tunnel secrets in `glue-system`:
   ```sh
   kubectl -n glue-system create secret generic glue-gateway-secrets \
     --from-literal=GLUE_GITHUB_CLIENT_ID="${GLUE_GITHUB_CLIENT_ID}" \
     --from-literal=GLUE_GITHUB_CLIENT_SECRET="${GLUE_GITHUB_CLIENT_SECRET}" \
     --from-literal=GLUE_ALLOWED_LOGINS="your-github-login" \
     --from-literal=GLUE_GITHUB_TOKEN="${GLUE_GITHUB_TOKEN}" \
     --from-literal=GEMINI_API_KEY="${GEMINI_API_KEY}" \
     --dry-run=client -o yaml | kubectl apply -f -

   kubectl -n glue-system create secret generic cloudflared-credentials \
     --from-literal=token="${TUNNEL_TOKEN}" \
     --dry-run=client -o yaml | kubectl apply -f -
   ```
2. Set `public_url: https://<your-host>`, `trust_forwarded_proto: true`, and `auth.mode: github` in `ConfigMap/glue-gateway-config`, then apply both manifests:
   ```sh
   kubectl -n glue-system rollout restart deployment/glue-gateway
   envsubst '${GLUE_PUBLIC_HOST}' < deploy/cloudflared/manifests.yaml | kubectl apply -f -
   kubectl -n glue-system rollout status deployment/glue-gateway deployment/cloudflared --timeout=60s
   ```

### Option B: Host-Mode Alternative (`systemd` + Local Backend)

When running `glue-gateway` directly on the host outside Kubernetes against a local `glue serve` daemon (`127.0.0.1:8787`):

1. Write `/etc/glue-gateway/gateway.yaml` with `listen: 127.0.0.1:8800`, `public_url: https://<your-host>`, `trust_forwarded_proto: true`, `auth.mode: github`, and `allowed_logins: [your-github-login]`.
2. Store `GLUE_GITHUB_CLIENT_ID`, `GLUE_GITHUB_CLIENT_SECRET`, and backend tokens in `/etc/glue-gateway/gateway.env` (`chmod 0600`).
3. Install `deploy/cloudflared/glue-gateway.service` to `/etc/systemd/system/glue-gateway.service`, configure host `cloudflared` ingress to route `https://<your-host>` to `http://127.0.0.1:8800`, and start both services:
   ```sh
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

On the host machine, verify no new listener is bound to a public interface:

```sh
ss -tlnp
```

In a browser:

1. Sign in with an allowlisted GitHub account -> lands on the **Jobs** list (`#/`) with an `HttpOnly; Secure; SameSite=Lax` `glue_session` cookie.
2. Sign in with a non-allowlisted GitHub account -> receives `403 Forbidden` (**Access Denied** page) and no session cookie.
3. Restart recovery: `kubectl -n glue-system rollout restart deployment/cloudflared deployment/glue-gateway` (or `sudo systemctl restart cloudflared glue-gateway`) recovers automatically without manual intervention.

## 5. Rotate Secrets

- **GitHub OAuth client secret:** Generate a new secret in the GitHub OAuth App settings, update `GLUE_GITHUB_CLIENT_SECRET` in `glue-gateway-secrets` (or `/etc/glue-gateway/gateway.env`), restart `glue-gateway`, then delete the old secret in GitHub.
- **Cloudflare Tunnel token:** Run `cloudflared tunnel token glue-gateway` (or rotate credentials in the Cloudflare dashboard), update `cloudflared-credentials`, and run `kubectl -n glue-system rollout restart deployment/cloudflared`.
- **Personal API tokens & browser sessions:** Revoke individual CLI tokens from `#/tokens` (`DELETE /v1/tokens/{id}`), or clear all active browser sessions with `sqlite3 /var/lib/glue-gateway/events.sqlite 'DELETE FROM sessions;'`.

## 6. Take It Offline Quickly (One Command)

- **In-cluster:**
  ```sh
  kubectl -n glue-system scale deployment/cloudflared --replicas=0
  ```
- **Host-mode (`systemd`):**
  ```sh
  sudo systemctl stop cloudflared
  ```

## 7. Optional Second Factor: Cloudflare Access

For defense in depth ahead of the gateway's own GitHub OAuth check, create a **Self-hosted Application** in **Cloudflare Zero Trust -> Access -> Applications** for `<your-host>` with an **Allow** policy restricted to your email or GitHub identity. Cloudflare edges then require Access authentication before forwarding any request through the tunnel to `glue-gateway`.
