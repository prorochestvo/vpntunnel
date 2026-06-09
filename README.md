# httpproxy

Lightweight forward HTTP proxy that routes every byte of egress through a
**userspace WireGuard tunnel** — no root, no `setcap`, no host-level VPN.
One static binary, distroless container, ~6k LOC.

```
   ┌────────┐  HTTP / CONNECT       ┌─────────────┐   WireGuard    ┌──────────┐
   │ client │ ─── :8080 ─────▶───   │  httpproxy  │ ─── userspace ─│ exit IP  │
   │  curl, │                       │   daemon    │ ───────▶─────  │ (Mullvad,│
   │ browser│                       │  (no root)  │                │  yours)  │
   │   app  │                       └─────────────┘                └──────────┘
   └────────┘                              │
                              /healthz :8081 (loopback, no auth)
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
curl --proxy http://<proxy-host>:8080 \
     --proxy-header "Proxy-Authorization: Bearer <your-token>" \
     https://am.i.mullvad.net/json
# {"mullvad_exit_ip": true, "country": "Sweden", ...}
```

**As an operator** — bring it up locally to try it out:

```bash
# 1. Get a wg-quick .conf from any WireGuard provider (or your own peer).
#    Mullvad: https://mullvad.net/account/wireguard-config
mkdir -p ./configs/tunnels ./configs/auth ./logs
mv ~/Downloads/se-sto-wg-001.conf ./configs/tunnels/
chmod 0400 ./configs/tunnels/*.conf

# 2. Generate a Bearer token.
openssl rand -hex 32 > ./configs/auth/token
chmod 0400 ./configs/auth/token

# 3. Write ./configs/proxy.json.
cat > ./configs/proxy.json <<'EOF'
{
  "upstream": { "configs": ["./tunnels/se-sto-wg-001.conf"] },
  "auth":     { "token_file": "./auth/token" }
}
EOF

# 4. Build and run.
make build && ./build/httpproxy -config ./configs/proxy.json
```

For a remote-server deployment via Docker + GitHub Actions, see
[Deploying](#deploying-as-an-operator).

## Using the proxy (as a client)

You need two things: the proxy URL (`http://<host>:8080`) and a Bearer
token. Both come from the operator. The auth header is **always**
`Proxy-Authorization` — not `Authorization`, which would travel to the
upstream origin and leak the token.

### curl

```bash
# HTTPS via CONNECT
curl --proxy http://<host>:8080 \
     --proxy-header "Proxy-Authorization: Bearer <token>" \
     https://example.com

# HTTP via forward proxy
curl --proxy http://<host>:8080 \
     --proxy-header "Proxy-Authorization: Bearer <token>" \
     http://example.com
```

Use `--proxy-header`, not `-H`. The `-H` flag puts the header on the
**forwarded request**, leaking the token to the upstream origin.
`--proxy-header` puts it on the CONNECT/proxy request, where the proxy
expects it.

A shell alias for repeat use:

```bash
alias xcurl='curl --proxy http://<host>:8080 --proxy-header "Proxy-Authorization: Bearer $(cat ~/.httpproxy.token)"'
xcurl https://am.i.mullvad.net/json
```

### Browser and system-wide

**Firefox** (`about:preferences#general → Network Settings → Manual proxy
configuration`):

- HTTP Proxy: `<host>` Port: `8080`
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
export HTTPS_PROXY=http://<host>:8080
export HTTP_PROXY=http://<host>:8080
# Most tools that read these vars do NOT pass Proxy-Authorization
# automatically — either disable auth or use the tool's programmatic API.
```

### Programmatic clients

**Go (`net/http`):**

```go
proxyURL, _ := url.Parse("http://<host>:8080")

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
    proxy="http://<host>:8080",
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
| `Connection refused` on the proxy port | daemon not running, or listening on loopback while you're connecting remotely | `docker logs httpproxy`; check `listen` is bound where you expect |
| HTTPS hangs ~30s then times out | WireGuard handshake stale or peer unreachable | `curl http://<host>:8081/healthz` — a 503 means the tunnel is down |
| Exit IP is your real IP, not the VPN | proxy URL points at the wrong host, or upstream isn't routing | confirm `am.i.mullvad.net/json` returns `mullvad_exit_ip: true` |
| Browser shows mixed-content warnings | proxy itself is plain HTTP (no TLS-termination) | this is expected; the tunnel **through** the proxy is encrypted (WireGuard), the client→proxy hop is not |
| `Proxy-Authorization` header appearing in upstream request logs | client sent it as `Authorization` or via a `-H`-equivalent | use the proxy-specific header API (`--proxy-header`, `ProxyConnectHeader`) |

For tunnel-level state, hit the admin endpoint:

```bash
curl -sS http://<host>:8081/healthz | jq .
# 200: {"status":"ok", "handshake_age_seconds": 42}
# 503: {"status":"unhealthy", "reason":"handshake_stale", "handshake_age_seconds": 312}
```

`reason` enum:

- `no_handshake` — tunnel hasn't completed its first handshake since startup.
- `handshake_stale` — last handshake older than `health.handshake_max_age`.
- `reporter_error` — internal failure reading tunnel state.

## Configuration reference

`proxy.json` is the only configuration file. All paths inside it are
resolved relative to the directory **containing `proxy.json`** (not the
process cwd — systemd and cron use `/`).

| Field | Type | Default | Purpose |
|---|---|---|---|
| `listen` | string | `127.0.0.1:8080` | Proxy listener (HTTP/CONNECT) |
| `dial_timeout` | duration | `10s` | Per-upstream dial timeout |
| `idle_timeout` | duration | `90s` | `http.Server` idle connection timeout |
| `shutdown_timeout` | duration | `15s` | Graceful shutdown drain on SIGTERM |
| `upstream.configs` | string[] | **required** | Paths to wg-quick `.conf` files |
| `upstream.active` | string | first entry | Basename (no `.conf`) of the tunnel to use |
| `auth.token` | string | empty | Inline Bearer token (mutually exclusive with `token_file`) |
| `auth.token_file` | string | empty | Path to a file whose contents are the Bearer token |
| `admin.listen` | string | `127.0.0.1:8081` | Admin/healthz listener |
| `admin.shutdown_timeout` | duration | `5s` | Admin graceful-shutdown drain |
| `health.handshake_max_age` | duration | `180s` | Age threshold for `/healthz` to flip to 503 |
| `access_log.path` | string | required | Path to rotating JSONL access log |
| `access_log.max_size_mb` | int | lumberjack default | Rotation size threshold |
| `access_log.max_age_days` | int | lumberjack default | Rotated-file age cap |
| `access_log.max_backups` | int | lumberjack default | Rotated-file count cap |
| `access_log.compress` | bool | `true` | gzip rotated files |
| `operational.level` | string | `info` | slog level: `debug`/`info`/`warn`/`error` |
| `operational.format` | string | `text` | slog format: `text`/`json` |

Auth is **optional**. Missing or empty `auth` block disables challenge — the
proxy serves any client reachable on `listen`. If you take that path, bind
to loopback (`127.0.0.1`, default) or front it with another access control.

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

For remote-server deployment the proxy ships as a Docker image hosted on
GHCR at `ghcr.io/<owner>/httpproxy`, with two automated pipelines:

- **Staging** — every push to `main` runs `.github/workflows/staging.yml`,
  publishes `:main-<7-char-sha>` (immutable) and `:edge` (floating), and
  rolls the staging host.
- **Production** — every `v*` tag push runs
  `.github/workflows/release.yml`, publishes `:vX.Y.Z`, `:X.Y`, and
  `:latest`, waits for required-reviewer approval, then rolls the
  production host.

Both pipelines share a single build defined in
`.github/workflows/build-image.yml`. The image built from a given SHA is
byte-identical whether it was triggered by a main push or a tag push.

**Replace `<owner>` with the actual GitHub username / org in every snippet
below.**

> **v4 → v5 cutover (skip if this is a first-time install)**
>
> Plan 006 changed both the repo `configs/compose.yml` and the server-side
> template to use `${IMAGE_TAG}` (no default) instead of the literal `:latest` tag.
> Before the first v5-built tag is deployed to your server, edit
> `/opt/httpproxy/compose.yml` on the server and replace `:latest` with
> `:${IMAGE_TAG}`:
>
> ```yaml
> image: ghcr.io/<owner>/httpproxy:${IMAGE_TAG}
> ```
>
> Then run a manual smoke to confirm the new shape works before letting
> GitHub Actions take over:
>
> ```bash
> sudo -iu httpproxy
> cd /opt/httpproxy
> IMAGE_TAG=<current-tag> docker compose up -d --wait --wait-timeout 120
> ```
>
> Substitute `<current-tag>` with the latest version already on GHCR (e.g.
> `1.0.0`). If `--wait` succeeds, the server is ready for automated deploys.

> **`config/` → `configs/` rename (skip if this is a first-time install)**
>
> The runtime config directory was renamed from `config/` to `configs/`
> across the repo (Dockerfile and `compose.yml` were also moved into the
> same directory). The server-side bind-mount path therefore changed from
> `./config:/etc/httpproxy:ro` to `./configs:/etc/httpproxy:ro`. Before
> the first post-rename deploy, on each host:
>
> ```bash
> sudo -iu httpproxy
> cd /opt/httpproxy
> mv config configs
> # edit compose.yml so the volume reads:  - ./configs:/etc/httpproxy:ro
> IMAGE_TAG=<current-tag> docker compose up -d --wait --wait-timeout 120
> ```
>
> The deploy job does NOT do this migration for you — it `cd`s into
> `$REMOTE_DIR` and runs `docker compose up`. A mismatched bind-mount
> path means the container starts with an empty `/etc/httpproxy`,
> fails to load `proxy.json`, and the `/healthz` smoke step trips.

<details>
<summary><b>Full deployment guide</b> — one-shot run, compose, server bootstrap, GH Environments, rollback (click to expand)</summary>

### 1. One-shot `docker run`

```bash
mkdir -p ./logs
sudo chown 65532:65532 ./logs    # distroless runs as UID 65532

docker run -d --name httpproxy --restart unless-stopped \
  -p 127.0.0.1:8080:8080 \
  -p 127.0.0.1:8081:8081 \
  -v $(pwd)/configs:/etc/httpproxy:ro \
  -v $(pwd)/logs:/var/log/httpproxy \
  ghcr.io/<owner>/httpproxy:latest
```

Use `:edge` to pull the latest staging image or `:latest` for the latest
production release.

### 2. Compose (recommended)

```bash
mkdir -p ./logs
sudo chown 65532:65532 ./logs
IMAGE_TAG=latest docker compose -f configs/compose.yml up -d
docker compose -f configs/compose.yml logs -f
```

### 3. First-time GHCR package visibility

Every first workflow run that pushes a new tag shape creates a **private**
package entry. External `docker pull` fails with `unauthorized: unauthorized`
until you flip it. This affects both `:edge` (first `staging.yml` run) and
any new semver tags (first `release.yml` run after a new major version).

If you wire up staging before pushing the first production tag (the
recommended order), the first `staging.yml` run publishes `:edge` as a
private package. Flip it to public before the first `release.yml` run, or
`docker compose pull` on the production host will fail.

1. After the first `staging.yml` run succeeds, go to
   `https://github.com/users/<owner>/packages/container/httpproxy/settings`
   (user-owned repo) or
   `https://github.com/orgs/<org>/packages/container/httpproxy/settings`
   (org-owned).
2. Scroll to "Danger Zone" → "Change package visibility".
3. Select "Public", confirm.

After this, `docker pull ghcr.io/<owner>/httpproxy:edge` and
`docker pull ghcr.io/<owner>/httpproxy:latest` work unauthenticated.

### 4. Volume permissions

The distroless image runs as UID 65532. The bind-mounted `./logs/` directory
must be writable by that UID, otherwise lumberjack fails to open the access
log at startup:

```bash
sudo chown 65532:65532 ./logs
```

Do this once per host before the first run.

### 5. `.conf` files

Bind-mount the directory containing your `.conf` files onto
`/etc/httpproxy/tunnels/` (the path that relative entries in `proxy.json`
resolve to). Treat each file like an SSH key:

```bash
chmod 0400 ./configs/tunnels/*.conf
```

The container reads them via the read-only mount; they never enter the
image.

### 6. Debugging a running container

The distroless image has no shell — `docker exec -it container sh` will
fail. Use one of:

- `docker logs <container>` — operational log goes to stdout.
- `docker run --rm --entrypoint=/httpproxy ghcr.io/<owner>/httpproxy:latest -healthcheck`
  — one-shot TCP probe.
- `docker inspect <container>` — full state including healthcheck history
  (in `Health.Log`).

### 7. Server-side prerequisites (do this on each host: staging and production)

The procedure is identical for both hosts. The only difference is which
image tag the operator uses for the first-time manual smoke test: `:edge`
on staging, the latest semver (e.g. `1.0.0`) on production.

If you are adding a staging host for the first time (the existing host from
Plan 006 becomes the production host), there is no migration — both
environments are independent state. Stand up the second host fresh,
following these steps.

**1. Install Docker Engine and Docker Compose v2.17+.**

Docker Compose v2.17 is load-bearing — it introduced the `--wait` flag that
the deploy job depends on. Verify after install:

```bash
docker compose version
# must be >= 2.17.0
```

Follow the [official installation guide](https://docs.docker.com/engine/install/)
for your distro. On Debian/Ubuntu, `docker-compose-plugin` from the Docker
apt repo ships v2; the `docker-compose` (v1) snap does not.

**2. Create a dedicated deploy user.**

```bash
useradd -m -s /bin/bash httpproxy
usermod -aG docker httpproxy
```

> **Security note:** membership in the `docker` group is effectively root on
> the host — any user in the group can mount `/` via `docker run -v /:/host`.
> This is the accepted trade-off for unattended deploys without `sudo`.
> Rootless Docker is listed under §10 hardening follow-ups.

The `docker` group membership lets the `httpproxy` user run `docker compose`
without `sudo`. Log out and back in (or open a new login shell for the
`httpproxy` user) before proceeding — the group is not effective until the
session is refreshed.

**3. Provision the deployment directory.**

```bash
mkdir -p /opt/httpproxy/configs/tunnels /opt/httpproxy/logs
chown -R httpproxy:httpproxy /opt/httpproxy
chown 65532:65532 /opt/httpproxy/logs
```

The `chown 65532:65532` step is required because the distroless container
image runs as UID 65532 (nonroot). There is no `65532` user on the host —
that is intentional; the bind mount is matched by UID, not by name.

**4. Drop the config files.**

Switch to the `httpproxy` user and place `proxy.json` and the WireGuard
`.conf` file:

```bash
sudo -iu httpproxy
cd /opt/httpproxy
# copy proxy.json and your .conf file here
chmod 0400 configs/tunnels/*.conf
```

Each host should use its own `.conf` with its own WireGuard tunnel. The
staging and production tunnels should be distinct — sharing a single
WireGuard peer endpoint between both hosts defeats the independence
guarantee of having separate environments.

**5. Hand-author `/opt/httpproxy/compose.yml`.**

This file is NOT copied automatically from the repo. It is authored once
by the operator and is never overwritten by the deploy job. Legitimate
divergences (sidecars, restart policy, volume paths, `env_file`) are
preserved across every deploy. The only contract the deploy job imposes
is that the file accepts `IMAGE_TAG` from the environment and that
`docker compose up -d --wait` works against it.

The repo ships a minimal template at `configs/compose.yml` — use it as
a starting point (`scp configs/compose.yml <host>:/opt/httpproxy/`) and
edit on the server, or paste the snippet below verbatim. The image
reference is hardcoded for this project (`ghcr.io/prorochestvo/httpproxy`);
if you fork to your own GHCR namespace, swap it out:

```yaml
services:
  httpproxy:
    # --wait relies on the image-level HEALTHCHECK. Do NOT add `healthcheck: disable`.
    image: ghcr.io/prorochestvo/httpproxy:${IMAGE_TAG}
    ports:
      - "127.0.0.1:8080:8080"
      - "127.0.0.1:8081:8081"
    volumes:
      - ./configs:/etc/httpproxy:ro
      - ./logs:/var/log/httpproxy
    restart: unless-stopped
```

`${IMAGE_TAG}` has no default — if unset, Compose fails loud. This is
intentional.

**6. First-time manual smoke.**

Before allowing GitHub Actions to drive deploys, confirm the setup
end-to-end.

On **staging** — use `:edge` if it already exists on GHCR (i.e.,
`staging.yml` has run at least once from a feature branch or previous
merge), otherwise use the latest semver tag:

```bash
sudo -iu httpproxy
cd /opt/httpproxy
IMAGE_TAG=edge docker compose up -d --wait --wait-timeout 120
curl -sS http://127.0.0.1:8081/healthz | jq .
# expected: {"status":"ok","handshake_age_seconds":N}
```

On **production** — use the latest semver tag already on GHCR (e.g.
`1.0.0`):

```bash
sudo -iu httpproxy
cd /opt/httpproxy
IMAGE_TAG=1.0.0 docker compose up -d --wait --wait-timeout 120
curl -sS http://127.0.0.1:8081/healthz | jq .
# expected: {"status":"ok","handshake_age_seconds":N}
```

If `--wait` times out before the container becomes healthy,
`docker compose logs httpproxy` is the first diagnostic. Check that the
`.conf` file is valid and that the WireGuard peer is reachable.

> **GHCR package visibility:** the first workflow run that publishes new
> tag shapes creates a private GHCR package (or adds private tags to an
> existing one). `docker compose pull` on the server will fail with
> `unauthorized` until you flip the package to public in the GitHub UI
> (see §3 of this README). Do this before running the first GHA-driven
> deploy on each host.

### 8. GitHub secrets, variables, and environments

The deploy jobs read configuration from **GitHub Environments** — one per
deployment target. Secrets and variables are scoped to their environment:
the `SSH_PRIVATEKEY` secret in the `STAGE` environment is a different
value from the `SSH_PRIVATEKEY` secret in the `PRIME` environment,
and the staging deploy job cannot read the production one (and vice versa).
The variable names are identical across both environments; the
environment scoping is what keeps them apart.

#### Creating the GitHub Environments

1. Go to `repo → Settings → Environments`.
2. Create an environment named **`STAGE`**.
   - No protection rules — staging deploys are automatic and unsupervised.
   - Add the `SSH_*` and (optionally) `TELEGRAM_*` secrets and variables
     listed below, using the staging-host values.
3. Create an environment named **`PRIME`**.
   - Under "Deployment protection rules", enable "Required reviewers" and
     add the repo owner (yourself). This creates a manual approval gate:
     every `v*` tag push pauses before the deploy step until you click
     "Approve and deploy" in the Actions UI. Five seconds of
     deliberateness per production deploy; worth it.
   - Add the same set of secrets and variables, using the production-host values.

#### Full secret and variable inventory

The `SSH_*` and `TELEGRAM_*` names are created in **each environment**;
only the values differ between STAGE and PRIME. `REMOTE_DIR` is a
**repository-level variable** because both hosts use the same path
(`/opt/httpproxy`); override per environment if any host differs.

| Name | Scope | Type | Purpose | Example value |
|---|---|---|---|---|
| `SSH_PRIVATEKEY` | Environment | Secret | ed25519 private key for the target host | full key contents |
| `SSH_HOSTNAME` | Environment | Variable | hostname or IP of the target VPS | `staging.example.com` / `proxy.example.com` |
| `SSH_HOSTPORT` | Environment | Variable | SSH port of the target VPS (use the real port, not 22) | `2222` |
| `SSH_USERNAME` | Environment | Variable | SSH user on the target VPS | `httpproxy` |
| `REMOTE_DIR` | Repository (or Environment override) | Variable | Absolute path on the host containing `compose.yml`; the deploy step `cd`s here before `docker compose pull` | `/opt/httpproxy` |
| `TELEGRAM_TOKEN` | Environment | Secret (optional) | Bot token used by the post-deploy notify steps | `123456:AbCdEf…` |
| `TELEGRAM_ROOT_CHAT_ID` | Environment | Variable (optional) | Telegram chat ID that receives deploy notifications | `-1001234567890` |

The Telegram pair is optional — if either value is empty, the notify steps
short-circuit and no message is sent. The deploy itself never depends on
Telegram reachability.

**Why variables (not secrets) for host/user/port?** None of
these values are sensitive. Making them variables keeps them visible in
workflow logs, which is useful when debugging "did this deploy hit the
right host?" Storing them as secrets would suppress them from logs with
no security benefit.

**Note on host-key verification.** The workflow does NOT pin the server's
SSH fingerprint — `appleboy/ssh-action` accepts whatever host key the
server presents at deploy time (TOFU). MITM-resistance therefore depends
on the path between GitHub's runner and your VPS not being hijacked.
This matches the `fx_rate_monitor` deploy style and trades fingerprint
bookkeeping for simpler setup. If you want strict pinning, switch the
SSH step to a manual `ssh -o StrictHostKeyChecking=yes` with a
pre-pinned `known_hosts` entry.

#### Generating the deploy keys

**Critical: generate a separate ed25519 key pair for each host.** Do NOT
reuse the same key on both staging and production — a leaked key would
then compromise both environments. Two hosts, two keys, no exceptions.

```bash
# staging key — run on your workstation
ssh-keygen -t ed25519 -C "httpproxy-deploy-staging@$(hostname)" \
  -f ~/.ssh/httpproxy_staging_deploy -N ""

# production key — run on your workstation
ssh-keygen -t ed25519 -C "httpproxy-deploy-prod@$(hostname)" \
  -f ~/.ssh/httpproxy_prod_deploy -N ""
```

`-N ""` creates a passphrase-less key, which is intentional — unattended
deploys require a key that can be used without interactive input.

**Installing the public key on each server:**

```bash
# staging host
ssh-copy-id -i ~/.ssh/httpproxy_staging_deploy.pub httpproxy@<staging-host>

# production host
ssh-copy-id -i ~/.ssh/httpproxy_prod_deploy.pub httpproxy@<prod-host>
```

**Adding the private keys to GitHub:**

Paste the full contents of each private key file (not the `.pub` file) into
the corresponding secret in each environment. The value **must** include the
`-----BEGIN OPENSSH PRIVATE KEY-----` and `-----END OPENSSH PRIVATE KEY-----`
lines and a trailing newline. Truncating either marker breaks the
`appleboy/ssh-action` parser with a cryptic error.

#### Upgrading from the prefixed-variable setup

If you previously set up the repo with `STAGING_HOST`/`STAGING_USER`/
`STAGING_FINGERPRINT`/`STAGING_SSH_KEY` and the corresponding `PROD_*`
names, rename them to the prefix-less form:

1. In `repo → Settings → Environments → STAGE`, recreate the variables
   under the new names (`SSH_HOSTNAME`, `SSH_HOSTPORT`, `SSH_USERNAME`)
   and the secret under `SSH_PRIVATEKEY`. `SSH_HOSTPORT` is new — fill
   it with the actual SSH port of the host (use `22` only if the daemon
   really listens there). `SSH_FINGERPRINT` is no longer used — drop it.
2. Same for `PRIME`.
3. Delete the old prefixed entries from each environment. Leaving them in
   place is harmless but misleading — they are no longer read.

If you previously set up the repo with the even older `DEPLOY_SSH_KEY` /
`SERVER_HOST` / `SERVER_USER` / `SERVER_FINGERPRINT` as repository-level
secrets/variables, delete those after the migration too — they are
likewise unread.

**Key rotation procedure** (trigger on workstation loss, sale, wipe, or
periodic scheduled rotation; repeat per environment):

1. Generate a new ed25519 key pair on your workstation (command above).
2. Add the **new** public key to `~httpproxy/.ssh/authorized_keys` on the
   target server **before** removing the old one.
3. Update the `SSH_PRIVATEKEY` secret in the corresponding GH Environment
   (`staging` or `production`) with the new private key.
4. Trigger a deploy (push to main for staging; push a throwaway tag for
   production) and confirm the workflow run is green.
5. Remove the **old** public key from `authorized_keys` on the server.

Always add before removing — reverting step 3 is easy if step 4 fails; a
server locked out by premature deletion is not.

### 9. Rollback

Rollback is manual and re-uses the same deploy pipeline. Automated
rollback is deliberately not implemented: if `docker compose up --wait`
times out, the container failed its healthcheck for a reason. Auto-reverting
masks that bug. The right response is to look at
`docker compose logs httpproxy`, diagnose, fix, and re-deploy forward.

**Source of truth for "what's running" (on either host):**

```bash
docker inspect httpproxy --format='{{.Config.Image}}'
```

Not any file. Not `compose.yml`. Not the workflow run history.

#### Staging rollback

**Primary path:** push a fix commit to `main`. The next `staging.yml` run
supersedes the broken one automatically.

**Fallback (if the new code is bad but you need immediate relief):** re-run
an older `staging.yml` workflow run from the GH Actions UI. Because every
previous run also published an immutable `:main-<sha>` tag, the re-run
re-pulls that specific image and rolls the staging host back to the
known-good state. `:edge` alone would not work here — GHCR has overwritten
it. The immutable per-commit tag is the only durable rollback handle.

#### Production rollback

**Primary path:** push a new tag pointing at an older commit.

```bash
# find the SHA you want to roll back to
git log --oneline --tags | head

# push a new tag pointing at that commit
git tag v1.2.4-rollback <oldsha>
git push origin v1.2.4-rollback
```

The workflow re-runs end-to-end: test → build → publish → approval gate →
deploy. Use a fresh tag — re-pushing an existing tag requires deleting and
re-creating it on both git and GHCR, breaks immutability expectations, and
confuses anyone who pulled by the original exact version.

**Fast fallback (image already on GHCR, no time to wait for a rebuild):**
SSH manually and re-deploy by tag:

```bash
ssh httpproxy@<prod-host>
cd /opt/httpproxy
IMAGE_TAG=1.2.2 docker compose up -d --wait --wait-timeout 120
```

This bypasses the approval gate and the pipeline entirely, but rolls the
host immediately to a known-published image.

**Anti-pattern — do not use `git tag --force`:** moving an existing tag
pointer to a different SHA reuses an immutable identifier, breaks GHCR's
expectations for that tag, and confuses anyone who pulled the original
image. Document it in a post-incident note; don't do it.

**Expected timeline (primary path):** ~3–4 minutes from `git push` to
served traffic on the rolled-back image (add ~1 minute for the approval
click).

**Soft floor:** tags built before Plan 006 shipped do not have a `deploy`
job in their `release.yml`. Tagging an ancient pre-006 commit will rebuild
and push the image but will not deploy it to the server automatically. Only
tags built after Plan 006 merged are safe rollback targets via the pipeline.

**`:latest` semantics post-rollback:** the `release.yml` pipeline publishes
`:latest` against the most-recently-pushed tag. After a rollback tag is
pushed, `:latest` resolves to the rolled-back image. This is consistent —
"the last `docker push` wins".

### 10. Hardening follow-ups (out of scope for current plans)

None of the items below are implemented. They are tracked here as future
work. Implementing any of them is a separate plan.

- **SHA-pin actions in workflow files** — all workflow files are currently
  on floating major-version pins (`@v4`, `@v5`, `@v6`). Pinning to specific
  commit SHAs prevents a supply-chain compromise from running arbitrary code
  in CI. Migrate all three files at once; piecemeal sends mixed signals.

- **`command=` restriction in `authorized_keys`** — adding a `command=`
  prefix limits what an attacker can do if a deploy key is leaked. Blast
  radius shrinks from "interactive shell as `httpproxy`" to "can re-run the
  deploy script". Requires the deploy script to be stable; any workflow
  change must update the `command=` in lockstep.

- **`fail2ban` on the SSH port** — brute-force mitigation against
  credential stuffing. Orthogonal to the deploy key mechanism.

- **Off-machine log shipping** (Loki, Vector, S3, etc.) — logs currently
  live only on each server. A compromised or crashed host loses its
  forensic trail.

- **Tailscale or WireGuard for the SSH channel** — pulling SSH inside an
  overlay network eliminates the public-internet SSH attack surface
  entirely. More infrastructure overhead; justified when the servers have
  other services or a higher threat model.

- **Staging implies signal, not guarantee** — staging runs on its own VPS
  with its own WireGuard `.conf`. A bug that only manifests with the
  production `.conf` (e.g. a provider-specific MTU quirk) will not be
  caught on staging. Staging is a signal-strength multiplier, not a
  bug-free guarantee.

</details>

## Observability

The daemon emits two log streams:

**Operational log** (stdout, slog):

- Lifecycle: startup, config load, tunnel handshake, graceful shutdown.
- Auth failures: `client_addr`, `method`, `target`, `reason` (one of
  `missing`, `wrong_scheme`, `wrong_token`, `malformed`). **Never** the
  token itself.

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

The access log has no `Authorization` or `Proxy-Authorization` field by
design. The Bearer token never appears in any log stream.

**Health endpoint** — `GET /healthz` on `admin.listen` (default
`127.0.0.1:8081`) returns 200 when the WireGuard tunnel handshake is
within `health.handshake_max_age`, otherwise 503. See the
[Troubleshooting](#troubleshooting) table for the body shapes and `reason`
enum. The endpoint is unauthenticated; keep it on loopback unless you
intend to expose tunnel-uptime to the network.

## Security

**Threat model (brief).** `httpproxy` hides your egress IP from upstream
origins by routing every byte through a WireGuard exit. It does **not**
encrypt the client → proxy hop (use it on loopback, an SSH tunnel, or a
Tailscale / WireGuard overlay network), does **not** prevent client-side
DNS leaks (the client must resolve through the proxy or DoH), and does
**not** protect against traffic analysis by your WireGuard provider.

**Secrets discipline.**

- WireGuard private keys live in `.conf` files in `./configs/tunnels/`, mode
  `0400`, gitignored.
- Bearer tokens live in `./configs/auth/`, mode `0400`, gitignored.
- Neither value appears in log output, error messages, or HTTP response
  bodies. Auth-failure responses are uniform — `407` with body
  `Proxy authentication required.\n`, no diagnostic that leaks token shape.
- Deploy SSH keys are ed25519, passphrase-less, **dedicated per server** —
  never reuse across environments.
- Token comparison uses `crypto/subtle.ConstantTimeCompare`. No length
  oracle, no early exit.

**Reporting vulnerabilities.** See [`SECURITY.md`](./SECURITY.md) for full
threat model, supported versions, dependency policy, and the responsible
disclosure procedure. Short version: open a private GitHub Security
Advisory or email `seilbekskindirov@gmail.com`; do not file public issues
for unpatched vulnerabilities.

## License

MIT. See [`LICENSE`](./LICENSE).
