# Cloudflare Tunnel Deployment (`deploy/cloudflared`)

Manifests and host-mode unit for exposing `glue-gateway` publicly over HTTPS via a named [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) without opening inbound ports on the host.

See [`docs/runbooks/public-gateway.md`](../../docs/runbooks/public-gateway.md) for the complete setup, verification, secret rotation, and emergency shutoff runbook.

## Files

- **`manifests.yaml`**: In-cluster `ConfigMap` (`cloudflared-config`) and `Deployment` (`cloudflared`) in namespace `glue-system`. Runs `cloudflare/cloudflared:2024.12.2` as non-root (`65532`) with a read-only root filesystem, reading `TUNNEL_TOKEN` from Secret `cloudflared-credentials` and routing `${GLUE_PUBLIC_HOST}` to `http://glue-gateway.glue-system.svc.cluster.local:80`.
- **`glue-gateway.service`**: Host-mode `systemd` unit for running `/usr/local/bin/glue-gateway --config /etc/glue-gateway/gateway.yaml` on loopback (`127.0.0.1:8800`) outside Kubernetes alongside a local `glue serve` backend and a host `cloudflared` service.
