# v4 to v5 Migration Runbook

The v5 release replaces the v4 admin listener (port 8081, plain HTTP `/healthz`,
no authentication) with a TLS-protected API listener (port 8888,
`/v1/admin/health` requiring the `X-Vpntunnel-Token` header). The config schema
is backwards-incompatible: the `admin` block is gone and the new `api` block is
required. Both the daemon and the deploy workflow need changes before the first
v5 deploy.

Complete every host step before pushing the v5 tag. If the host token files do
not exist when the v5 daemon starts it will refuse to start.

## Required host changes

Run these steps on the production host as root (the systemd unit runs as
`User=root`).

### 1. Stop the running v4 unit

```sh
systemctl stop vpntunnel
```

### 2. Generate the three API token files

Each token must be 64-512 bytes (after trimming whitespace), mode 0600. Use any
cryptographically strong source. These are opaque bearer tokens — the exact byte
content does not matter beyond length and uniqueness.

```sh
install -d -m 0700 /opt/vpntunnel/auth
for role in proxy admin deploy; do
  head -c 80 /dev/urandom | base64 | tr -d '\n' > /opt/vpntunnel/auth/${role}_token
  chmod 0600 /opt/vpntunnel/auth/${role}_token
done
```

Verify:

```sh
ls -l /opt/vpntunnel/auth/
# -rw------- 1 root root 108 ...  admin_token
# -rw------- 1 root root 108 ...  deploy_token
# -rw------- 1 root root 108 ...  proxy_token
```

The three tokens must be DISTINCT. The snippet above generates them
independently, so they will be different.

### 3. Create the TLS cert directory

The daemon generates `cert.pem` and `key.pem` under this directory on first
startup. The directory must exist and be writable by root.

```sh
install -d -m 0700 /opt/vpntunnel/tls
```

### 4. Update proxy.json on the host

Replace the v4 `admin` block with the v5 `api` block. The `admin` key must not
appear in the file. See `configs/proxy.example.json` in this repository for the
full v5 shape. Minimum required changes:

```json
{
  "upstream": {
    "configs": ["..."],
    "active": "random"
  },
  "api": {
    "listen": "127.0.0.1:8888",
    "shutdown_timeout": "5s",
    "tls": {
      "hostname": "vpntunnel.local",
      "cert_dir": "/opt/vpntunnel/tls/",
      "ip_sans": []
    },
    "auth": {
      "proxy_token_file": "/opt/vpntunnel/auth/proxy_token",
      "admin_token_file": "/opt/vpntunnel/auth/admin_token",
      "deploy_token_file": "/opt/vpntunnel/auth/deploy_token"
    },
    "max_request_body_bytes": 10485760,
    "upstream_timeout": "30s",
    "max_upstream_timeout": "5m"
  },
  "health": {
    "handshake_max_age": "180s"
  }
}
```

Fields to set for production:
- `api.tls.hostname`: the hostname in the self-signed cert's SAN. For a
  loopback-only API this can be any stable string (e.g. `vpntunnel.local`).
- `api.tls.cert_dir`: absolute path to the directory created in step 3.
- `api.auth.*_token_file`: absolute paths to the files created in step 2.

### 5. Add DEPLOY_TOKEN to the PRIME GitHub Actions environment

Read the value from the host file:

```sh
cat /opt/vpntunnel/auth/deploy_token
```

Copy the exact output (no trailing newline) and set it as the `DEPLOY_TOKEN`
secret in the repository's PRIME environment:
`GitHub repo → Settings → Environments → PRIME → Secrets → DEPLOY_TOKEN`.

Critical: the host file and the GitHub secret must hold the SAME value. Drift
between them causes every CI deploy to fail at the post-deploy healthcheck while
the daemon itself stays up. Rotation must update BOTH in lockstep (see Token
rotation below).

### 6. Start the v5 unit and verify

```sh
systemctl start vpntunnel
systemctl status vpntunnel
journalctl -u vpntunnel -n 50
```

The startup log should include lines for each loaded tunnel, the TLS cert
fingerprint, and confirmation that the API token files were loaded.

## Verify connectivity from the host

Use the admin token (does not appear in the access log query string — it is a
header):

```sh
# write the token to a temp file to avoid it appearing in shell history
cat /opt/vpntunnel/auth/admin_token > /tmp/.tok
curl -fsSk -H "X-Vpntunnel-Token: $(cat /tmp/.tok)" \
  https://127.0.0.1:8888/v1/admin/health
shred -u /tmp/.tok
```

A successful response is `{"status":"ok","tunnels":[...]}` with HTTP 200.

Note on `-k`: the API listener serves a self-signed certificate. The `-k` flag
disables TLS verification on this loopback call. This is acceptable for a
localhost-only diagnostic call — do not use `-k` for any external-facing curl
invocation.

## Token rotation

To rotate one of the three tokens (proxy / admin / deploy):

1. Generate a new token on the host:
   ```sh
   head -c 80 /dev/urandom | base64 | tr -d '\n' > /opt/vpntunnel/auth/${role}_token
   chmod 0600 /opt/vpntunnel/auth/${role}_token
   ```
2. If rotating `deploy_token`, update the `DEPLOY_TOKEN` secret in the PRIME
   GitHub environment with the new value BEFORE restarting the unit. The CI
   healthcheck runs AFTER the daemon restarts — if the secret does not match
   the new file value, the post-deploy healthcheck will fail.
3. Restart the unit: `systemctl restart vpntunnel`.

The daemon reads token files at startup only. A restart is required for any
token change to take effect.

## Rollback to v4

The v5 release is backwards-incompatible with v4 (config schema changed, admin
listener removed). To roll back:

1. `systemctl stop vpntunnel`
2. Point the live symlink at the previous versioned binary:
   ```sh
   ln -sf vpntunnel.v4.X.Y /opt/vpntunnel/vpntunnel
   ```
3. Restore the v4 `proxy.json` from backup (the `admin` block, no `api` block).
4. `systemctl start vpntunnel`

The token files and TLS directory created during this migration are ignored by
v4 and can be left in place.
