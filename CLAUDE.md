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
| `GET` | `/v1/tunnels` | admin, proxy |
| `*` | `/v1/tunnels/{id}/proxy/{scheme}/{rest...}` | admin, proxy |

All other paths return 404 with a JSON error envelope. Every response carries
`X-Request-Id` (UUIDv7) and `X-Proxy-Error` on error paths.

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

Run one-off tests with the standard `go test -race -run 'TestName/subtest' ./<pkg>/` forms. Pin `CGO_ENABLED=0` for `go build`/`go vet` and always pass `-o ./build/<name>`; leave `CGO_ENABLED` unset for `-race` (see Constraints).

## Code Organization Principles

These rules govern *where code lives*. Apply them by default; treat a violation as
something to flag, not silently accept.

### Package placement follows consumption, not aspiration

- Code shared by **multiple** binaries/entry points belongs in the shared tree
  (`internal/`).
- Code with **exactly one** consumer belongs **next to that consumer**
  (`cmd/<binary>/`), not in the shared tree.
- Prefer the private location (`internal/`) over a public one (`pkg/`) unless there
  is a **real external (out-of-module) consumer**. Don't promise a public API
  surface the project doesn't actually provide.
- **Why:** the shared tree is for genuinely shared layers; one consumer means co-locate, no external importer means keep it private.

### Deduplication is not a goal in itself

- Distinguish **coincidental similarity** (looks alike today but must be free to
  diverge) from a **genuine cross-cutting invariant**. Coincidental similarity →
  duplicate the few trivial lines and let each site evolve. A true invariant →
  centralize it once, where it belongs.
- Do **not** build a shared `bootstrap` / `startup` / `wiring` layer for multiple
  binaries just because their startup looks similar — inline it per entry point
  (`cmd/<binary>/main.go`) so each stays free to diverge (different DBs,
  dependencies, config).
- Before extracting a helper, check whether the only thing being shared is already
  captured elsewhere (e.g. already a one-line call) — if so, don't wrap it. An
  abstraction can re-introduce the very complexity it pretends to hide (e.g. a
  returning constructor needs error-cleanup that an inline fatal-and-exit path
  simply doesn't).
- **Why:** premature extraction imposes a contract where code should be free to diverge; centralize only for a named invariant or real divergence risk.

### Business logic is organized by concern, not by launcher

- Business-logic packages are judged by being **simple and isolated**, regardless of
  which binary runs them or how they are launched ("how it starts is not the
  package's concern"). Keep a flat, per-concern split.
- Do **not** reorganize business logic by runtime-vs-operator, by deployment, or by
  consuming binary.
- **Why:** grouping by launcher couples code to deployment (which changes); cohesion by concern is stabler.

## File Declaration Order

Order the top-level declarations in each `*.go` file so the important, public surface
is at the top and private internals are hidden at the bottom. A reader should see
everything important first; scanning the file should not require digging.

For a file built around one object:

1. Exported `const` and `var`, plus the `New<Object>` constructor(s). These come first
   because they are what you need to create and use the object — the first thing a
   reader looks for.
2. The object's struct definition.
3. The object's methods (prefer alphabetical order; not mandatory).
4. Unexported `const` and `var`.
5. Auxiliary/helper structs (unexported support types) — placed between the unexported
   vars/consts and the unexported methods.
6. Unexported methods/functions (prefer alphabetical order; not mandatory).

- **Multiple structs in one file:** keep the same layout but put the primary ("main")
  struct first. A combined layout is acceptable but very rare — two large objects in one
  file usually means the file should be split into two.
- **Files with no object** (free functions plus a config/data type): apply the same
  spirit — exported type(s) and function(s) on top, then unexported consts/vars, then
  auxiliary structs, then unexported helper functions.

Treat a file that violates this order as something to fix.

## Error Handling

The project's contract: separate user-facing errors from internal failures via a
dedicated `PublicError` wrapper type (typically `internal.PublicError`). Any error
message that is **safe to show** to a user is wrapped with `internal.NewPublicError(...)`
at the point where the error is created (typically in the service layer).

**Rule**: if a function can fail in a way that meaningfully communicates something to
the user, return a public error. For all other failures (DB down, unexpected nil,
upstream proxy unreachable, etc.) return a plain error — the controller will send a
generic fallback.

#### Creating a public error (service layer)

```go
import "<module>/internal"

// user should know about this
return internal.NewPublicError("Invalid input. <specific guidance>")

// internal failure — user gets generic message
return fmt.Errorf("db query failed: %w", err)
```

### Testing the error path

Every controller error-branch test must assert a response was actually sent, equal to `PublicError.Details()` when the error is a `PublicError`, else equal to the fallback constant.

## Constraints

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
- **Forbidden imports**: enforced by `make lint`; nothing is currently banned. Add a module here (and to the lint check) only when the team rejects one.
- **Testing**: use `github.com/stretchr/testify`; run with `-race`; prefer parallel subtests where there's no shared mutable state. `make test`'s `-race` step deliberately does NOT pin `CGO_ENABLED` — Go 1.26's race detector links libtsan via cgo and refuses under `CGO_ENABLED=0`, so the runner default is let through (Linux picks cgo, darwin uses its built-in detector).
- **One `Test*` per method, scenarios as subtests**: each tested method/function gets
  exactly one top-level test function named after it (e.g. `TestEncode` for `Encode`),
  and every scenario for that method lives as a `t.Run("descriptive name", ...)`
  subtest inside it. Do **not** create separate top-level tests like
  `TestEncode_EmptyInput`, `TestEncode_Unicode`, `TestEncode_Error` — these belong
  as subtests of a single `TestEncode`. Methods on a type follow the same rule with
  the standard `TestType_Method` form (e.g. `TestUser_Validate`).
- **No CGO in production**: `go build`/`go vet` always pin `CGO_ENABLED=0` (static binary, no libc link); we never force `CGO_ENABLED=1` for production — the `-race` exception is in the Testing entry above.
- **Compile-time interface checks**: Every mock/stub struct in test files must have a
  `var _ interfaceName = &mockStruct{}` assertion at the top of the file.
- **No section-divider comments**: Do not use `// --- section ---` or `// ----` style
  separator comments. Let the code structure speak for itself.
- **No skipped errors**: Never use `_` to discard error return values in production or
  test code. Always capture the error and assert/check it. The only exceptions are
  `fmt.Fprint*` writes to loggers, `Rollback()` calls in error-recovery paths, and
  resource `.Close()` in `t.Cleanup` / `defer`.
- **Comments**: lowercase first word (e.g. `// wrap the driver error so callers can match on it`).
- **Godoc**: every exported identifier gets a doc comment starting with its name and ending with a period; skip it if it would only restate the signature. One `// Package <name>` per package (`cmd/*` uses `// Command <name>`). Document the non-obvious: concurrency guarantees, which methods return `PublicError` vs plain errors, lifecycle contracts ("caller must Close"), and error-sentinel conditions. Never overwrite a substantive WHY-comment with a generic restatement; don't bulk-comment private helpers.
- **Build outputs live in `./build/`, scratch in `./tmp/`, logs in `./logs/`**:
  Never run `go build` without `-o ./build/<name>` — bare `go build ./cmd/<binary>`
  drops a binary in the project root, which is **not** in `.gitignore` and
  would be picked up by `git add .`. The same applies to any throwaway artifacts,
  fixtures, or intermediate files: use `./tmp/` rather than the repo root. Runtime /
  cyclic logs go to `./logs/`. Only these three directories are gitignored at the root.
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

## Planning Workflow

All non-trivial work is tracked as a Markdown plan file before implementation begins.

### Directory layout

```
plans/
├── NNN-task-slug.md     # active / in-progress plans (e.g. 001-fix-auth.md)
├── completed/           # plans for fully shipped tasks (e.g. 260422.0001.fix-auth.md)
└── history/             # archived / cancelled plans
```

### File naming

- **Active plans (`plans/`)** — zero-padded sequential index + kebab-case slug:
  `NNN-description.md` (e.g. `001-fix-unauthorized-middleware.md`, `002-add-rate-limiting.md`).
  Pick the next number by checking the highest existing prefix across `plans/`, `plans/completed/`,
  and `plans/history/`.

- **Completed plans (`plans/completed/`)** — date prefix + zero-padded daily index (4 digits) + slug:
  `YYMMDD.NNNN.description.md` (e.g. `260422.0001.fix-unauthorized-middleware.md`).
  `NNNN` resets to `0001` each day and increments for each additional completion on that day.

- **Archived plans (`plans/history/`)** — keep the original `NNN-` filename from `plans/`.

### Lifecycle

1. **Create** — before touching code, produce a plan file in `plans/` using the `NNN-slug.md`
   naming convention described above.
2. **Implement** — work through the tasks defined in the plan. The plan file stays in
   `plans/` while work is in progress.
3. **Complete** — once every acceptance criterion is met and the test suite passes, move
   the file to `plans/completed/` using the date-based naming above.
4. **Archive** — if a plan is abandoned or superseded without being fully implemented or
   if we need to save intermediate data or task execution logs, move it to `plans/history/` instead.

### Plan file format

One line: Overview; Assumptions; Tasks (Description / Acceptance Criteria / Pitfalls / Complexity); Execution Order; Risks; Trade-offs.

### Rules

- **One plan per concern.** Don't bundle unrelated changes in a single plan file.
- **Plan before code.** Claude must create (or confirm an existing) plan file before
  writing or modifying any source files.
- **Keep plans honest.** If implementation diverges from the plan, update the plan file
  before moving it to `completed/`.
- **Slug matches intent.** The description part of the filename should be readable at a glance:
  `002-add-rate-limiting.md`, `003-migrate-sqlite-to-postgres.md`, not `004-task.md`.

## Agent Pipeline

Every non-trivial task runs a three-stage pipeline; `gocode-testdoctor` is invoked on-demand whenever tests fail at any stage.

1. **gocode-architect** — creates the plan file at `plans/NNN-slug.md` (see Planning Workflow) before any code is written; update an existing plan rather than adding one.
2. **gocode-engineer** — implements the plan's tasks plus tests for new code.
3. **gocode-reviewer x3, in parallel (one message, three tool calls)** — each prompt self-contained (lens name, focus, what to SKIP, file list, deliverable `file:line` + patch sketch + word cap, priorities P0-P3):
   - **A correctness & tests** — bugs, races, edge/error paths, context propagation, resource cleanup, coverage + test structure (one `Test*` per method with subtests).
   - **B security & operations** — input validation, auth boundaries, secrets handling, injection, observability, log volume, operator/runbook UX.
   - **C performance & architecture** — allocations, blocking I/O, goroutine/resource leaks, layer boundaries, dependency direction, API-contract/exported-surface stability.

The orchestrator (main session) merges the three reports, dedupes, and resolves conflicting verdicts explicitly (it chooses, names the rejected suggestion, explains — user has final say). Tests must be green before review; a red tree goes to `gocode-testdoctor` first (minimal fix, no redesign). The full three-lens fan-out is mandatory on the FIRST review; after a P0/P1 fix the orchestrator runs ONE pass scoped to the changed lines. The plan moves to `plans/completed/` only once every P0/P1 is fixed or explicitly accepted with rationale.
