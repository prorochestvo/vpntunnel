---
name: vpntunnel-deployment
description: The vpntunnel production host — the `/opt/vpntunnel/` release layout, its immutable `artifacts/<VERSION_ID>/` store and `bin/release` channel symlink, the root-owned secret tree confining the `github_aide` CI identity, the startup permission assertions, the operator-managed `.env` and unit, the `make init` / `make deploy-nginx` / `make healthz` targets, tunnel `.conf` storage, and the pinned gvisor revision. Load before touching `Makefile` deploy targets, `configs/provision-host.sh`, `configs/vpntunnel.*`, `configs/nginx/`, `DEPLOY.md`, `.github/workflows/ci.main.yml` or any release workflow; before changing `EnvironmentFile`, `ExecStart`, the systemd hardening, the `0700`/`0755` secret-tree modes, or the sudoers grant; before reading or writing any `VPNTUNNEL_*` variable or the `SSH_PRIVATEKEY` / `SSH_HOSTPORT` secrets; and before touching the `gvisor.dev/gvisor` pin in `go.mod`.
---

# vpntunnel deployment and host layout

One static binary (`CGO_ENABLED=0`), deployed to the production host as a systemd unit at
`/etc/systemd/system/vpntunnel.service`. One environment, one host.

**Production deploys on every `v*` tag after a manual approval gate.** Pushes to `main` and
PRs against `main` run `.github/workflows/ci.main.yml` — lint + test + a sanity `go build`,
no binary build and no deploy. **The CI workflow never touches the host.**

## On-host layout

Standard release layout: an immutable per-version artifact store plus a named channel symlink,
with the config tree at `$REMOTE_DIR/configs/`.

```
$REMOTE_DIR/                          root:root 0755   base dir, CI cannot create top-level entries
    .env                              root:root 0600   read by systemd, NOT the service; operator-managed
    configs/                          root:root 0755   service's own tree (auth/tls/tunnels are 0700)
    state/  logs/                     root:root 0750   async.db, access.log (root writes)
    artifacts/<VERSION_ID>/vpntunnel  github_aide 0755, immutable build store
    bin/release -> ../artifacts/<VERSION_ID>          github_aide, relative channel symlink
```

`VERSION_ID = <YYYYMMDDhhmmss UTC>-r_<version>` (the tag with its leading `v` stripped). One
channel, `release`. The workflow scps the binary into a fresh `artifacts/$VERSION_ID/`,
verifies its SHA256 **there** (a mismatch removes only that dir), then atomic-swaps the
relative `bin/release` symlink. A failed `systemctl` restart **or** a failed post-deploy
`/v1/admin/health` check auto-rolls the channel back to the previous `VERSION_ID` and
restarts. Retention keeps the 3 newest dirs and **never prunes a live channel's target**.
Pre-release tags deploy identically.

## Why the service runs as root

The service runs as **root**, matching the rest of the fleet (`hive_scout`, `beacon`). Root is
**not a runtime requirement** — userspace WireGuard needs no root and both listeners bind
loopback ports >1024. It exists so the secret tree is root-owned and unreadable to the deploy
identity.

The daemon enforces at startup that each auth token is mode 0600 **and owned by the process
UID**, that the tunnel-id HMAC key and each `.conf` are 0600, and that the TLS cert dir is
0700. Because the service runs as root the whole secret tree is root-owned and the
owner==self check passes against UID 0.

The base dir stays `0755` (not `0700`) so the CI user `github_aide` can traverse into its own
`artifacts/` and `bin/` — it is not in the `root` group. **Secret isolation comes from the
`0700` root-owned `configs/auth`, `configs/tls` and `configs/tunnels` subdirs**, which
`github_aide` cannot open, plus `state/` and `logs/`.

`github_aide` writes **only** under `artifacts/` and `bin/`. The one privileged action it
needs, `systemctl restart`, is granted by the narrow `configs/vpntunnel.sudoers` (installed
once to `/etc/sudoers.d/vpntunnel-deploy`).

**Accepted trade-off, and its open residue.** A remote-code-execution bug in the public proxy
now yields root rather than `github_aide`; this is accepted per the fleet decision and
partially offset by `ProtectSystem=strict`, `NoNewPrivileges` and `PrivateTmp`. **Residual,
not closed:** the binary in `artifacts/` is `github_aide`-owned and executed by root, so a
leaked deploy key can still reach root via a binary swap plus restart. Tracked as a follow-up
(root-owned binary slot + privileged promote).

## The operator path

`make init` is run **as a sudo-capable account (`pi5_aide`, password sudo) — not the
`github_aide` CI key.** It stages the repo-managed files into `/tmp` and runs
`configs/provision-host.sh` under `sudo bash`, which provisions the root-owned tree,
**generates absent secrets without rotating existing ones**, and does the chown + unit swap +
restart atomically. It then chains `make deploy-nginx`.

`make deploy-nginx` installs the Cloudflare-fronted edge vhost and reloads nginx. The CF
Origin Certificate and key are operator-placed once under `/etc/nginx/certificates/cloudflare/`
and are **never shipped from the repo**; if they are absent the vhost is staged but nginx is
**not** reloaded.

`make healthz` asserts over SSH that egress leaves through the Mullvad exit and that the
health plane reports ok. `make examination` PASS/FAILs each API route against a locally
running daemon, including the 401 no-token gate and the admin/user role split.

The one-time host restructure onto this layout is the runbook in
`configs/RUNBOOK-migrate-release-layout.md`.

`configs/vpntunnel.service`, `configs/env.example` and `configs/vpntunnel.sudoers` are
installed **once by the operator** — the deploy touches none of them.

## `.env` is operator-managed

TLS settings are **not** in `proxy.json`; they are CLI flags. The systemd unit sources them
from `/opt/vpntunnel/.env` via `EnvironmentFile`, which is **operator-managed and
hand-authored once** — the deploy no longer rewrites it. It cannot: the CI user cannot write
the base dir, and the values are stable because `ExecStart` points at the fixed `bin/release`
symlink rather than a per-version path.

```
EnvironmentFile=/opt/vpntunnel/.env
ExecStart=/opt/vpntunnel/bin/release/vpntunnel -config ${VPNTUNNEL_CONFIG_PATH} \
  -tls-cert-dir ${VPNTUNNEL_TLS_CERT_DIR} -tls-hostname ${VPNTUNNEL_TLS_CERT_HOST}
```

`EnvironmentFile` is re-read on each restart, so an env change needs only a restart, no
`daemon-reload`. Production's `VPNTUNNEL_TLS_CERT_DIR` is `/opt/vpntunnel/configs/tls/`; the
daemon generates a self-signed cert there on first start.

The same `.env` carries the second (and so far only other) env-injected setting:
**`VPNTUNNEL_TELEGRAMBOT_DSN`**, read directly via `os.Getenv` in `cmd/vpntunnel/main.go` —
not a CLI flag, not part of `proxy.json`. It is optional: unset disables the Telegram
tunnel-change notifier entirely, and **a malformed value only warns and disables — it never
blocks startup**, because the notifier is auxiliary telemetry rather than a startup
precondition. Same operator-hand-adds-and-restarts handling as the TLS vars. DSN format is in
`configs/env.example`.

## CI secrets

The release health-check uses the `VPNTUNNEL_ADMIN_TOKEN` GH secret, scoped to the `PRIME`
environment. **It must exist before the next `v*` tag** or the post-deploy health-check fails.

The deploy SSH key (`SSH_PRIVATEKEY` GH secret, also scoped to `PRIME`) is ed25519, dedicated
to the production host, passphrase-less. Never log private-key contents in workflow output.
The deploy workflow populates the runner's `~/.ssh/known_hosts` via `ssh-keyscan` at deploy
time — **trust-on-first-use, there is no pinned host fingerprint.** The SSH port lives in the
env-scoped `SSH_HOSTPORT` variable, so non-standard ports need no code change.

## Tunnel storage

`.conf` files live in `./configs/tunnels/` (gitignored); only `./configs/tunnels/.gitkeep` is
tracked. **Treat each `.conf` like an SSH private key** — mode 0600, never committed to a
shared repository. `./configs/auth/` is gitignored on the same terms. Keep both that way.

## The one dependency pin that matters

Third-party modules live in `go.mod`. The only non-obvious pin: **`gvisor.dev/gvisor` must
stay at `v0.0.0-20250503011706-39ed1f5ac29c`.** It is a transitive dependency of
wireguard-go's `tun/netstack` — pulled in whether or not the binary is containerized, adding
~20MB to the binary and ~10-50MB RSS — and newer revisions hit a "two packages in same dir"
build error.
