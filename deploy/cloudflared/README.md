# Cloudflare Tunnel Deployment (`deploy/cloudflared`)

Host-mode `systemd` unit for exposing the optional `glue-gateway` browser and mobile portal publicly over HTTPS via a named [Cloudflare Tunnel](https://developers.cloudflare.com/cloudflare-one/connections/connect-networks/) without opening inbound ports on the host or deploying Kubernetes manifests.

See [`docs/runbooks/public-gateway.md`](../../docs/runbooks/public-gateway.md) for setup, verification, secret rotation, and emergency shutoff instructions.

## Files

- **`glue-gateway.service`**: Host-mode `systemd` unit running `/usr/local/bin/glue-gateway --config /etc/glue-gateway/gateway.yaml` on loopback (`127.0.0.1:8800`) outside Kubernetes (configured with `type: ax` or `type: local` backends) alongside a host `cloudflared` service.
