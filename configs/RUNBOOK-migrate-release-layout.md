# Host migration runbook — release-layout

One-time operator procedure to move an existing `/opt/vpntunnel` host from the old
"versioned binary file + single `vpntunnel` symlink" layout onto the artifacts/bin
release-layout the deploy workflow now expects. Run it once, as root, on the
production host. After it, every `v*` tag deploys with no further host changes.

Substitutions used below: `APP=/opt/vpntunnel` (the `REMOTE_DIR` PRIME var), and the
CI/service user `github_aide`. If your current deploy user has a different name,
rename it to `github_aide` (or adjust every reference here and set the `SSH_USERNAME`
PRIME var accordingly) before starting.

## 0. Preconditions

- `SSH_USERNAME` PRIME GH variable is `github_aide`.
- `github_aide` exists on the host and is the user the deploy SSHes in as.
- The current live binary and `configs/` tree are in place under `$APP`.

## 1. Stop touching the old paths (no downtime yet)

The service keeps running on the old `$APP/vpntunnel` symlink throughout step 2–5.
Do not delete it until step 6 swaps the unit over.

## 2. Build the new structure

```bash
APP=/opt/vpntunnel
CI=github_aide

# immutable artifact store + channel dir, owned by the CI/service user
install -d -o "$CI" -g "$CI" -m 0755 "$APP/artifacts"
install -d -o "$CI" -g "$CI" -m 0755 "$APP/bin"

# seed an initial version from the currently-running binary so bin/release is a
# valid symlink before the first post-migration deploy (its rollback readlink
# needs a target). Use a UTC timestamp + the running version.
VER="$("$APP/vpntunnel" -version 2>/dev/null || echo 0.0.0)"   # or read it from your records
VID="$(date -u +%Y%m%d%H%M%S)-r_${VER}"
install -d -o "$CI" -g "$CI" -m 0755 "$APP/artifacts/$VID"
install -o "$CI" -g "$CI" -m 0755 "$(readlink -f "$APP/vpntunnel")" "$APP/artifacts/$VID/vpntunnel"
ln -sfn "../artifacts/$VID" "$APP/bin/release.tmp"
mv -Tf "$APP/bin/release.tmp" "$APP/bin/release"
```

## 3. Service-writable state, logs, TLS

```bash
APP=/opt/vpntunnel
CI=github_aide

install -d -o "$CI" -g "$CI" -m 0750 "$APP/state"   # async.db lives here
install -d -o "$CI" -g "$CI" -m 0750 "$APP/logs"    # access.log
install -d -o "$CI" -g "$CI" -m 0700 "$APP/configs/tls"   # self-signed cert
# move an existing async.db / access.log into state/ and logs/ if they were elsewhere.
```

`ProtectSystem=strict` blocks creating files outside `ReadWritePaths`, and the
tunnel-id HMAC key is auto-generated only if absent. To avoid a cold-start crash,
pre-generate it (exactly 64 random bytes, mode 0600, owned by the service user —
the daemon enforces 0600 and reads it as `github_aide`):

```bash
APP=/opt/vpntunnel
CI=github_aide

if [ ! -f "$APP/configs/auth/tunnel-id.key" ]; then
  install -d -o "$CI" -g "$CI" -m 0700 "$APP/configs/auth"
  umask 077
  head -c 64 /dev/urandom > "$APP/configs/auth/tunnel-id.key"
  chown "$CI:$CI" "$APP/configs/auth/tunnel-id.key"
  chmod 0600 "$APP/configs/auth/tunnel-id.key"
fi
```

## 4. Ownership and modes (base dir locked, secrets owned by the service user)

The daemon enforces, at startup: each `*_token` file is mode 0600 **and owned by
the process UID**; `tunnel-id.key` is 0600; each `.conf` is 0600; the TLS cert dir
is exactly 0700. Because the service runs as `github_aide`, every file it reads or
writes must be `github_aide`-owned — including the entire `configs/` tree. This is
the direct consequence of running the service as the deploy user (CI == SVC): the
deploy user owns the service's secrets. Only the base dir, `.env`, and the
unit stay root-owned (the unit lives in `/etc/systemd/system/`).

```bash
APP=/opt/vpntunnel
CI=github_aide

# base dir: root-owned so CI cannot create/replace top-level entries
chown root:root "$APP"
chmod 0755 "$APP"

# env file: read by systemd (root) BEFORE launching the service, never by the
# service itself — so it stays root:root 0600. No secrets, only paths.
cp "$APP/configs/env.example" "$APP/.env"   # first time only; then edit by hand
chown root:root "$APP/.env"
chmod 0600 "$APP/.env"

# configs/ is the service's own tree — owned by the service user so the daemon's
# owner==self token check passes and 0600 secrets are readable.
chown -R "$CI:$CI" "$APP/configs"
chmod 0700 "$APP/configs/auth" "$APP/configs/tls"
chmod 0600 "$APP/configs/auth/"*_token "$APP/configs/auth/tunnel-id.key" 2>/dev/null || true
chmod 0600 "$APP/configs/tunnels/"*.conf 2>/dev/null || true
```

> **Blast radius (CI == SVC == github_aide).** A leaked deploy key can flip
> `bin/release` to a malicious binary, restart the service (via the narrow
> sudoers), and read anything `github_aide` can read — the WireGuard `.conf`
> files, the auth tokens, and `tunnel-id.key`. It **cannot** write `.env`,
> the unit, or escalate to root (the unit pins `User=github_aide`, and systemd —
> not the service — reads the root-owned env file). This is the accepted trade-off
> of not creating a dedicated runtime user; it is strictly better than the prior
> root service but weaker than a CI/SVC split.

## 5. Narrow sudoers grant

```bash
APP=/opt/vpntunnel
install -m 0440 "$APP/configs/vpntunnel.sudoers" /etc/sudoers.d/vpntunnel-deploy
visudo -c                                   # must report "parsed OK"
# remove any prior broad grant for the deploy user
grep -rE 'NOPASSWD|github_aide' /etc/sudoers /etc/sudoers.d/
```

## 6. Swap the unit over (the one short restart)

```bash
APP=/opt/vpntunnel
cp "$APP/configs/vpntunnel.service" /etc/systemd/system/vpntunnel.service
systemctl daemon-reload
systemctl restart vpntunnel
systemctl is-active vpntunnel
curl -k -H "X-Vpntunnel-Token: <admin-token>" https://127.0.0.1:8888/v1/admin/health
```

The new unit's `ExecStart` now points at `bin/release/vpntunnel`. Once health is
green, the old `$APP/vpntunnel` symlink and any `vpntunnel.v*` files are dead weight.

## 7. Clean up the old layout

```bash
APP=/opt/vpntunnel
rm -f "$APP/vpntunnel"                 # old channel symlink
rm -f "$APP"/vpntunnel.v*              # old versioned binaries (now in artifacts/)
```

## Rollback during migration

Until step 6, the old symlink still drives the running service — abort by simply not
swapping the unit. After step 6, roll back by pointing `bin/release` at a prior
`artifacts/<VID>` (or restoring the old unit) and `systemctl restart vpntunnel`.
