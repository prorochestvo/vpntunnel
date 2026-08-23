---
name: vpntunnel-config
description: The vpntunnel v6 `proxy.json` schema and wg-quick tunnel discovery — the `vpnstream` / `api` / `api.vpn` / `access_log` / `operational` block split, the streaming-only `allowed_countries` scope, the single-key guard, the HMAC tunnel-id key file, the on-demand timers, and the CLI-flag-only TLS settings. Load before touching `internal/infrastructure/config/config.go`, `internal/policy` (`defaults.go`, `constants.go`), `internal/infrastructure/wireguard/wgconf`, `internal/application/tunnelpool`, or `configs/proxy*.json`; before adding, renaming, defaulting or validating any `vpnstream.*` or `api.*` key, `tunnel_id_hmac_key_file`, or any `Default*` constant; and before changing `VerifySingleKey`, `ParsedConfig`, `tunnelpool.DefaultHandshakeMaxAge`, the `raw*` decoder structs, tunnel discovery under `<configDir>/tunnels/`, or the `-tls-cert-dir` / `-tls-hostname` / `-tls-ip-sans` flags.
---

# vpntunnel configuration (v6)

Proxy-egress config lives under `vpnstream`; on-demand VPN proxy config lives under
`api.vpn`. TLS settings are CLI flags, **not** part of `proxy.json`. Tunnels are never
listed in config — they are auto-discovered from `<configDir>/tunnels/`.

## Where the schema actually is

**The full config shape (every key) is the `raw*` struct set in
`internal/infrastructure/config/config.go`**, mirrored by the committed
`configs/proxy.json`. **The value applied when a key is absent is the matching `Default*`
constant in `internal/policy/defaults.go`.** Read those two files rather than any prose list —
this skill documents only the semantics that are non-obvious or that fail silently. Env var
name constants sit in `internal/policy/constants.go`.

Key inventory (v6): top level — `vpnstream`, `access_log`, `operational`, `api`,
`tunnel_id_hmac_key_file`. Under `vpnstream` — `listen`, `allowed_countries`, `auth.token` /
`auth.token_file`, `reconnect_min` / `reconnect_max`, `dial_timeout`, `idle_timeout`,
`shutdown_timeout`. Under `access_log` — `path`, `max_size_mb`, `max_age_days`, `max_backups`,
`compress`; under `operational` — `level`, `format`. Under `api` — `listen`,
`shutdown_timeout`, `auth.admin_token_file` / `auth.proxy_token_file`,
`max_request_body_bytes`, `log.path_sanitize_patterns`, `vpn`; under `api.vpn` — `timeout`,
`max_timeout`, `demand.grace` / `demand.settle_delay` / `demand.idle_ttl`,
`async.storage_path`.

`api` block is **required**. All other top-level blocks are optional — missing means defaults
apply. `api.vpn` absent applies all vpn defaults; `vpnstream` absent applies all vpnstream
defaults.

## `tunnel_id_hmac_key_file`

Optional top-level field, default `./auth/tunnel-id.key`, **resolved relative to the config
directory** by the binary (not the cwd). Points to a 0600 file holding 64 random bytes.
Generated on first run if absent; **never rewritten back into `proxy.json`**.

The KEY MATERIAL (file contents) must never be logged — only the basename and `key_len` may
appear in startup logs. The derived tunnel id is `HMAC-SHA256(key, conf_basename)`,
hex-encoded to 64 lowercase chars, and is **non-secret**: it appears in `/v1/tunnels`
responses and in the `{id}` path segment.

Consequence worth stating out loud: **regenerating or losing this key silently renames every
tunnel id.** Nothing errors — clients simply 404 against ids that no longer exist.

## `vpnstream.allowed_countries` — streaming only

Optional list of two-letter lowercase country codes. **Scope: streaming only.**

- Empty list, or field absent, means all discovered configs are eligible for the always-on
  streaming random-pick.
- **A code that matches no config is silently dropped.** The daemon errors at startup only
  when the resulting country-filtered set is empty (streaming is mandatory, so its empty set
  panics). A typo in one of several codes therefore narrows the pool with no diagnostic.
- **The on-demand scheduler (`/v1/tunnels/{id}/proxy/...`) accepts any discovered tunnel
  regardless of `allowed_countries`** — it uses the full unfiltered set. So does
  `/v1/tunnels`. This asymmetry is deliberate; do not "fix" it by filtering both.

The single-key guard `VerifySingleKey` (`internal/application/tunnelpool/verify.go`, called
from `cmd/vpntunnel/main.go`) runs over the **full discovered set**, not the filtered one:
all `.conf` files must share one WireGuard key. That guard is what the whole session-budget
scheme rests on — one shared `[Interface] PrivateKey` means one provider device across ~700
server configs. If keys ever differ per config, each config burns its own provider session
and the design is non-viable.

## Tunnel discovery

Scans `<configDir>/tunnels/` (`discover.go`) for **top-level** `*.conf` files.

- Only regular files (**no symlinks**) with a `.conf` suffix are included.
- Dotfiles (including `.gitkeep`) and subdirectories are ignored.
- A **missing** tunnels directory is a startup error. An **empty** directory is also a
  startup error.
- **Unparseable `.conf` files are warn-skipped at device-build time** (`build.go`) — they do not fail
  startup. A malformed config therefore shrinks the pool with only a log line to show for it.

WireGuard parameters are the standard wg-quick `[Interface]` / `[Peer]` fields, read from the
`.conf` by the `wgconf` parser (`internal/infrastructure/wireguard/wgconf`) into
`ParsedConfig`.

## Timers

`vpnstream.reconnect_min` / `vpnstream.reconnect_max` — initial and maximum exponential
backoff between reconnect attempts for the always-on streaming device (`supervisor.go`).

`api.vpn.demand.grace` — after the last active job on the current zone finishes, how long the
scheduler stays on that zone before switching to the next zone with a backlog. Each new
same-zone request resets the window. Default 10s.

`api.vpn.demand.settle_delay` — **mandatory** pause between tearing the current on-demand
device down and bringing the next zone's device up, so the provider frees the old session
before the new one starts. Without it a zone switch transiently presents an extra session and
exceeds the connection budget. Default 15s, **minimum 5s**.

`api.vpn.demand.idle_ttl` — how long the on-demand device is kept live with no active job
before teardown; a request for a **different** zone tears it down immediately regardless.
Default 168h — which means the on-demand device is effectively always live, and every
streaming-side change must assume it is.

`handshake_max_age` is **no longer configurable**. It is `tunnelpool.DefaultHandshakeMaxAge
= 180s` — roughly 3x the 25s persistent keepalive plus a safety margin, so quiet tunnels do
not flap. (Older plans and agent memory call this `lazy.DefaultHandshakeMaxAge`; the `lazy`
package was renamed `tunnelpool`.)

## Auth and async

`vpnstream.auth` — optional proxy bearer token. Pick **one** of `token` or `token_file`;
setting both is a config error. Missing means proxy auth is disabled entirely.

`api.vpn.async.storage_path` — path for async job state, default
`/opt/vpntunnel/state/async.db`. The async TTL and concurrency knobs are **built-in constants
in `internal/application/asyncjob`**, not configurable. (Pre-restructure paths say
`internal/asyncjob`.)

## TLS is CLI flags, not config

| Flag | Default | Description |
|------|---------|-------------|
| `-tls-cert-dir` | `""` (HTTP mode) | Directory holding the API TLS cert/key. Empty means plain HTTP — loopback/dev only. Production must pass `/opt/vpntunnel/tls/`. A relative value resolves against the cwd, not the config dir. |
| `-tls-hostname` | `localhost` | TLS server name embedded in the API certificate. |
| `-tls-ip-sans` | `` (empty) | Comma-separated list of IP Subject Alternative Names. |

Production sets these on the systemd `ExecStart` line from `/opt/vpntunnel/.env`; see the
`vpntunnel-deployment` skill.
