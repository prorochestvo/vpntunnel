# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project State

`vpntunnel` is a lightweight forward HTTP proxy that routes all egress through a
userspace WireGuard tunnel (no root required). v6 ships one binary deployed to the
production host as a systemd service. One environment, one host: production deploys
on every `v*` tag after a manual approval gate. Pushes to `main` and PRs against
`main` run lint + tests only — no binary build, no deploy. Server-side state lives
at `/opt/vpntunnel/` on the host and is hand-authored once during setup.

Operator flow (binary): download a wg-quick `.conf` from mullvad.net or any WireGuard
provider → drop it into `./configs/tunnels/` — it is auto-discovered on the next startup;
no change to `configs/proxy.json` is needed → run `./build/vpntunnel -config configs/proxy.json`
(plain HTTP mode locally; no TLS flags needed for dev). Production passes `-tls-cert-dir`
explicitly on the systemd `ExecStart` line; see the Deployment section.

### Binaries

- `cmd/vpntunnel/` — long-running daemon; composition root wired inline in `main.go`.

### Layer table

| Layer | Package(s) | Responsibility |
|-------|-----------|----------------|
| Types | `internal/domain` | Pure value types (`RequestSummary`). No I/O. |
| Cross-cutting | `internal/config`, `internal/publicerror` | JSON config loading/validation; user-facing error wrapper. |
| Auth | `internal/auth` | `Verifier` interface + `BearerVerifier`; constant-time token check. |
| Egress | `internal/tunnel` | `Dialer` + `DialerCloser` + `HealthReporter` interfaces. |
| WireGuard | `internal/tunnel/wireguard` | Userspace WireGuard dialer via wireguard-go + gVisor netstack. |
| wg-quick parser | `internal/tunnel/wireguard/wgconf` | Parses `[Interface]`/`[Peer]` `.conf` files into `ParsedConfig`. |
| Observability | `internal/observability` | Operational slog logger (stdout) + lumberjack rotating access log (JSONL). |
| Notifications | `internal/notify` | `Notifier` interface + `Nop` + `TelegramNotifier`; reports tunnel changes to Telegram. |
| Orchestration | `internal/service` | `ProxyService`: `HandleHTTP`, `HandleCONNECT`, `WaitTunnels`. |
| Transport | `internal/transport/httpserver` | Thin `*http.Server` wrapper, graceful shutdown. |
| Health + API | `internal/transport/apiserver`, `internal/transport/apiserver/handlers` | HTTP/HTTPS API listener (plain HTTP when -tls-cert-dir empty; TLS 1.3 when set), request-ID, role-based auth, routing; multi-tunnel `/v1/admin/health` handler. |

### HTTP routes

The proxy listener (default `127.0.0.1:7788`) dispatches `CONNECT` to `HandleCONNECT`
(hijack + bidirectional copy) and everything else to `HandleHTTP` (absolute-URI forward,
strips hop-by-hop headers). When auth is configured, every request is challenged with
`407 Proxy Authentication Required` before any hijack or forwarding occurs; the
`Proxy-Authorization: Bearer <token>` header is required (except clients connecting from
the loopback interface, which bypass the token check).

The API listener (default `127.0.0.1:8888`; HTTP when `-tls-cert-dir` is empty, HTTPS otherwise)
requires one of two Bearer tokens sent via `X-Vpntunnel-Token`. There are two roles: admin
(full access) and proxy (forward-proxy + tunnels list/use). The tunnel is selected by the `{id}` path
segment (the HMAC tunnel id); there is no `X-Tunnel-Id` header. Routes:

| Method | Path | Roles |
|--------|------|-------|
| `GET` | `/v1/admin/health` | admin |
| `POST` | `/v1/admin/rotate` | admin |
| `GET` | `/v1/tunnels` | admin, proxy |
| `*` | `/v1/tunnels/{id}/proxy/{scheme}/{rest...}` | admin, proxy |

All other paths return 404 with a JSON error envelope. Every response carries
`X-Request-Id` (UUIDv7) and `X-Proxy-Error` on error paths.

`/v1/admin/rotate` triggers a graceful streaming-tunnel rotation: gated on zero
active **streaming** sessions unless `?force=true`. It always does
break-before-make + settle (tear the current exit down, wait the on-demand
settle delay, then bring a fresh random exit up) — the streaming role
self-limits to ≤1 session, so on-demand is intentionally not consulted and the
host's 2-device ceiling holds with no cross-role coordination.

`/v1/tunnels` returns the FULL discovered catalog grouped by country as a
`map[country][ids]` JSON object — it is a static catalog, not a live-device report, and
contains no `healthy` or `handshake_age` fields. An optional `?country=us,se` query parameter
narrows the result; an unmatched filter returns `{}` with 200. With lazy building enabled,
`/v1/admin/health` still reports only the currently live devices (1 streaming + 0..1 on-demand);
its body shape is unchanged (the fixed shape is specified in Constraints).

### Config schema (v6)

Proxy-egress config lives under `vpnstream`; on-demand VPN proxy config lives under
`api.vpn`. TLS settings are CLI flags, not part of `proxy.json`. Tunnels are
auto-discovered from `<configDir>/tunnels/`.

The full config shape (every key + default) is the `raw*` struct set in `internal/config/config.go`, mirrored by the committed `configs/proxy.json`. Only the non-obvious semantics are documented below.

`tunnel_id_hmac_key_file` — optional top-level field (default `./auth/tunnel-id.key`), resolved
relative to the config directory by the binary. Points to a 0600 file holding 64 random bytes.
Generated on first run if absent; never rewritten to `proxy.json`. The KEY MATERIAL (file
contents) must never be logged — only the basename and `key_len` may appear in startup logs.
The derived tunnel id is `HMAC-SHA256(key, conf_basename)` hex-encoded (64 lowercase chars) and
is non-secret: it appears in `/v1/tunnels` responses and in the `{id}` path segment.

`vpnstream.allowed_countries` — optional list of two-letter lowercase country codes.
**Scope: streaming only.** An empty list (or the field absent) means all discovered
configs are eligible for the always-on streaming random-pick. A code that matches no
config is silently dropped; the daemon errors at startup only when the resulting
country-filtered set is empty (streaming is mandatory and its empty-set panics). The
on-demand scheduler (`/v1/tunnels/{id}/proxy/...`) accepts **any** discovered tunnel regardless of
`allowed_countries` — it uses the full unfiltered set. The single-key guard
(`VerifySingleKey`) runs over the full discovered set, so all `.conf` files must share
one WireGuard key.

Tunnel discovery scans `<configDir>/tunnels/` for top-level `*.conf` files. Only
regular files (no symlinks) with a `.conf` suffix are included; dotfiles (including
`.gitkeep`) and subdirectories are ignored. A missing tunnels directory is a startup
error. An empty directory is also a startup error. Unparseable `.conf` files are
warn-skipped at device-build time — they do not cause startup to fail.

`vpnstream.reconnect_min` / `vpnstream.reconnect_max` — initial and maximum
exponential backoff between reconnect attempts for the always-on streaming device.

`api.vpn.demand.grace` — after the last active job on the current zone finishes, how
long the scheduler stays on that zone (each new same-zone request resets the window)
before switching to the next zone with a backlog. Default 10s.
`api.vpn.demand.settle_delay` — mandatory pause between tearing the current on-demand
device down and bringing the next zone's device up, so the provider frees the old
session before the new one starts (avoids transiently exceeding the connection budget).
Default 15s, minimum 5s.
`api.vpn.demand.idle_ttl` — how long the on-demand device is kept live with no active
job before it is torn down; a request for a different zone tears it down immediately
regardless. Default 168h.

`api.vpn.async.storage_path` — path for async job state (default `/opt/vpntunnel/state/async.db`). The async TTL/concurrency knobs are built-in constants in `internal/asyncjob`, not configurable.

`vpnstream.auth` — optional proxy bearer token (pick one of `token` or `token_file`;
both set is a config error). Missing means proxy auth disabled.

`handshake_max_age` is no longer configurable — it is `lazy.DefaultHandshakeMaxAge = 180s`,
which is ~3× the 25s persistent keepalive plus a safety margin so quiet tunnels do not flap.

WireGuard parameters are the standard wg-quick `[Interface]`/`[Peer]` fields, read from the `.conf` via the `wgconf` parser.

`api` block is required. All other top-level blocks are optional — missing → defaults
apply. `api.vpn` absent → all vpn defaults applied. `vpnstream` absent → all vpnstream
defaults applied.

**TLS CLI flags** (not part of `proxy.json`):

| Flag | Default | Description |
|------|---------|-------------|
| `-tls-cert-dir` | `""` (HTTP mode) | Directory holding the API TLS cert/key. Empty = plain HTTP (loopback/dev only). Production must pass `/opt/vpntunnel/tls/`. Relative resolves against cwd. |
| `-tls-hostname` | `localhost` | TLS server name embedded in the API certificate. |
| `-tls-ip-sans` | `` | Comma-separated list of IP Subject Alternative Names. |

### Tunnel storage

`.conf` files live in `./configs/tunnels/` (gitignored). Only
`./configs/tunnels/.gitkeep` is tracked. Treat each `.conf` like an SSH private
key (mode 0600, never commit to a shared repository).

### Dependencies

Third-party modules live in `go.mod`. The only non-obvious pin: `gvisor.dev/gvisor` must stay at `v0.0.0-20250503011706-39ed1f5ac29c` — it is a transitive dep of wireguard-go's `tun/netstack` (pulled in whether or not the binary is containerized; adds ~20MB to the binary and ~10-50MB RSS), and newer revisions hit a "two packages in same dir" build error.

### Deployment

One static binary (`CGO_ENABLED=0`), deployed to the production host as a systemd
unit at `/etc/systemd/system/vpntunnel.service`. The on-host layout follows the
standard release-layout: an immutable per-version artifact store plus a named
channel symlink, with the config tree at `$REMOTE_DIR/configs/`.

```
$REMOTE_DIR/                     root:root 0755    base dir, CI cannot create top-level entries
    .env                root:root 0600    read by systemd, NOT the service; operator-managed
    configs/                     root:root 0755    service's own tree (auth/tls/tunnels are 0700)
    state/  logs/                root:root 0750    async.db, access.log (root writes)
    artifacts/<VERSION_ID>/vpntunnel   github_aide 0755, immutable build store
    bin/release -> ../artifacts/<VERSION_ID>       github_aide, relative channel symlink
```

The daemon enforces at startup that each auth token is mode 0600 **and owned by the
process UID**, the tunnel-id HMAC key and each `.conf` are 0600, and the TLS cert
dir is 0700. The service runs as root, so the whole secret tree is root-owned and
the owner==self check passes against UID 0. The base dir stays `0755` (not `0700`)
so the CI user `github_aide` can traverse into its own `artifacts/` and `bin/`
(it is not in the `root` group); secret isolation comes from the `0700` root-owned
`configs/auth`, `configs/tls`, and `configs/tunnels` subdirs, which `github_aide`
cannot open. Only `artifacts/` and `bin/` are `github_aide`-owned.

`VERSION_ID = <YYYYMMDDhhmmss UTC>-r_<version>` (tag with leading `v` stripped). One channel, `release`. The workflow scps the binary into a fresh `artifacts/$VERSION_ID/`, verifies its SHA256 there (a mismatch removes only that dir), then atomic-swaps the relative `bin/release` symlink. A failed `systemctl` restart OR post-deploy `/v1/admin/health` check auto-rolls the channel back to the previous `VERSION_ID` and restarts. Retention keeps the 3 newest dirs and never prunes a live channel's target. Pre-release tags deploy identically.

The service runs as **root**, matching the rest of the fleet (`hive_scout`,
`beacon`). Root is not a runtime requirement — userspace WireGuard needs no root
and both listeners bind loopback ports >1024 — it exists so the secret tree is
root-owned and unreadable to the deploy identity. The CI deploy user `github_aide`
writes **only** under `artifacts/` and `bin/` and can no longer read any secret
(the `0700` root-owned `configs/auth|tls|tunnels`, `state/`, and `logs/` are
closed to it). The one privileged action it needs, `systemctl restart`, is granted
by the narrow `configs/vpntunnel.sudoers` (install once to
`/etc/sudoers.d/vpntunnel-deploy`). Trade-off: a remote-code-execution bug in the
public proxy now yields root rather than `github_aide`; this is accepted per the
fleet decision and partially offset by `ProtectSystem=strict`, `NoNewPrivileges`,
and `PrivateTmp`. Residual: the binary in `artifacts/` is `github_aide`-owned and
executed by root, so a leaked deploy key can still reach root via a binary swap +
restart — tracked as a follow-up (root-owned binary slot + privileged promote),
not closed here.

The operator path is `make init`, run as a sudo-capable account (`pi5_aide` with
password sudo) — **not** the `github_aide` CI key. It stages the repo-managed
files into `/tmp` and runs `configs/provision-host.sh` under `sudo bash`, which
provisions the root-owned tree, generates absent secrets without rotating
existing ones, and does the chown + unit swap + restart atomically.

The release health-check uses the `VPNTUNNEL_ADMIN_TOKEN` GH secret (scoped to the
`PRIME` environment). It must exist before the next `v*` tag or the post-deploy
health-check fails.

TLS settings are **not** in `proxy.json` — they are CLI flags. The systemd unit
sources them from `/opt/vpntunnel/.env` (`EnvironmentFile`), which is
**operator-managed and hand-authored once** — the deploy no longer rewrites it (the
CI user cannot write the base dir, and the values are stable because `ExecStart`
points at the fixed `bin/release` symlink, not a per-version path):

```
EnvironmentFile=/opt/vpntunnel/.env
ExecStart=/opt/vpntunnel/bin/release/vpntunnel -config ${VPNTUNNEL_CONFIG_PATH} \
  -tls-cert-dir ${VPNTUNNEL_TLS_CERT_DIR} -tls-hostname ${VPNTUNNEL_TLS_CERT_HOST}
```

`EnvironmentFile` is re-read on each restart, so an env change needs only a restart,
no `daemon-reload`. Production's `VPNTUNNEL_TLS_CERT_DIR` is
`/opt/vpntunnel/configs/tls/`; the daemon generates a self-signed cert there on first
start. The unit (`configs/vpntunnel.service`), `configs/env.example`, and
`configs/vpntunnel.sudoers` are installed once by the operator — the deploy touches
none of them. The one-time host restructure onto this layout is the runbook in
`configs/RUNBOOK-migrate-release-layout.md`.

The same `.env` also carries the second (and so far only other) env-injected
setting: `VPNTUNNEL_TELEGRAMBOT_DSN`, read directly via `os.Getenv` in
`cmd/vpntunnel/main.go` (not a CLI flag, not part of `proxy.json`). It is
optional — unset disables the Telegram tunnel-change notifier entirely, and a
malformed value only warns and disables, it never blocks startup (the
notifier is auxiliary telemetry, not a startup precondition). Same
operator-hand-adds-and-restarts handling as the TLS vars above; see
`configs/env.example` for the DSN format.

`main` and PR pushes run `.github/workflows/ci.main.yml` (lint + test + sanity
`go build`). The CI workflow does not touch the host.

## Commands

A `Makefile` exists at the repo root with the standard targets:

```bash
make build            # builds ./build/vpntunnel
make build-vpntunnel  # ./build/vpntunnel
make test             # gofmt check + go vet + go test -race ./...
make lint             # go vet + forbidden-imports check
make fmt              # gofmt -w .
make run              # go run ./cmd/vpntunnel -config ./configs/proxy.json
                      # NOTE: requires at least one .conf in ./configs/tunnels/
make clean            # rm -rf ./build ./tmp/*.tmp
```

Run one-off tests with the standard `go test -race -run 'TestName/subtest' ./<pkg>/` forms. Pin `CGO_ENABLED=0` for `go build`/`go vet` and always pass `-o ./build/<name>`; leave `CGO_ENABLED` unset for `-race` (see Conventions).

## Conventions

Generic Go conventions (style, file declaration order, test structure, test-only
code placement, godoc, error discipline, code organization) come from the
`stack-go` plugin skills — they are not restated here. Error handling follows the
standard `PublicError` contract via the cross-cutting `internal/publicerror`
package: construct with `publicerror.New(...)`, match with `publicerror.Is`; every
controller error-branch test asserts the response text (public message when the
error is public, generic fallback otherwise). Project-specific constraints:

- **Never log WireGuard key material**: the private key and pre-shared key (PSK)
  must NEVER appear in any log call, error message, or string format. The private
  key lives only in the `.conf` file and in `wireguard.Options.PrivateKey` at
  runtime. The `./configs/tunnels/` directory is gitignored — keep it that way.
  Startup logs may include `peer_endpoint`, `local_address`, and the `.conf`
  basename (source) only.
- **Never log the Bearer token**: the proxy auth token (configured via `auth.token`
  or `auth.token_file`) must NEVER appear in any log call, error message, or string
  format. Startup logs may include `token_len` and `source` (inline or file basename)
  only. Auth failure logs record only a `reason` enum — never the attempted token
  value. The `./configs/auth/` directory is gitignored — keep it that way.
- **Never log the tunnel-id HMAC key**: the 32 raw bytes held in `tunnel_id_hmac_key_file`
  must NEVER appear in any log call, error message, or string format — not as raw bytes, not
  as hex. Startup logs may include the key file basename and `key_len` only. The derived
  hex tunnel ids (output of `HMAC-SHA256(key, basename)`) ARE non-secret and safe to log.
- **Never log the Telegram bot token, DSN, or admin chat id**: `VPNTUNNEL_TELEGRAMBOT_DSN`
  and everything parsed out of it are secret material and must NEVER appear in any log call,
  error message, or string format. Startup/status logs may include only `token_len` and
  whether the notifier is enabled/disabled.
- **Forbidden imports**: enforced by `make lint`; nothing is currently banned. Add a module
  here (and to the lint check) only when the team rejects one.
- **Race + CGO**: `make test`'s `-race` step deliberately does NOT pin `CGO_ENABLED` —
  Go 1.26's race detector links libtsan via cgo and refuses under `CGO_ENABLED=0`
  (Linux picks cgo, darwin uses its built-in detector). Production builds always pin
  `CGO_ENABLED=0` (static binary, no libc link).
- **`/v1/admin/health` response body is fixed**: top-level `{status, tunnels: [...]}`;
  each tunnel entry is `{id, healthy, handshake_age_seconds}`. Never include
  `peer_endpoint`, key material, peer public key, or `TunnelHealth.Err` text.
  The 503 body is the diagnostic surface for operators only; tightened scope prevents
  accidental leakage if the API port is ever exposed.
- **Deploy SSH key (`SSH_PRIVATEKEY` GH secret, scoped to the `PRIME` environment)
  is ed25519, dedicated to the production host, passphrase-less.** Never log
  private-key contents in workflow output. The deploy workflow populates the
  runner's `~/.ssh/known_hosts` via `ssh-keyscan` at deploy time (trust-on-first-use
  — there is no pinned host fingerprint). The SSH port lives in the env-scoped
  `SSH_HOSTPORT` variable, so non-standard ports are supported without code changes.

## Working agreement

All non-trivial work follows the plan-first pipeline:

1. **Plan** — the `architect` agent writes `plans/NNN-slug.md` (create via the
   `pipeline:new-plan` skill). No source edits before a plan exists.
2. **Implement** — the `engineer` agent executes the plan's tasks with tests.
3. **Review** — three `reviewer` agents launched in parallel in ONE message, each
   prompt naming its lens (A: correctness & tests, B: security & operations,
   C: performance & architecture) and the changed files. Full three-lens fan-out is
   mandatory on the first review; the post-fix re-review is ONE solo reviewer scoped
   to the changed lines.
4. **Gate** — `make test` must be green before review; a red tree goes to the
   `testdoctor` agent first, at any stage.
5. **Complete** — the orchestrator merges the three reports, deduplicates, resolves
   conflicting verdicts (naming what was rejected and why; the user has final say).
   P0/P1 findings loop back to the engineer. Only when every P0/P1 is fixed or
   explicitly accepted: move the plan via the `pipeline:complete-plan` skill.

Plans live in `plans/` (active), `plans/completed/` (shipped, `YYMMDD.NNNN.slug.md`),
`plans/history/` (abandoned/superseded). One plan per concern.
