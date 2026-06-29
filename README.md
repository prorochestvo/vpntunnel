# VPNTunnel

Lightweight forward HTTP proxy that routes every byte of egress through a
**userspace WireGuard tunnel** — no root, no `setcap`, no host-level VPN.
One static binary, systemd service.

```
   ┌────────┐  HTTP / CONNECT       ┌─────────────┐   WireGuard    ┌──────────┐
   │ client │ ─── :7788 ─────▶───   │  vpntunnel  │ ─── userspace ─│ exit IP  │
   │  curl, │                       │   daemon    │ ───────▶─────  │ (Mullvad,│
   │ browser│                       │  (no root)  │                │  yours)  │
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

## Table of contents

- [Quick start](#quick-start)
- [Using the proxy (as a client)](#using-the-proxy-as-a-client)
  - [curl](#curl)
  - [Browser and system-wide](#browser-and-system-wide)
  - [Programmatic clients](#programmatic-clients)
  - [Troubleshooting](#troubleshooting)
- [Configuration reference](#configuration-reference)
- [Deploying (as an operator)](#deploying-as-an-operator)
- [Observability](#observability)
- [Security](#security)
- [License](#license)

## Quick start

**As a client** — proxy is already running somewhere; you have a URL and a
Bearer token from whoever operates it:

```bash
curl --proxy http://<proxy-host>:7788 \
     --proxy-header "Proxy-Authorization: Bearer <your-token>" \
     https://am.i.mullvad.net/json
# {"mullvad_exit_ip": true, "country": "Sweden", ...}
```

**As an operator** — bring it up locally to try it out. Tunnels are
auto-discovered: drop a wg-quick `.conf` into `./configs/tunnels/` and it is
picked up on the next startup. No entry in `proxy.json` is needed.

```bash
# 1. Get a wg-quick .conf from any WireGuard provider (or your own peer).
#    Mullvad: https://mullvad.net/account/wireguard-config
mkdir -p ./configs/tunnels ./configs/auth ./logs
mv ~/Downloads/se-sto-wg-001.conf ./configs/tunnels/
chmod 0600 ./configs/tunnels/*.conf

# 2. Generate the tokens. The forward-proxy token is optional; the two API
#    tokens are required and MUST be mode 0600, owned by you, 64–512 bytes.
openssl rand -hex 48 > ./configs/auth/token          # forward-proxy (optional)
openssl rand -hex 48 > ./configs/auth/proxy_token     # API: proxy role
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
#    production adds -tls-cert-dir to serve HTTPS — see Deploying.
make build && ./build/vpntunnel -config ./configs/proxy.json
```

On first run the daemon generates `./configs/auth/tunnel-id.key` (64 random
bytes, mode 0600) if it is absent. Treat it like a private key — see
[Security](#security).

For a remote-server deployment via systemd + GitHub Actions, see
[Deploying](#deploying-as-an-operator).

## Using the proxy (as a client)

You need two things: the proxy URL (`http://<host>:7788`) and a Bearer
token. Both come from the operator. The auth header is **always**
`Proxy-Authorization` — not `Authorization`, which would travel to the
upstream origin and leak the token.

### curl

```bash
# HTTPS via CONNECT
curl --proxy http://<host>:7788 \
     --proxy-header "Proxy-Authorization: Bearer <token>" \
     https://example.com

# HTTP via forward proxy
curl --proxy http://<host>:7788 \
     --proxy-header "Proxy-Authorization: Bearer <token>" \
     http://example.com
```

Use `--proxy-header`, not `-H`. The `-H` flag puts the header on the
**forwarded request**, leaking the token to the upstream origin.
`--proxy-header` puts it on the CONNECT/proxy request, where the proxy
expects it.

A shell alias for repeat use:

```bash
alias xcurl='curl --proxy http://<host>:7788 --proxy-header "Proxy-Authorization: Bearer $(cat ~/.vpntunnel.token)"'
xcurl https://am.i.mullvad.net/json
```

### Browser and system-wide

**Firefox** (`about:preferences#general → Network Settings → Manual proxy
configuration`):

- HTTP Proxy: `<host>` Port: `7788`
- "Also use this proxy for HTTPS": yes

Firefox has no UI for Bearer-style proxy auth. Options:

- Disable auth on the proxy (operator-side) and rely on network-level access
  control (loopback, SSH tunnel, Tailscale).
- Use the `FoxyProxy` extension with a "Custom Header" rule injecting
  `Proxy-Authorization`.

**macOS system proxy** (`System Settings → Network → <interface> → Details →
Proxies`): same limitation as Firefox. Only practical when auth is
disabled.

**Environment variables** (shell, most CLI tools):

```bash
export HTTPS_PROXY=http://<host>:7788
export HTTP_PROXY=http://<host>:7788
# Most tools that read these vars do NOT pass Proxy-Authorization
# automatically — either disable auth or use the tool's programmatic API.
```

### Programmatic clients

**Go (`net/http`):**

```go
proxyURL, _ := url.Parse("http://<host>:7788")

transport := &http.Transport{
    Proxy: http.ProxyURL(proxyURL),
    ProxyConnectHeader: http.Header{
        "Proxy-Authorization": []string{"Bearer <token>"},
    },
}
client := &http.Client{Transport: transport}

resp, _ := client.Get("https://example.com")
```

`ProxyConnectHeader` is the load-bearing field — it sets the header on the
CONNECT request to the proxy. A plain `req.Header.Set("Proxy-Authorization",
...)` would send it to the origin, not the proxy.

**Python (`httpx`):**

```python
import httpx

client = httpx.Client(
    proxy="http://<host>:7788",
    headers={"Proxy-Authorization": "Bearer <token>"},
)
r = client.get("https://example.com")
```

`requests` does work but has historically had quirks with CONNECT header
delivery across versions. `httpx` handles it cleanly. If you must use
`requests`, set the header on a `Session` and verify against the proxy's
access log that requests actually reach the proxy (any access-log line is
proof; absence means the header didn't make it through and the origin
saw your token).

### Troubleshooting

| Symptom | Probable cause | Fix |
|---|---|---|
| `407 Proxy Authentication Required` | wrong / missing Bearer token, or sent as `Authorization` instead of `Proxy-Authorization` | check the token; ensure `--proxy-header` (curl) or `ProxyConnectHeader` (Go) |
| `Connection refused` on the proxy port | daemon not running, or listening on loopback while you're connecting remotely | `journalctl -u vpntunnel -n 200 --no-pager`; check `listen` is bound where you expect |
| HTTPS hangs ~30s then times out | WireGuard handshake stale or peer unreachable | `curl -k -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" https://<host>:8888/v1/admin/health` — a 503 means the tunnel is down |
| Exit IP is your real IP, not the VPN | proxy URL points at the wrong host, or upstream isn't routing | confirm `am.i.mullvad.net/json` returns `mullvad_exit_ip: true` |
| Browser shows mixed-content warnings | proxy itself is plain HTTP (no TLS-termination) | this is expected; the tunnel **through** the proxy is encrypted (WireGuard), the client→proxy hop is not |
| `Proxy-Authorization` header appearing in upstream request logs | client sent it as `Authorization` or via a `-H`-equivalent | use the proxy-specific header API (`--proxy-header`, `ProxyConnectHeader`) |

For tunnel-level state, query the API health endpoint with the admin token in
the `X-Vpntunnel-Token` header. On the production host the API serves HTTPS with
a self-signed certificate, so pass `-k`; the admin token lives at
`/opt/vpntunnel/configs/auth/admin_token`. (A local dev daemon without
`-tls-cert-dir` serves plain HTTP — drop the `-k` and use `http://`.)
Note that the `$(cat ...)` substitution puts the token bytes in your shell
history — if that matters, pass it via an env var instead (`ADMIN_TOKEN=... curl ...`).

```bash
# -k because the production daemon serves a self-signed certificate
curl -k -sS \
  -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" \
  https://<host>:8888/v1/admin/health | jq .
# 200: {"status":"ok",    "tunnels":[{"id":"se-sto","healthy":true,"handshake_age_seconds":42}]}
# 200: {"status":"degraded","tunnels":[{"id":"se-sto","healthy":true,...},{"id":"de-ber","healthy":false,...}]}
# 503: {"status":"down",  "tunnels":[{"id":"se-sto","healthy":false,"handshake_age_seconds":312}]}
```

Unlike the proxy listener (`:7788`), the API server has **no loopback bypass** — every
request must carry the `X-Vpntunnel-Token` header regardless of where it originates.

Aggregation rules:

- `ok` — every tunnel in the pool is healthy (200).
- `degraded` — at least one tunnel is healthy and at least one is not (200).
- `down` — no tunnels are healthy, or the pool is empty (503).

## Configuration reference

`proxy.json` is the only configuration file. The **`api` block is required**;
every other top-level block is optional and falls back to defaults. Relative
paths to the token files, the tunnel-id key, and the `tunnels/` directory
resolve against the directory **containing `proxy.json`**; `access_log.path`
resolves against the process working directory, so prefer an absolute path
there. The full,
canonical schema — including the on-demand-VPN (`api.vpn`) and
reconnect/timeout knobs not shown here — lives in
[`configs/proxy.example.json`](./configs/proxy.example.json).

The table below covers only the knobs an operator typically sets:

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
| `operational.level` | `info` | slog level: `debug`/`info`/`warn`/`error` |
| `operational.format` | `text` | slog format: `text`/`json` |

**Tunnel auto-discovery.** Tunnels are not listed in `proxy.json`. The daemon
scans `<config-dir>/tunnels/` for top-level `*.conf` files at startup; drop a
wg-quick `.conf` in and restart. A missing or empty `tunnels/` directory is a
startup error. Unparseable `.conf` files are warn-skipped, not fatal.

**Forward-proxy auth is optional; the two API tokens are required.** If
`vpnstream.auth` is absent the forward proxy serves any client reachable on
`vpnstream.listen` — bind to loopback (the default) or front it with other
access control if you take that path. The `api.auth.proxy_token_file` and
`api.auth.admin_token_file` are both mandatory whenever the `api` block is
present, must be mode `0600`, owned by the running user, and 64–512 bytes; the
daemon rejects other modes or lengths at startup.

**TLS is set by CLI flags, not `proxy.json`.** Pass `-tls-cert-dir` (empty →
plain HTTP, loopback/dev only; e.g. `/opt/vpntunnel/tls/` → HTTPS 1.3 with a
self-signed cert the daemon manages), `-tls-hostname` (default `localhost`),
and `-tls-ip-sans` (comma-separated IP SANs). Locally the API is plain HTTP;
production passes `-tls-cert-dir` and serves HTTPS.

If you move `access_log.path` outside `/opt/vpntunnel/logs/` (the path
covered by the unit's `ReadWritePaths`), also edit `vpntunnel.service` to
add the new path to `ReadWritePaths` and run `systemctl daemon-reload`.
Otherwise the daemon fails to open the log at startup and systemd restart-loops.

The WireGuard `.conf` is standard wg-quick format:

```ini
[Interface]
PrivateKey = ...
Address    = 10.x.y.z/32
DNS        = 10.64.0.1

[Peer]
PublicKey           = ...
AllowedIPs          = 0.0.0.0/0
Endpoint            = 169.150.x.x:51820
PersistentKeepalive = 25
```

Any wg-quick `.conf` works — Mullvad, ProtonVPN, IVPN, your own peer. No
custom format, no provider-specific bootstrap.

## Deploying (as an operator)

The deploy pipeline uploads a fresh binary into an immutable per-version
artifact store on the production host on every `v*` tag, atomically flips the
`release` channel symlink to it, and restarts the systemd unit (self-healing
back to the previous version if the restart or health check fails).

- **CI** — every push to `main` and every PR against `main` runs
  `.github/workflows/ci.main.yml`. Lint + tests + sanity `go build`. No
  binary build, no deploy.
- **Release** — every `v*` tag push runs `.github/workflows/release.yml`,
  waits for required-reviewer approval on the PRIME environment, builds the
  binary on the runner, uploads it into `artifacts/<VERSION_ID>/`, flips the
  `release` channel symlink, restarts the unit, and verifies with `systemctl is-active` +
  `GET /v1/admin/health` (HTTPS, admin token). Pre-release tags (`vX.Y.Z-rc1`
  etc.) deploy identically — there is no floating-tag surface to protect.

> **v5 → v6 cutover: Docker → systemd**
>
> If you are upgrading from the previous Docker-based deploy, complete the
> one-time migration below **before pushing the first v6-series tag**. The
> automated deploy workflow will fail if these prerequisites are absent.
>
> 1. On the production host, stop and remove the Docker container:
>    ```bash
>    cd /opt/vpntunnel
>    docker compose down || true
>    ```
> 2. Build the binary locally and seed it into the artifact store, then point the
>    `release` channel at it (see § 7 step 4 for the full form):
>    ```bash
>    CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ./build/vpntunnel ./cmd/vpntunnel/
>    VID="$(date -u +%Y%m%d%H%M%S)-r_<version>"
>    ssh github_aide@host "mkdir -p /opt/vpntunnel/artifacts/$VID"
>    scp ./build/vpntunnel "github_aide@host:/opt/vpntunnel/artifacts/$VID/vpntunnel"
>    ssh github_aide@host "chmod +x /opt/vpntunnel/artifacts/$VID/vpntunnel && ln -sfn ../artifacts/$VID /opt/vpntunnel/bin/release"
>    ```
>    Then seed configs, the tunnel `.conf`, and the unit file per § 7. Create the
>    two required API tokens (`proxy_token`, `admin_token`) under
>    `/opt/vpntunnel/configs/auth/` at mode `0600`, owned by `github_aide` (see
>    § 7 step 3). The full one-time host restructure is in
>    `configs/RUNBOOK-migrate-release-layout.md`.
> 3. On the host, promote the unit file and start the service:
>    ```bash
>    sudo mv /tmp/vpntunnel.service /etc/systemd/system/
>    sudo systemctl daemon-reload
>    sudo systemctl enable --now vpntunnel
>    sudo systemctl status vpntunnel
>    curl -k -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" \
>      https://127.0.0.1:8888/v1/admin/health
>    ```
> 4. Drop the sudoers line for the deploy user (see § 7 step 6 below).
>
> Only after all four steps above succeed should you push the first v6
> production tag.

<details>
<summary><b>Full deployment guide</b> — smoke test, server bootstrap, GH Environments, rollback (click to expand)</summary>

### 1. Local smoke test

Build the binary and run it locally against your dev config to confirm
it starts and the WireGuard handshake completes. Without `-tls-cert-dir`
the API serves plain HTTP, so use `http://` and no `-k`:

```bash
make build
./build/vpntunnel -config ./configs/proxy.json
# in another terminal:
curl -sS \
  -H "X-Vpntunnel-Token: $(cat ./configs/auth/admin_token)" \
  http://127.0.0.1:8888/v1/admin/health | jq .
# expected: {"status":"ok","tunnels":[{"id":"...","healthy":true,"handshake_age_seconds":N}]}
```

`make examination` runs the same checks plus the role/routing contract
(admin-only health, the `/v1/tunnels` catalog, unknown-id handling) against
a locally running daemon. This is a local sanity check only; it does not
deploy to the server.

### 4. File modes

Restrict access to secrets before placing them on the server. The two API
token files **must** be mode `0600` and owned by the running user (`github_aide`) —
the daemon rejects any other mode/owner at startup:

```bash
# the whole configs/ tree is owned by the service user, because the daemon
# requires its secrets to be owned by the process UID and the service runs as
# github_aide (= the deploy user).
sudo chown -R github_aide:github_aide /opt/vpntunnel/configs
chmod 0700 /opt/vpntunnel/configs/auth /opt/vpntunnel/configs/tls
chmod 0600 /opt/vpntunnel/configs/tunnels/*.conf
chmod 0600 /opt/vpntunnel/configs/auth/token            # forward-proxy (optional)
chmod 0600 /opt/vpntunnel/configs/auth/proxy_token      # API: required
chmod 0600 /opt/vpntunnel/configs/auth/admin_token      # API: required
sudo chown root:root /opt/vpntunnel
sudo chmod 0755 /opt/vpntunnel
```

The service is de-rooted: it runs as `github_aide` (the same user that deploys).
The base dir, `.env`, and the unit stay root-owned; the CI/service user
owns `configs/`, `artifacts/`, `bin/`, `state/`, and `logs/`.
`/opt/vpntunnel/logs/` must stay writable by `github_aide` so lumberjack can rotate
the access log. The daemon generates `configs/auth/tunnel-id.key` (mode `0600`) on
first run if absent — but `ProtectSystem=strict` blocks that on a cold start, so
pre-generate it (see the runbook); back it up, see [Security](#security). Full
ownership/mode table and the one-time restructure are in
`configs/RUNBOOK-migrate-release-layout.md`.

### 5. `.conf` files

Place each `.conf` file at `/opt/vpntunnel/configs/tunnels/` and set mode
`0600`. They are auto-discovered on the next startup — no entry in
`proxy.json`. Treat each file like an SSH private key — never commit it to a
shared repository.

### 6. Debugging the running service

```bash
journalctl -u vpntunnel -f                          # follow operational log
journalctl -u vpntunnel --since '10 minutes ago'    # last 10 minutes
systemctl status vpntunnel                          # unit state + last lines
ss -tnlp | grep vpntunnel                           # verify listen ports
# WireGuard tunnel health (-k for the production self-signed cert; token in shell history — use env var if that matters)
curl -k -sS -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" \
  https://127.0.0.1:8888/v1/admin/health
```

### 7. Server-side prerequisites (do this once on the production host)

The deploy pipeline ships to a single host. The steps below configure it
end-to-end so `release.yml` has something healthy to update.

**The deploy workflow does NOT install the unit file.** The operator does
this once, as described in step 5.

**1. Provision the deployment directory.**

The service is de-rooted: the unit runs as `github_aide`, the same user that
deploys (no separate runtime user). The base dir, `.env`, and the unit
stay `root:root`; the CI/service user owns the artifact store, channel symlinks,
the state/logs/cert dirs, and the `configs/` tree (the daemon requires its secrets
to be owned by the process UID).

```bash
sudo install -d -o github_aide -g github_aide -m 0755 /opt/vpntunnel/configs /opt/vpntunnel/configs/tunnels
sudo install -d -o github_aide -g github_aide -m 0700 /opt/vpntunnel/configs/auth /opt/vpntunnel/configs/tls
sudo install -d -o github_aide -g github_aide -m 0755 /opt/vpntunnel/artifacts /opt/vpntunnel/bin
sudo install -d -o github_aide -g github_aide -m 0750 /opt/vpntunnel/state /opt/vpntunnel/logs
sudo chown root:root /opt/vpntunnel
sudo chmod 0755 /opt/vpntunnel
```

**3. Drop the config files.**

Place `proxy.json` and your `.conf` files under `/opt/vpntunnel/configs/`, and
create the auth tokens in `/opt/vpntunnel/configs/auth/`. The two API tokens
(`proxy_token`, `admin_token`) are **required** and must be 64–512 bytes; the
forward-proxy `token` is optional. Generate and lock them down:

```bash
openssl rand -hex 48 | sudo tee /opt/vpntunnel/configs/auth/proxy_token  > /dev/null
openssl rand -hex 48 | sudo tee /opt/vpntunnel/configs/auth/admin_token > /dev/null
openssl rand -hex 48 | sudo tee /opt/vpntunnel/configs/auth/token       > /dev/null  # optional

chmod 0600 /opt/vpntunnel/configs/tunnels/*.conf
chmod 0600 /opt/vpntunnel/configs/auth/proxy_token /opt/vpntunnel/configs/auth/admin_token
chmod 0600 /opt/vpntunnel/configs/auth/token
```

The two API token files must be mode `0600` and owned by the running user, or
the daemon refuses to start.

**4. Bootstrap the binary and install the systemd unit.**

Build the binary locally and seed an initial version into the artifact store,
then point the `release` channel at it (no daemon running yet, so the swap is
simple). `VID` is `<UTC-timestamp>-r_<version>`:

```bash
CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o ./build/vpntunnel ./cmd/vpntunnel/
VID="$(date -u +%Y%m%d%H%M%S)-r_<version>"
ssh github_aide@host "mkdir -p /opt/vpntunnel/artifacts/$VID"
scp ./build/vpntunnel "github_aide@host:/opt/vpntunnel/artifacts/$VID/vpntunnel"
ssh github_aide@host "chmod +x /opt/vpntunnel/artifacts/$VID/vpntunnel && ln -sfn ../artifacts/$VID /opt/vpntunnel/bin/release"
```

Seed `proxy.json`, the tunnel `.conf` files, and the auth tokens per step 3 (all
under `configs/`, owned by `github_aide`). Then install the unit and sudoers and
start the service on the host:

```bash
sudo install -m 0644 /opt/vpntunnel/configs/vpntunnel.service /etc/systemd/system/vpntunnel.service
sudo install -m 0440 /opt/vpntunnel/configs/vpntunnel.sudoers /etc/sudoers.d/vpntunnel-deploy
sudo systemctl daemon-reload
sudo systemctl enable --now vpntunnel
sudo systemctl status vpntunnel
curl -k -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/configs/auth/admin_token)" \
  https://127.0.0.1:8888/v1/admin/health
```

If the unit file changes in a future repo update, re-scp and `daemon-reload`
manually — the deploy workflow does not push unit file changes.

**6. Authorize the deploy user to restart the unit.**

The deploy user `github_aide` is non-root, so it needs a restricted sudoers grant
to run the workflow's `systemctl`/`journalctl` commands without a password. The
exact grant ships in the repo — install it verbatim:

```bash
sudo install -m 0440 /opt/vpntunnel/configs/vpntunnel.sudoers /etc/sudoers.d/vpntunnel-deploy
sudo visudo -c            # must report "parsed OK"
```

The grant is split into three exact commands (not prefixes) so a leaked deploy
key cannot run arbitrary `sudo` — only `systemctl restart`, `systemctl is-active
--quiet`, and the deploy's `journalctl` invocation are permitted. Any future
workflow change that adds a new `sudo` command must update
`configs/vpntunnel.sudoers` in lockstep, or the deploy will fail on that step.

### 8. GitHub secrets, variables, and environments

The deploy job reads its configuration from the **`PRIME` GitHub
Environment**. Scoping the SSH secret to an Environment (rather than the
repo) keeps it off any workflow that has no business reaching the
production host.

#### Creating the GitHub Environment

1. Go to `repo → Settings → Environments`.
2. Create an environment named **`PRIME`**.
   - Under "Deployment protection rules", enable "Required reviewers" and
     add the repo owner (yourself). This creates a manual approval gate:
     every `v*` tag push pauses before the deploy step until you click
     "Approve and deploy" in the Actions UI.
   - Add the `SSH_*` and (optionally) `TELEGRAM_*` secrets and variables
     listed below.

#### Full secret and variable inventory

`REMOTE_DIR` is a **repository-level variable** because the path
(`/opt/vpntunnel`) doesn't carry environment-specific value; override at
the environment level only if the host actually uses a different path.

> **Note:** the deploy/runtime user is `github_aide`. If migrating from an older
> setup, set the PRIME environment's `REMOTE_DIR` to `/opt/vpntunnel` and
> `SSH_USERNAME` to `github_aide` before pushing the first tag.

| Name | Scope | Type | Purpose | Example value |
|---|---|---|---|---|
| `SSH_PRIVATEKEY` | Environment (`PRIME`) | Secret | ed25519 private key for the production host | full key contents |
| `SSH_HOSTNAME` | Environment (`PRIME`) | Variable | hostname or IP of the production VPS | `proxy.example.com` |
| `SSH_HOSTPORT` | Environment (`PRIME`) | Variable | SSH port of the production VPS | `2222` |
| `SSH_USERNAME` | Environment (`PRIME`) | Variable | SSH/deploy user on the production VPS (also the service user) | `github_aide` |
| `REMOTE_DIR` | Repository (or `PRIME` override) | Variable | Absolute path to the service base dir on the host | `/opt/vpntunnel` |
| `VPNTUNNEL_ADMIN_TOKEN` | Environment (`PRIME`) | Secret | Admin API token for the post-deploy `/v1/admin/health` check — **required**, or the health check 401s and the deploy auto-rolls-back and fails | matches `configs/auth/admin_token` on the host |
| `ACTION_EMITER_TBOT_TOKEN` | Environment (`PRIME`) | Secret (optional) | Bot token used by the post-deploy notify steps | `123456:AbCdEf…` |
| `TELEGRAM_ROOT_CHAT_ID` | Environment (`PRIME`) | Variable (optional) | Telegram chat ID that receives deploy notifications | `-1001234567890` |

The Telegram pair is optional — if either value is empty, the notify steps
short-circuit and no message is sent. The deploy itself never depends on
Telegram reachability.

**Why variables (not secrets) for host/user/port?** None of these values
are sensitive. Making them variables keeps them visible in workflow logs,
which is useful when debugging "did this deploy hit the right host?"

**Note on host-key verification.** The workflow does NOT pin the server's
SSH fingerprint — `ssh-keyscan` accepts whatever host key the server
presents at deploy time (TOFU). MITM-resistance depends on the path
between GitHub's runner and your VPS not being hijacked. If you want strict
pinning, switch to a manual `ssh -o StrictHostKeyChecking=yes` with a
pre-pinned `known_hosts` entry.

#### Generating the deploy key

```bash
ssh-keygen -t ed25519 -C "vpntunnel-deploy-prod@$(hostname)" \
  -f ~/.ssh/vpntunnel_prod_deploy -N ""
```

`-N ""` creates a passphrase-less key, which is intentional — unattended
deploys require a key that can be used without interactive input.

**Installing the public key on the server:**

```bash
ssh-copy-id -i ~/.ssh/vpntunnel_prod_deploy.pub github_aide@<prod-host>
```

**Adding the private key to GitHub:**

Paste the full contents of the private key file into the `SSH_PRIVATEKEY`
secret in the `PRIME` environment. The value must include the
`-----BEGIN OPENSSH PRIVATE KEY-----` and `-----END OPENSSH PRIVATE KEY-----`
lines and a trailing newline.

**Key rotation procedure:**

1. Generate a new ed25519 key pair on your workstation.
2. Add the **new** public key to `~github_aide/.ssh/authorized_keys` on the
   server **before** removing the old one.
3. Update the `SSH_PRIVATEKEY` secret in the `PRIME` environment.
4. Push a throwaway pre-release tag and confirm the workflow run is green.
5. Remove the **old** public key from `authorized_keys`.

Always add before removing — reverting step 3 is easy if step 4 fails; a
server locked out by premature deletion is not.

### 9. Rollback

**Automatic rollback.** The deploy self-heals: if `systemctl is-active` fails OR
`GET /v1/admin/health` does not return `status=ok` after the flip, the workflow
points `bin/release` back at the `VERSION_ID` it captured before the flip and
restarts. A failed deploy therefore leaves the last-good version running. The run
still fails (so you are alerted), and the job log carries the journal for diagnosis.

**Source of truth for "what's running" on the host:**

```bash
readlink /opt/vpntunnel/bin/release        # -> ../artifacts/<VERSION_ID>
systemctl show vpntunnel --property=ExecMainStartTimestamp,ExecMainPID
```

**Primary path:** push a new tag pointing at an older commit.

```bash
git log --oneline --tags | head
git tag v1.2.4-rollback <oldsha>
git push origin v1.2.4-rollback
```

The workflow re-runs end-to-end: test → build → approval gate → deploy.
Use a fresh tag — re-pushing an existing tag breaks immutability expectations.

**Fast manual fallback (no time to wait for a pipeline run):**
The host retains the **3 newest version dirs** under `artifacts/` (the active
version plus 2 rollback candidates); a version a channel still points at is never
pruned, so rolling back to any retained version is always safe. To roll back, flip
the channel — a relative target resolved from `bin/`:

```bash
# list the retained version dirs on the host, newest first
ssh github_aide@<prod-host> 'ls -1dt /opt/vpntunnel/artifacts/*/ | head -3'

# roll back to a specific version (replace <VID> with one from the list above)
ssh github_aide@<prod-host> \
  'ln -sfn ../artifacts/<VID> /opt/vpntunnel/bin/release && sudo systemctl restart vpntunnel'
```

This bypasses the approval gate entirely — use it only in genuine production
emergencies. If the version you need is older than the 3 retained on the host,
check out the older tag on your workstation, build, and seed it into `artifacts/`
manually (see § 7 step 4).

### 10. Hardening follow-ups (out of scope for current plans)

None of the items below are implemented. They are tracked here as future work.

- **SHA-pin actions in workflow files** — all workflow files are on floating
  major-version pins. Pinning to commit SHAs prevents supply-chain compromise.

- **`command=` restriction in `authorized_keys`** — limits attacker blast
  radius if a deploy key is leaked. Requires the deploy script to be stable.

- **`fail2ban` on the SSH port** — brute-force mitigation.

- **Off-machine log shipping** (Loki, Vector, S3) — logs currently live only
  on the server.

- **Tailscale or WireGuard for the SSH channel** — eliminates the
  public-internet SSH attack surface.

</details>

## Observability

The daemon emits two log streams:

**Operational log** (stdout → journald via `StandardOutput=journal`):

- Lifecycle: startup, config load, tunnel handshake, graceful shutdown.
- Auth failures: `client_addr`, `method`, `target`, `reason` (one of
  `missing`, `wrong_scheme`, `wrong_token`, `malformed`). **Never** the
  token itself.
- **Auth bypass (loopback)**. One line per request when `auth` is configured and
  the client connects from `127.0.0.0/8` or `::1`. Shape: `msg="auth bypassed"`,
  fields `reason=loopback`, `client_addr=<RemoteAddr>`. Expected during normal
  local use; spot-check `client_addr` if you see unexpectedly high volume —
  every value must be an actual loopback address, since the log line only
  fires when `isLoopbackRemote` returns true.

```bash
journalctl -u vpntunnel -f
```

**Access log** (rotating JSONL file via lumberjack):

```json
{
  "ts": "2026-06-08T12:34:56Z",
  "method": "CONNECT",
  "target": "example.com:443",
  "client_addr": "127.0.0.1:54321",
  "status_code": 200,
  "bytes_in": 1234,
  "bytes_out": 5678,
  "duration_ms": 142,
  "upstream_error": ""
}
```

```bash
tail -F /opt/vpntunnel/logs/access.log
```

The access log has no `Authorization` or `Proxy-Authorization` field by
design. The Bearer token never appears in any log stream.

**Health endpoint** — `GET /v1/admin/health` on `api.listen` (default
`127.0.0.1:8888`) returns the aggregated multi-tunnel health snapshot.
Requires the `X-Vpntunnel-Token` header with the admin token — there is no
loopback bypass on the API server. The API is plain HTTP unless the daemon was
started with `-tls-cert-dir`, in which case it serves HTTPS with a self-signed
certificate (pass `-k` to curl). Production runs with TLS; a local dev daemon
does not.

Response body shape:

```json
{
  "status": "ok",
  "tunnels": [
    {"id": "se-sto-wg-001", "healthy": true,  "handshake_age_seconds": 42},
    {"id": "de-ber-wg-001", "healthy": false, "handshake_age_seconds": -1}
  ]
}
```

`status` values: `ok` (all tunnels healthy, HTTP 200), `degraded` (some healthy,
HTTP 200 — automation must parse the body), `down` (none healthy or empty pool, HTTP 503).
`handshake_age_seconds` is `-1` when no handshake has been observed or an error
occurred reading tunnel state. The staleness threshold is a fixed 180s (not
configurable).

## Security

**Threat model (brief).** `vpntunnel` hides your egress IP from upstream
origins by routing every byte through a WireGuard exit. It does **not**
encrypt the client → proxy hop (use it on loopback, an SSH tunnel, or a
Tailscale / WireGuard overlay network), does **not** prevent client-side
DNS leaks (the client must resolve through the proxy or DoH), and does
**not** protect against traffic analysis by your WireGuard provider.

**Loopback bypass (forward proxy only).** Clients connecting to the forward
proxy (`vpnstream.listen`, default `:7788`) from the loopback interface
(`127.0.0.1` or `::1`) bypass the Bearer-token challenge. With the default
loopback binding this means every local process can use the proxy without a
token; with a non-loopback `vpnstream.listen` the bypass is inactive and every
request must carry a valid Bearer token. The rule is: if you change
`vpnstream.listen` to a non-loopback address, also configure `vpnstream.auth`,
or the proxy is open to any network client. The API listener has **no** such
bypass — every API request always needs a valid `X-Vpntunnel-Token`.

**Secrets discipline.**

- WireGuard private keys live in `.conf` files in `./configs/tunnels/`, mode
  `0600`, gitignored.
- Bearer/API tokens live in `./configs/auth/`, mode `0600`, gitignored. The two
  API token files are rejected by the daemon unless they are exactly `0600` and
  owned by the running user.
- The tunnel-id HMAC key (`tunnel_id_hmac_key_file`, default
  `./configs/auth/tunnel-id.key`) is 64 bytes of key material, mode `0600`,
  auto-generated on first run. **Never log it; back it up like a private key** —
  overwriting it silently rotates every derived tunnel id. The derived hex ids
  themselves are public and may appear in logs and API responses.
- None of the above appear in log output, error messages, or HTTP response
  bodies. Auth-failure responses are uniform — `407` with body
  `Proxy authentication required.\n`, no diagnostic that leaks token shape.
- The deploy SSH key is ed25519, passphrase-less, dedicated to the
  production host. Rotate per the procedure in §8.
- Token comparison uses `crypto/subtle.ConstantTimeCompare`. No length
  oracle, no early exit.

**Reporting vulnerabilities.** See [`SECURITY.md`](./SECURITY.md) for full
threat model, supported versions, dependency policy, and the responsible
disclosure procedure. Short version: open a private GitHub Security
Advisory or email `seilbekskindirov@gmail.com`; do not file public issues
for unpatched vulnerabilities.

## License

MIT. See [`LICENSE`](./LICENSE).
