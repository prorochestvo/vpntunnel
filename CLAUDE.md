# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

It is deliberately a map, not a manual. It holds what applies to every task plus the rules
whose violation is silent; the depth lives in the project skills listed below and is loaded
on demand.

## Session budget — the tripwire that fails silently and off-box

**The Mullvad account runs at exactly 5 of 5 concurrent WireGuard sessions. This host must
never present more than 2 at any instant** — 1 persistent streaming tunnel + at most 1
on-demand tunnel. The operator's 3 personal devices (phone + 2 laptops) hold the other three.

Exceeding 2 evicts a session on **another machine**: provider-side, silent, no error raised
here, landing outside this repository. No test can catch it.

Every tunnel lifecycle change must therefore be **break-before-make + settle**, never
make-before-break, on both roles. On-demand's `idle_ttl` defaults to 168h, so treat the
on-demand device as permanently live when reasoning about a streaming-side change
(2 streaming + 1 on-demand = 3 = a breach). This is a provider-imposed budget, not a tuning
choice. Derivation: agent memory `tunnel-session-budget`.

## Project skills

Invoke by name (Skill tool) when the work touches their area. Each is the full canon for its
subject — this file keeps only the tripwire.

| Skill | Load before touching |
|---|---|
| `vpntunnel-http-api` | `internal/gateway/**`, `internal/application/proxy.go`, any `/v1/...` route, `X-Vpntunnel-Token`, `X-Request-Id`, the `/v1/admin/health` body |
| `vpntunnel-config` | `internal/infrastructure/config`, `internal/policy`, `internal/application/tunnelpool`, `wgconf`, any `proxy.json` key, the `-tls-*` flags, tunnel discovery |
| `vpntunnel-deployment` | `Makefile` deploy targets, `configs/*` host files, `.github/workflows/*`, `/opt/vpntunnel/` layout, `.env` vars, `go.mod` pins, `./configs/tunnels/` |

Generic Go conventions (style, declaration order, test structure, godoc, error discipline,
build hygiene, organisation) come from the `stack-go` plugin skills and are not restated
anywhere in this repo.

### Where new canon goes

This file is loaded whole into every session, so its size taxes every conversation whatever
the task touches. Route new documentation by *when the reader needs it*:

- **CLAUDE.md** — what applies to every task (binary map, layer table, key patterns, env vars,
  error handling, the working agreement), plus rules whose violation is **silent**. A tripwire
  keeps its place here even after its subject has moved out.
- **A project skill** (`.claude/skills/<name>/SKILL.md`) — the depth for one subject area. The
  `description` frontmatter *is* the load trigger: name the packages, paths, symbols and env
  vars that pull it in. A description that summarises the prose never loads, and the knowledge
  is lost with it.
- **Neither** — incident narratives, enumerations derivable from the code, and the reasoning
  behind a decision already taken: commit bodies, `plans/` and `docs/`.

**Measure, never estimate.** This file is mostly contracts and identifiers, which do not
compress. `wc -c` before and after, and prove a move lost nothing by extracting every
backticked span from the old text and confirming each survives somewhere.

## Project state

`vpntunnel` is a lightweight forward HTTP proxy routing all egress through a userspace
WireGuard tunnel (no root required). v6 ships one daemon to the production host as a systemd
service. One environment, one host. Server-side state lives at `/opt/vpntunnel/`, hand-authored
once during setup. Deploy detail: skill `vpntunnel-deployment`.

Operator flow: download a wg-quick `.conf` from mullvad.net or any WireGuard provider, drop it
into `./configs/tunnels/` — **auto-discovered on the next startup; no change to
`configs/proxy.json` is needed** — then run `./build/vpntunnel -config configs/proxy.json`
(plain HTTP locally, no TLS flags for dev). Production passes `-tls-cert-dir` on the systemd
`ExecStart` line. `make generate-vpn-config` writes a `.conf` interactively (Mullvad zip ingest).

### Binaries

- `cmd/vpntunnel/` — the long-running daemon; composition root wired inline in `main.go`.
- `cmd/generatevpnconfig/` — one-shot operator CLI that writes `.conf` files into
  `configs/tunnels/`. Run via `make generate-vpn-config`, not directly.

### Layer table

| Layer | Package(s) | Responsibility |
|-------|-----------|----------------|
| Domain | `internal/domain` | Pure value types (`RequestSummary`), `HealthReporter`, country and tunnel-event types. No I/O. |
| Policy | `internal/policy` | Every `Default*` constant, shared limits, the IP deny list. |
| Application | `internal/application` | Orchestration free of transport: `ProxyService` (`HandleHTTP`, `HandleCONNECT`, `WaitTunnels`), `tunnelpool` (discovery, `VerifySingleKey`, streaming supervisor, on-demand scheduler), `asyncjob`. |
| Gateway | `internal/gateway` | Receiving and rendering: `httpserver` (proxy listener, thin `*http.Server` wrapper, graceful shutdown), `router` + `router/apitls` (API listener; plain HTTP when `-tls-cert-dir` is empty, TLS 1.3 when set), `middleware` (request-id, role auth), `httpV1/{routes,handlers,dto}`. |
| Infrastructure | `internal/infrastructure` | External edges: `config` (JSON load/validate), `wireguard` (`Dialer`, `DialerCloser`; userspace WireGuard via wireguard-go + gVisor netstack) and its `wgconf` wg-quick `[Interface]`/`[Peer]` parser producing `ParsedConfig`, `observability` (slog stdout + lumberjack rotating JSONL access log), `notify` (`Notifier`, `Nop`, `TelegramNotifier`), `ipdeny`. |
| Tools | `internal/tools` | Cross-cutting: `bearerauth` (`Verifier`, `BearerVerifier`, constant-time token check), `hmackey`, `rotation`. |

> **Renamed in the DDD restructure**; `plans/` and agent memory use the old names.
> `internal/config`→`internal/infrastructure/config`, `internal/observability`→
> `internal/infrastructure/observability`, `internal/notify`→`internal/infrastructure/notify`,
> `internal/tunnel`+`internal/tunnel/wireguard`→`internal/infrastructure/wireguard`,
> `internal/tunnel/wireguard/wgconf`→`internal/infrastructure/wireguard/wgconf`,
> `internal/auth`→`internal/tools/bearerauth`, `internal/service`→`internal/application`,
> `internal/asyncjob`→`internal/application/asyncjob`, `lazy`→`tunnelpool` (so
> `lazy.DefaultHandshakeMaxAge = 180s` is now `tunnelpool.DefaultHandshakeMaxAge`),
> `internal/transport/httpserver`→`internal/gateway/httpserver`,
> `internal/transport/apiserver`→`internal/gateway/router`,
> `internal/transport/apiserver/handlers`→`internal/gateway/httpV1/handlers`.
> `internal/publicerror` is gone — see Conventions.

### HTTP surface

The proxy listener (default `127.0.0.1:7788`) dispatches `CONNECT` to `HandleCONNECT` and
everything else to `HandleHTTP`. The API listener (default `127.0.0.1:8888`) serves four frozen
`/v1` routes behind an `X-Vpntunnel-Token` admin/proxy role split. Two silent breakages:

- **Loopback clients are not authenticated at all** — the token check is skipped for
  them, which is the whole reason the listener binds loopback. Binding anything else
  exposes the proxy unauthenticated. For every other client the challenge must be written
  before the hijack: a `407 Proxy Authentication Required` written after it has no status
  line left to write.
- **The `/v1/admin/health` body is fixed**: `{status, tunnels: [...]}`, each entry exactly
  `{id, healthy, handshake_age_seconds}`. Never add `peer_endpoint`, key material, the peer
  public key, or `TunnelHealth.Err` text.

Route table, roles, `?force=true` rotation and the `/v1/tunnels` catalog: skill
`vpntunnel-http-api`. No `/ping` / `/health/check` pair exists — a known gap.

## Commands

```bash
make build       # format, then CGO_ENABLED=0 go build -o ./build/vpntunnel ./cmd/vpntunnel/
make run         # build, source ./.env, run against ./configs/proxy.json
                 # NOTE: requires at least one .conf in ./configs/tunnels/
make test        # THE GATE: lint, then gofmt check + go vet + go test -race ./...
make lint        # CGO_ENABLED=0 go vet ./... + forbidden-imports check
make format      # go fmt ./...   (there is no `make fmt` target)
make clean       # rm -rf ./build ./tmp/*.tmp
make generate-vpn-config  # interactive .conf generator; ARGS="-force" overwrites
make init        # provision/update the host, then deploy-nginx — sudo operator only
make deploy-nginx  # install the edge vhost and reload nginx
make healthz     # over SSH: assert Mullvad egress + health plane on prod
make examination # PASS/FAIL every API route against a local `make run` daemon
```

`make build` and `make run` run `go fmt ./...` first — a build is never read-only on this
tree. `make test` depends on `lint`, so the gate vets before compiling tests.

Run one-off tests with the standard `go test -race -run 'TestName/subtest' ./<pkg>/` forms.
Pin `CGO_ENABLED=0` for `go build`/`go vet` and always pass `-o ./build/<name>`; leave
`CGO_ENABLED` unset for `-race` (see Conventions).

## Conventions

Error handling follows the standard `PublicError` contract from
`github.com/prorochestvo/loginjector`: construct with `loginjector.NewPublicErrorDetails(...)`,
match with `errors.As(err, &loginjector.PublicDetailsError)`. Every controller error-branch
test asserts the response text — the public message when public, the generic fallback
otherwise. Project-specific constraints:

- **Never log WireGuard key material**: the private key and pre-shared key (PSK) must NEVER
  appear in any log call, error message, or string format. The private key lives only in the
  `.conf` file and in `wireguard.Options.PrivateKey` at runtime. Startup logs may include
  `peer_endpoint`, `local_address` and the `.conf` basename (`source`) only.
- **Never log the Bearer token**: the proxy auth token (`auth.token` / `auth.token_file`) must
  NEVER appear in any log call, error message, or string format. Startup logs may include
  `token_len` and `source` (inline or file basename) only. Auth failure logs record only a
  `reason` enum — never the attempted token value.
- **Never log the tunnel-id HMAC key**: the raw bytes in `tunnel_id_hmac_key_file` must NEVER
  appear anywhere — not raw, not hex. Startup logs may include the key file basename and
  `key_len` only. The derived hex ids (`HMAC-SHA256(key, basename)`) ARE non-secret.
- **Never log the Telegram bot token, DSN, or admin chat id**: `VPNTUNNEL_TELEGRAMBOT_DSN` and
  everything parsed from it are secret. Startup/status logs may include only `token_len` and
  whether the notifier is enabled.
- **`./configs/tunnels/` and `./configs/auth/` are gitignored — keep them that way.** Treat
  each `.conf` like an SSH private key (0600, never committed).
- **Race + CGO**: `make test`'s `-race` step deliberately does NOT pin `CGO_ENABLED` — Go
  1.26's race detector links libtsan via cgo and refuses under `CGO_ENABLED=0` (Linux picks
  cgo, darwin uses its built-in detector). Production builds always pin `CGO_ENABLED=0`
  (static binary, no libc link).
- **Forbidden imports**: enforced by `make lint`; nothing is currently banned. Add a module
  here (and to the lint check) only when the team rejects one.
- **`gvisor.dev/gvisor` is pinned** at `v0.0.0-20250503011706-39ed1f5ac29c`; newer revisions
  fail to build. Why, and its size cost: skill `vpntunnel-deployment`.
- **Never log deploy private-key contents** (`SSH_PRIVATEKEY`, scoped to the `PRIME` GH
  environment) in workflow output.

## Working agreement

Plan-first pipeline; the canonical procedure is the `pipeline:working-agreement` skill — load
it before starting non-trivial work. Project delta:

- **Gate:** `make test` (verified against the `Makefile`: it chains `lint` first, then gofmt +
  `go vet` + `go test -race ./...`)
- **Lenses:** standard set — see `pipeline:working-agreement`, which includes lens O.
- **Branching:** standard (`type/<issue>-<slug>`, PR into `main`). Production deploys on each
  `v*` tag after a manual approval gate; `main` and PRs run lint + tests only.
