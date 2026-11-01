# VPNTunnel

Lightweight forward HTTP proxy that routes every byte of egress through a
**userspace WireGuard tunnel** — no root, no `setcap`, no host-level VPN.
One static binary, systemd service.

```
   ┌────────┐  HTTP / CONNECT       ┌─────────────┐   WireGuard    ┌──────────┐
   │ client │ ─── :7788 ─────▶───   │  vpntunnel  │ ─── userspace ─│  exit IP │
   │  curl, │                       │   daemon    │ ───────▶─────  │ (any WG  │
   │ browser│                       │  (no root)  │                │ provider)│
   │   app  │                       └─────────────┘                └──────────┘
   └────────┘                              │
                   GET /v1/admin/health :8888 (loopback, token required)
```

**What it is.** A forward proxy speaking `Proxy-Authorization: Bearer` auth
(RFC 7235), accepting plain HTTP forward (`GET http://...`) and HTTPS tunnel
(`CONNECT host:port`), dialing every upstream through a wg-quick-format
WireGuard tunnel.

**What it is not.** A TLS-terminating proxy, transparent proxy, mitmproxy,
PAC-aware router, or anything that inspects payload bytes. CONNECT traffic
is opaque.

## Features

- **Userspace WireGuard egress** — wireguard-go + gVisor netstack. No root, no
  `setcap`, no kernel module, no host-level VPN.
- **Multi-tunnel catalog** — drop any number of wg-quick `.conf` files into
  `configs/tunnels/`; they are auto-discovered at startup and exposed as a
  country-grouped catalog over the API. Stable, non-secret tunnel ids are
  derived by HMAC-SHA256 over the file name.
- **Always-on streaming exit** — one supervised WireGuard device stays up with
  exponential-backoff auto-reconnect, optionally restricted to a country
  allow-list, and rotatable on demand without a restart (`POST /v1/admin/rotate`).
- **On-demand per-zone exits** — the API brings a second exit up for a specific
  zone on request and tears it down when idle, staying inside the provider's
  device budget via break-before-make + settle.
- **Two listeners, two roles** — a forward-proxy port (HTTP/CONNECT) and an API
  port (health, tunnel catalog, proxy routing) guarded by role-based Bearer
  tokens (`admin`, `proxy`).
- **Health endpoint** — aggregated multi-tunnel readiness at
  `GET /v1/admin/health`: `ok` / `degraded` / `down` with per-tunnel handshake age.
- **Optional Telegram notifications** on tunnel changes (startup, reconnect,
  on-demand zone switches).
- **Single static binary** (`CGO_ENABLED=0`) deployed as a systemd service on an
  immutable, self-healing release layout — see [DEPLOY.md](./DEPLOY.md).

## Quick start

**As a client** — the proxy is already running somewhere; you have a URL and a
Bearer token from whoever operates it:

```bash
curl --proxy http://<proxy-host>:7788 \
     --proxy-header "Proxy-Authorization: Bearer <your-token>" \
     https://api.ipify.org
# prints the exit IP — the WireGuard exit's address, not your own
```

**As an operator** — bring it up locally to try it out. Tunnels are
auto-discovered: drop a wg-quick `.conf` into `./configs/tunnels/` and it is
picked up on the next startup. No entry in `proxy.json` is needed.

```bash
# 1. Get a wg-quick .conf from any WireGuard provider (or your own peer).
mkdir -p ./configs/tunnels ./configs/auth ./logs
mv ~/Downloads/se-sto-wg-001.conf ./configs/tunnels/
chmod 0600 ./configs/tunnels/*.conf

# 2. Generate the tokens. The forward-proxy token is optional; the two API
#    tokens are required and MUST be mode 0600, owned by you, 64–512 bytes.
openssl rand -hex 48 > ./configs/auth/token          # forward-proxy (optional)
openssl rand -hex 48 > ./configs/auth/proxy_token    # API: proxy role
openssl rand -hex 48 > ./configs/auth/admin_token    # API: admin role
chmod 0600 ./configs/auth/token ./configs/auth/proxy_token ./configs/auth/admin_token

# 3. Write ./configs/proxy.json. Only the `api` block is mandatory; everything
#    else takes defaults. See configs/proxy.example.json for the full schema.
```

```json
{
  "vpnstream": {
    "listen": "127.0.0.1:7788",
    "auth": { "token_file": "./auth/token" }
  },
  "api": {
    "listen": "127.0.0.1:8888",
    "auth": {
      "proxy_token_file": "./auth/proxy_token",
      "admin_token_file": "./auth/admin_token"
    }
  }
}
```

```bash
# 4. Build and run. Locally the API serves plain HTTP (no TLS flags needed);
#    production adds -tls-cert-dir to serve HTTPS — see DEPLOY.md.
make build && ./build/vpntunnel -config ./configs/proxy.json
```

On first run the daemon generates `./configs/auth/tunnel-id.key` (64 random
bytes, mode 0600) if it is absent. Treat it like a private key — see
[Security](#security). For a remote-server deployment via systemd + GitHub
Actions, see [DEPLOY.md](./DEPLOY.md).

## Using the proxy (as a client)

You need the proxy URL (`http://<host>:7788`) and a Bearer token, both from the
operator. The auth header is **always** `Proxy-Authorization` — never
`Authorization`, which would travel to the upstream origin and leak the token.

```bash
# HTTPS via CONNECT, or HTTP via forward proxy — same invocation, different scheme
curl --proxy http://<host>:7788 \
     --proxy-header "Proxy-Authorization: Bearer <token>" \
     https://example.com
```

Use `--proxy-header`, not `-H`: `-H` puts the header on the **forwarded
request** and leaks the token to the origin. The same rule holds in code — set
the header on the proxy/CONNECT request, not the origin request:

- **Go (`net/http`):** `http.Transport{Proxy: http.ProxyURL(u), ProxyConnectHeader: http.Header{"Proxy-Authorization": {"Bearer <token>"}}}`. `ProxyConnectHeader` is the load-bearing field.
- **Python:** `httpx.Client(proxy="http://<host>:7788", headers={"Proxy-Authorization": "Bearer <token>"})`. `httpx` handles CONNECT header delivery cleanly; `requests` has historically been quirky here.

Browsers, the macOS system proxy, and `HTTP(S)_PROXY` env vars generally have
**no** UI for Bearer proxy auth — either disable auth on the proxy and rely on
network-level access control (loopback, SSH tunnel, Tailscale), or use a
header-injecting extension (e.g. FoxyProxy "Custom Header").

**Troubleshooting.**

| Symptom | Probable cause | Fix |
|---|---|---|
| `407 Proxy Authentication Required` | wrong/missing token, or sent as `Authorization` instead of `Proxy-Authorization` | check the token; use `--proxy-header` / `ProxyConnectHeader` |
| `Connection refused` on the proxy port | daemon down, or listening on loopback while you connect remotely | `journalctl -u vpntunnel -n 200 --no-pager`; check `listen` |
| Exit IP is your real IP | proxy URL points at the wrong host, or upstream isn't routing | confirm via `curl --proxy http://<host>:7788 --proxy-header "Proxy-Authorization: Bearer <token>" https://api.ipify.org` |
| HTTPS hangs ~30s then times out | WireGuard handshake stale or peer unreachable | query the health endpoint (below); a `down` status means the tunnel is dead |

For tunnel-level state, query the health endpoint with the admin token. On the
production host the API serves HTTPS with a self-signed cert, so pass `-k`; a
local dev daemon without `-tls-cert-dir` serves plain HTTP (drop the `-k`, use
`http://`):

```bash
curl -k -sS \
  -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" \
  https://<host>:8888/v1/admin/health | jq .
# 200 {"status":"ok",      "tunnels":[{"id":"se-sto","healthy":true, "handshake_age_seconds":42}]}
# 200 {"status":"degraded","tunnels":[{...healthy:true...},{...healthy:false...}]}
# 503 {"status":"down",    "tunnels":[{"id":"se-sto","healthy":false,"handshake_age_seconds":312}]}
```

Unlike the proxy listener, the API server has **no loopback bypass** — every
request must carry `X-Vpntunnel-Token`. (The `$(cat ...)` puts the token in your
shell history; pass it via an env var if that matters.)

## Configuration

`proxy.json` is the only configuration file. The **`api` block is required**;
every other top-level block is optional and falls back to defaults. Relative
paths (token files, tunnel-id key, `tunnels/`) resolve against the directory
containing `proxy.json`; `access_log.path` resolves against the working
directory, so prefer an absolute path there. The full canonical schema —
including the on-demand-VPN (`api.vpn`) and reconnect/timeout knobs — lives in
[`configs/proxy.example.json`](./configs/proxy.example.json).

The knobs an operator typically sets:

| Field | Default | Purpose |
|---|---|---|
| `vpnstream.listen` | `127.0.0.1:7788` | Forward-proxy listener (HTTP/CONNECT) |
| `vpnstream.allowed_countries` | `[]` (all) | Two-letter ISO codes the streaming tunnel may pick from |
| `vpnstream.auth.token_file` | empty | Forward-proxy Bearer token (optional; or inline `vpnstream.auth.token`) |
| `api.listen` | `127.0.0.1:8888` | API listener (health, tunnel catalog, proxy routing) |
| `api.auth.proxy_token_file` | **required** | Token file for the `proxy` role |
| `api.auth.admin_token_file` | **required** | Token file for the `admin` role |
| `tunnel_id_hmac_key_file` | `./auth/tunnel-id.key` | HMAC key deriving stable tunnel ids (auto-generated if absent) |
| `access_log.path` | `./logs/access.log` | Rotating JSONL access log |

A few things that are **not** in `proxy.json`, or that bite if missed:

- **Tunnel auto-discovery.** Tunnels are never listed in `proxy.json`; the
  daemon scans `<config-dir>/tunnels/` for top-level `*.conf` files at startup.
  A missing or empty `tunnels/` directory is a startup error; unparseable
  `.conf` files are warn-skipped, not fatal. Any wg-quick `.conf` works — any
  WireGuard provider or your own peer.
- **TLS is set by CLI flags, not `proxy.json`.** `-tls-cert-dir` (empty → plain
  HTTP, loopback/dev only; a path → HTTPS 1.3 with a self-signed cert the daemon
  manages), `-tls-hostname` (default `localhost`), `-tls-ip-sans`
  (comma-separated). Production passes `-tls-cert-dir`; locally the API is HTTP.
- **Optional Telegram notifications** via the `VPNTUNNEL_TELEGRAMBOT_DSN` env
  var (`tbot://<adminChatID>:@<botToken>/`). Unset disables them; a malformed
  DSN only warns and disables — it never blocks startup. See
  [`configs/env.example`](./configs/env.example).

## Deploying

One static binary, deployed to a single host as a systemd unit. Every `v*` tag
uploads a fresh binary into an immutable per-version artifact store, atomically
flips the `release` channel symlink, restarts the unit, and self-heals back to
the previous version if the restart or `/v1/admin/health` check fails. Pushes to
`main` and PRs run lint + tests only. Full operator guide — host bootstrap,
GitHub Environment secrets, exit rotation, rollback, and the log streams — is in
**[DEPLOY.md](./DEPLOY.md)**.

## Observability

The daemon emits an operational log (stdout → journald) and a rotating JSONL
access log (via lumberjack). Neither ever contains a Bearer token,
`Authorization`, or `Proxy-Authorization` field. Health is exposed at
`GET /v1/admin/health`. Log shapes and the full health-body contract are in
[DEPLOY.md](./DEPLOY.md#observability).

## Security

`vpntunnel` hides your egress IP from upstream origins by routing every byte
through a WireGuard exit. It does **not** encrypt the client → proxy hop (use it
on loopback, an SSH tunnel, or a Tailscale/WireGuard overlay), does **not**
prevent client-side DNS leaks, and does **not** defend against traffic analysis
by your WireGuard provider.

- **Loopback bypass (forward proxy only).** Clients reaching the forward proxy
  from `127.0.0.1`/`::1` skip the Bearer challenge. If you change
  `vpnstream.listen` to a non-loopback address, you **must** also configure
  `vpnstream.auth`, or the proxy is open to any network client. The API listener
  has no such bypass.
- **Secrets discipline.** WireGuard `.conf` files, Bearer/API tokens, and the
  tunnel-id HMAC key all live under `./configs/` at mode `0600`, gitignored. The
  two API token files are rejected at startup unless they are exactly `0600` and
  owned by the running user. None of this material ever appears in logs, errors,
  or response bodies. Back up `tunnel-id.key` like a private key — overwriting it
  silently rotates every derived tunnel id.
- Token comparison uses `crypto/subtle.ConstantTimeCompare` — no length oracle,
  no early exit.

Full threat model, supported versions, and disclosure procedure are in
[`SECURITY.md`](./SECURITY.md). Report vulnerabilities via a private GitHub
Security Advisory or `seilbekskindirov@gmail.com`; do not open public issues for
unpatched vulnerabilities.

## License

MIT. See [`LICENSE`](./LICENSE).
