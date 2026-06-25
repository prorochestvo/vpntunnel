# Async Proxy Endpoint — Operator Guide

## Overview

The async-with-tag mode adds retry-safe, decoupled-timing semantics to the
existing `{METHOD} /v1/proxy/{scheme}/{rest...}` endpoint. Sending the
`Proxy-Retry-Tag` request header opts a single request into this mode; the
daemon accepts ownership, returns `202 Accepted` immediately, and processes the
upstream call asynchronously. A subsequent request carrying the same tag either
returns the upstream response verbatim (once the job is terminal) or another
`202` while it is still in flight. The sync path — any request without
`Proxy-Retry-Tag` — is byte-identical to v1 behaviour and is unaffected.

## Contents

- [The `Proxy-Retry-Tag` contract](#the-proxy-retry-tag-contract)
- [Status matrix](#status-matrix)
- [The `Proxy-Async-Status: pending` discriminator pattern](#the-proxy-async-status-pending-discriminator-pattern)
- [The `api.async` config knobs](#the-apiasync-config-knobs)
- [The `api.log.path_sanitize_patterns` schema](#the-apilogpath_sanitize_patterns-schema)
- [Operational signals](#operational-signals)
- [Storage and lifecycle](#storage-and-lifecycle)
- [Health endpoint](#health-endpoint)

## The `Proxy-Retry-Tag` contract

**Header name:** `Proxy-Retry-Tag`

**Tag format:** `^[a-zA-Z0-9_-]{8,128}$` — alphanumeric, hyphen, and underscore,
8–128 characters. UUID v4 and v7 string forms satisfy this comfortably (e.g.
`019685b3-6e2a-7f4a-8b3d-1234abcd5678`). Tags outside this charset/length range
are rejected with `400 tag_invalid`.

**Who generates the tag:** the client always. The server never invents or
transforms it.

**Idempotency window:** `complete_ttl + tombstone_ttl`, approximately 25 hours
with defaults (1h + 24h). Within `complete_ttl` the full upstream response is
available on retry. Within `tombstone_ttl` after that, the tag returns
`410 Gone`. After both windows expire the tag is purged and resubmitting it
starts a fresh job.

> **WARNING: client MUST poll for the result — within the idempotency window.**
>
> A submitted request that the client never polls will be lost on the next
> daemon restart (pending records are deleted at startup). Additionally, a
> client that polls too late — after `complete_ttl + tombstone_ttl` (~25h
> with defaults) — receives `410 Gone` instead of the upstream response.
> Fire-and-forget and slow-polling clients both risk silent loss.

**Security note:** tag values are never written to operational logs by the
daemon itself (only counts are logged). Tags are client-controlled opaque
strings; treat them with the same care as session tokens. Avoid logging them
in external tooling (alerting rules, sidecars, WAF plugins) at levels above
DEBUG.

## Status matrix

| Response | When | Discriminator |
|---|---|---|
| `202` + `Proxy-Async-Status: pending` | First submit OR retry while the job is still in flight | Header **present** |
| Upstream response verbatim (any status) | Tag in a terminal state, within `complete_ttl` | Header **absent** |
| `410 Gone` (`tag_evicted`) | Tag was tombstoned; result payload dropped, past `complete_ttl` | Body envelope `code` |
| `503 Service Unavailable` (`queue_full`) | Worker pool semaphore saturated. Action: increase `api.async.max_concurrent_jobs` or investigate upstream slowness (check `pending_jobs_count` on `/v1/admin/health`). | Body envelope `code` |
| `503 Service Unavailable` (`async_disabled`) | `api.async` block missing from config — async wiring not initialised. Action: add the block and restart. | Body envelope `code` |
| `503 Service Unavailable` (`shutting_down`) | Graceful daemon shutdown in progress. Action: wait and retry; no config change needed. | Body envelope `code` |
| `500 Internal Server Error` (`internal`) | Store I/O failure (bbolt error); check operational log | Body envelope `code` |
| `405 Method Not Allowed` (`method_not_allowed`) | `CONNECT` request with `Proxy-Retry-Tag` | Body envelope `code` |
| `413 Request Entity Too Large` (`body_too_large`) | Request body exceeds 10 MB | Body envelope `code` |
| `400 Bad Request` (`tag_invalid`) | Tag does not match `^[a-zA-Z0-9_-]{8,128}$` | Body envelope `code` |
| `400 Bad Request` (`invalid_scheme`) | Scheme not `http` or `https` | Body envelope `code` |
| `400 Bad Request` (`tunnel_required`) | `X-Tunnel-Id` header missing | Body envelope `code` |

All error responses use the standard JSON envelope: `{"code": "...", "message": "..."}`.
The `X-Request-Id` header (UUIDv7) is present on every response.

## The `Proxy-Async-Status: pending` discriminator pattern

A completed upstream response can itself carry status `202 Accepted` (for
example, an upstream that accepted a task asynchronously). Without a
discriminator, the client cannot tell whether a `202` it receives means "daemon
is still working" or "upstream returned 202".

**Rule:** always check the `Proxy-Async-Status: pending` header before
interpreting the status code on a `202`.

### Worked example

**Step 1 — first submit**

```
→  POST /v1/proxy/https/api.example.com/tasks
   Proxy-Retry-Tag: 019685b3-6e2a-7f4a-8b3d-1234abcd5678
   X-Vpntunnel-Token: <proxy-token>

←  HTTP/1.1 202 Accepted
   Proxy-Async-Status: pending
   Content-Type: application/json

   {"status": "pending", "retry_tag": "019685b3-6e2a-7f4a-8b3d-1234abcd5678"}
```

Client sees `Proxy-Async-Status: pending` → daemon still owns the request.
Wait and retry later.

**Step 2 — retry while still in flight**

```
→  POST /v1/proxy/https/api.example.com/tasks
   Proxy-Retry-Tag: 019685b3-6e2a-7f4a-8b3d-1234abcd5678
   X-Vpntunnel-Token: <proxy-token>

←  HTTP/1.1 202 Accepted
   Proxy-Async-Status: pending
   ...
```

Same response. Still in flight.

**Step 3 — retry after upstream completes (upstream itself returned 202)**

```
→  POST /v1/proxy/https/api.example.com/tasks
   Proxy-Retry-Tag: 019685b3-6e2a-7f4a-8b3d-1234abcd5678
   X-Vpntunnel-Token: <proxy-token>

←  HTTP/1.1 202 Accepted
   Content-Type: application/json
   X-Request-Id: 019685b3-9f3c-7a11-bcd2-fedcba098765

   {"task_id": "xyz", "queued": true}
```

`Proxy-Async-Status` is **absent** → this is the upstream's answer.
The `202` here is the upstream's `202`, not the daemon's "pending" signal.

## The `api.async` config knobs

All fields live under the `api.async` JSON object.

| Field | Default | Unit | Operator guidance |
|---|---|---|---|
| `storage_path` | `/opt/vpntunnel/state/async.db` | File path | Path to the bbolt database file. The daemon creates the parent directory (mode 0700) on startup if it is missing; it must be writable by the daemon process. Use an absolute path. |
| `max_concurrent_jobs` | `100` | Integer (semaphore slots) | Maximum number of jobs executing concurrently. Increase if `queue_full` responses appear under expected load; decrease under observed disk or memory pressure. |
| `pending_timeout` | `5m` | Duration string (`5m`, `30s`) | Jobs still pending after this duration are transitioned to `failed_timeout`. Increase if upstream calls legitimately take longer. Decrease to fail fast and reclaim the slot. Increase if jobs that legitimately run long are timing out (visible as rising `timed_out=N` in `async_gc_pass` log lines). |
| `complete_ttl` | `1h` | Duration string | Terminal jobs (completed, failed, failed_timeout) older than this are tombstoned and their response payload dropped. Increase to give clients a longer retry window; decrease to reduce disk footprint. Decrease if `tombstone_jobs_count` on `/v1/admin/health` is large and disk is constrained. |
| `tombstone_ttl` | `24h` | Duration string | Tombstones older than this are deleted permanently. Total idempotency window = `complete_ttl + tombstone_ttl`. After this window the tag is reusable. Reduce if `tombstone_jobs_count` grows unboundedly and idempotency beyond 1–2h is not needed. |

**v1 → v2 migration:** if the `api.async` block is absent from `proxy.json`,
the async subsystem is not initialised. All sync requests (`/v1/proxy/`
without `Proxy-Retry-Tag`) work exactly as in v1 — no behaviour change. Any
request that does carry `Proxy-Retry-Tag` receives `503 async_disabled`.
Add the `api.async` block only when you intend to enable async mode.

For the full surrounding `api` config block (TLS, listen, auth), see the Config schema v5 section in `CLAUDE.md`.

### Example config snippet

```json
{
  "api": {
    "async": {
      "storage_path": "/opt/vpntunnel/state/async.db",
      "max_concurrent_jobs": 100,
      "pending_timeout": "5m",
      "complete_ttl": "1h",
      "tombstone_ttl": "24h"
    }
  }
}
```

## The `api.log.path_sanitize_patterns` schema

The sanitiser applies at log-write time to the single
`/v1/proxy/{scheme}/{rest...}` access log call site. Both sync requests (no
`Proxy-Retry-Tag`) and async-with-tag requests flow through the same path, so
one config block covers both. Routing inside the daemon is never affected.

**Config shape:** array of objects, each with a `pattern` (Go RE2 regex string)
and a `replacement` (Go replacement string, supporting `$1`, `$2`, `${name}`
capture-group references).

```json
"path_sanitize_patterns": []
```

Zero patterns means zero behavioural change (default).

**Precompile + fail-fast:** patterns are compiled once at config load via
`regexp.Compile`. A malformed pattern causes the daemon to refuse to start; the
error message names the offending array index. A config deploy with a bad regex
is caught before traffic hits the daemon.

**Important:** every pattern below is a working Go (RE2) regex. Go does not
support lookaheads or backreferences. Test new patterns at
[regex101.com](https://regex101.com) with "Go" flavor before deploying.

### Copy-paste examples

```json
"path_sanitize_patterns": [
  {
    "pattern":     "(/bot)[0-9]+:[A-Za-z0-9_-]+(/)",
    "replacement": "$1<REDACTED>$2"
  },
  {
    "pattern":     "(api_key=)[A-Za-z0-9_-]+",
    "replacement": "$1<REDACTED>"
  },
  {
    "pattern":     "(/services/T[A-Z0-9]+/B[A-Z0-9]+/)[A-Za-z0-9]{24}",
    "replacement": "$1<REDACTED>"
  }
]
```

**Pattern 1 — Telegram bot tokens**
Matches paths like `/bot123456789:ABCdefGHIjklMNO_pqrSTUvwxYZ/...`.
The `$1` preserves `/bot` and `$2` preserves the trailing `/` so the rest of the
path remains readable in the log.

Input:  `/bot123456789:ABCdefGHIjklMNO_pqrSTUvwxYZ/sendMessage`
Output: `/bot<REDACTED>/sendMessage`

**Pattern 2 — Generic query-style API keys in path segments**
Matches `api_key=<token>` embedded in URL paths (common in REST APIs that embed
credentials in the path rather than a header). Captures the key name so the log
still shows which credential was present.

Input:  `/v2/data?api_key=ghp_xK3mN8pQrTuvWxYz012345abcdefgh`
Output: `/v2/data?api_key=<REDACTED>`

**Pattern 3 — Slack incoming webhook tokens**
Slack webhook URLs follow the shape `/services/TXXXXXXXX/BXXXXXXXX/<24-char>`.
The token is the 24-character alphanumeric segment at the end. Preserves the
`/services/T.../B.../` prefix so incidents can still be correlated to a
workspace and channel.

Input:  `/services/T01ABCDEF/B02GHIJKL/xYzABCDEFGHIJKLMNOPQRSTU`
Output: `/services/T01ABCDEF/B02GHIJKL/<REDACTED>`

## Operational signals

These log lines are the primary observability surface for the async subsystem.

### Boot-time

**`operational log scrubbing active`** (INFO)

Emitted once at startup when the scrub handler is active (i.e. at least one
`path_sanitize_patterns` entry is configured, or the host:port scrub handler
is wired). Confirms the safety net is in place.

**`access log sanitiser configured patterns=N`** (INFO)

Emitted once at startup. `N` is the number of compiled sanitiser patterns.
Action: confirm this appears at every boot. If `N` is 0 unexpectedly, your
`api.log.path_sanitize_patterns` config block is missing or empty.

**`restart_pending_dropped count=N`** (INFO)

Emitted once at startup after the recovery scan. `N` is the number of
`pending` records deleted from the database. A value of 0 means there were
no in-flight jobs at the time of the previous shutdown (clean stop) or restart.
Action: if `N` spikes unexpectedly (e.g. dozens or hundreds on a daemon that
normally stops cleanly), investigate whether the daemon is being killed under
load — OOM kill, systemd `TimeoutStopSec` expiry, or a manual `SIGKILL`. Those
clients lost their in-flight work and should be prompted to resubmit.

### Steady-state

**`async_gc_pass timed_out=N tombstoned=N deleted=N`** (INFO, every 60s)

GC heartbeat. `timed_out` = jobs transitioned from `pending` to
`failed_timeout` this pass. `tombstoned` = terminal jobs whose payload was
stripped. `deleted` = tombstones purged from the database.
Action: if this line is absent for more than 2 minutes, the GC goroutine has
stalled — restart the daemon and open a bug. Otherwise ignore.

**`async proxy: job queue full, rejecting request`** (WARN)

Backpressure signal. Clients are receiving `503 queue_full` responses.
Action: grep for frequency. If sustained, either increase
`api.async.max_concurrent_jobs` or investigate upstream slowness causing jobs
to pile up (check `pending_jobs_count` on `/v1/admin/health`).

**`forwarder: response body truncated at cap`** (WARN)

The upstream returned more than 10 MB and the daemon truncated the response
before storing it. The stored (and replayed) body is the first 10 MB only.
Action: if this appears for legitimate upstream responses, the async path is
the wrong transport for that workload. Those clients should submit without
`Proxy-Retry-Tag` to use the v1 sync streaming path.

## Storage and lifecycle

The async subsystem uses a single [bbolt](https://github.com/etcd-io/bbolt)
file (configurable via `api.async.storage_path`). All records live in one
bucket (`jobs`); the key is the retry tag and the value is a JSON-serialised
job record.

The state machine is: `pending → {completed | failed | failed_timeout} →
tombstone → DELETE`.

On submit, a record is written as `pending`. On job completion (upstream
responded) or error, it transitions to `completed` or `failed`. Jobs that
exceed `pending_timeout` with no completion are marked `failed_timeout` by the
GC. All three terminal states age into `tombstone` after `complete_ttl` (the
response payload is dropped at this transition to bound disk usage). Tombstones
age out and are deleted after `tombstone_ttl`. Request bodies are never
persisted — only metadata and, on completion, the upstream response headers and
body (capped at 10 MB) are stored.

bbolt uses a write-ahead log and copy-on-write pages; records are always
either fully written or absent — there is no partial-state scenario after an
unclean shutdown.

**Sizing guidance:** tombstone records are approximately 80 bytes each. At
100 jobs per minute with default TTLs, tombstone accumulation peaks around
11 MB before GC clears them. Completed records retain the upstream response
body (up to 10 MB per job); at `max_concurrent_jobs = 100` the concurrent
working set is at most ~1 GB body data in the worst case, decaying as jobs
tombstone and GC runs. Monitor `tombstone_jobs_count` on `/v1/admin/health`
for abnormal growth.

**Backup:** bbolt holds an exclusive file lock; a `cp` of the live file
produces a crash-consistent copy (bbolt uses copy-on-write pages). However,
the snapshot is point-in-time and any pending jobs in it will be deleted on
the next daemon startup (per restart-recovery semantics). Backup the file
after a clean daemon stop if you need pending records to survive. Completed
and tombstone records are safe to back up at any time.

## Health endpoint

`GET /v1/admin/health` includes three async-specific integer fields:

| Field | Meaning |
|---|---|
| `pending_jobs_count` | Jobs currently in `pending` state |
| `completed_jobs_count` | Jobs currently in `completed` state (upstream responded successfully), still within `complete_ttl`. Does **NOT** include `failed` or `failed_timeout` records. |
| `tombstone_jobs_count` | Jobs in `tombstone` state, awaiting deletion after `tombstone_ttl` |

A value of `-1` for any field means "count temporarily unavailable" — the bbolt
scan for that count returned an error. Check the operational log (journalctl) for
the underlying cause. This is transient; the next health poll should succeed.

Operators can derive approximate throughput by sampling these counts at two
points in time. If `tombstone_jobs_count` is growing unboundedly, check
`tombstone_ttl` and whether the GC heartbeat (`async_gc_pass`) is being emitted.

**Monitoring snippet:**

```sh
curl -sk \
  -H "X-Vpntunnel-Token: $(cat /opt/vpntunnel/auth/admin.token)" \
  https://127.0.0.1:8888/v1/admin/health \
  | jq '{pending_jobs_count, completed_jobs_count, tombstone_jobs_count}'
```

Returns the three async counters alongside the tunnel health block. A value of
`-1` for any counter means the bbolt scan failed transiently; check
`journalctl -u vpntunnel` for the cause.
