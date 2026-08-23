---
name: vpntunnel-http-api
description: The vpntunnel proxy and API listeners — the CONNECT/absolute-URI split on 127.0.0.1:7788, the 407 Proxy Authentication Required challenge and its loopback bypass, the X-Vpntunnel-Token admin/proxy role split on 127.0.0.1:8888, the frozen v1 route table, the break-before-make /v1/admin/rotate, and the fixed /v1/admin/health body. Load before touching internal/gateway/* (httpserver, router, middleware, httpV1 routes / handlers / dto), internal/application/proxy.go, internal/infrastructure/ipdeny, ProxyService, HandleHTTP, HandleCONNECT, WaitTunnels or RequestSummary; before adding, moving or renaming any `/v1/tunnels`, `/v1/tunnels/{id}/proxy/{scheme}/{rest...}`, `/v1/admin/health` or `/v1/admin/rotate` route; and before changing the `routes.*` constants, `X-Request-Id`, `X-Proxy-Error`, `Proxy-Authorization`, `?force=true`, `?country=us,se`, or anything that renders `TunnelHealth`.
---

# vpntunnel HTTP surface

Two listeners, two different auth models. They share a process and nothing else.

## Proxy listener (default `127.0.0.1:7788`)

Dispatches `CONNECT` to `HandleCONNECT` (hijack + bidirectional copy) and everything else to
`HandleHTTP` (absolute-URI forward, strips hop-by-hop headers). Both hang off `ProxyService`
in `internal/gateway/httpserver`, a thin `*http.Server` wrapper with graceful shutdown;
`WaitTunnels` is the readiness barrier it blocks on before serving.

When auth is configured, **every request is challenged with `407 Proxy Authentication
Required` before any hijack or forwarding occurs** — the order is load-bearing, because a
hijack that happens first has already handed the socket over and there is no status line
left to write. The `Proxy-Authorization: Bearer <token>` header is required, **except for
clients connecting from the loopback interface, which bypass the token check**. That bypass
is why the listener binds loopback by default and why moving it to a routable address is a
security change, not a config change.

## API listener (default `127.0.0.1:8888`)

Plain HTTP when `-tls-cert-dir` is empty, HTTPS (TLS 1.3) when set. Requires one of two
Bearer tokens sent via **`X-Vpntunnel-Token`** — not `Authorization`. Two roles:

- **admin** — full access (`api.auth.admin_token_file`)
- **proxy** — forward-proxy passthrough plus the tunnels catalog (`api.auth.proxy_token_file`)

**The tunnel is selected by the `{id}` path segment** (the HMAC tunnel id). There is no
`X-Tunnel-Id` header; if you find code reading one, it is a leftover.

### The frozen route table

Patterns live as exported constants in `internal/gateway/httpV1/routes` so the wire contract
sits in one reviewable place. The values are path patterns only — the method, where a route
is method-scoped, is applied at the mux registration site in the router package.

| Method | Constant | Path | Roles |
|--------|----------|------|-------|
| `GET` | `routes.AdminHealth` | `/v1/admin/health` | admin |
| `POST` | `routes.AdminRotate` | `/v1/admin/rotate` | admin |
| `GET` | `routes.Tunnels` | `/v1/tunnels` | admin, proxy |
| `*` | `routes.TunnelProxy` | `/v1/tunnels/{id}/proxy/{scheme}/{rest...}` | admin, proxy |

**`AdminRotate` is registered methodless on purpose** — the handler enforces `POST` itself
and replies 405 with `Allow: POST`. Registering it as `POST` would let the CatchAll route
swallow a non-POST request down the 404 path, turning "wrong method" into "no such route".
`TunnelProxy` is methodless because any method is proxied.

All other paths return 404 with a JSON error envelope. Every response carries `X-Request-Id`
(UUIDv7), and `X-Proxy-Error` on error paths.

## `/v1/admin/rotate`

Triggers a graceful streaming-tunnel rotation, gated on zero active **streaming** sessions
unless `?force=true`.

It always does **break-before-make + settle**: tear the current exit down, wait the
on-demand settle delay, then bring a fresh random exit up. Make-before-break would hold two
streaming sessions at once and breach the host session budget — see the tripwire in
`CLAUDE.md`. The streaming role self-limits to ≤1 session, so on-demand is **intentionally
not consulted** and the host's 2-device ceiling holds with no cross-role coordination. Do
not "optimise" the settle pause away; it is what makes the ceiling hold across a switch.

## `/v1/tunnels`

Returns the FULL discovered catalog grouped by country as a `map[country][ids]` JSON object.

It is a **static catalog, not a live-device report** — it contains no `healthy` and no
`handshake_age` fields, and listing a tunnel says nothing about whether a device for it is
up. An optional `?country=us,se` query parameter narrows the result; an unmatched filter
returns `{}` with 200, never 404.

Note the asymmetry with `vpnstream.allowed_countries`: this catalog and the on-demand
scheduler both use the **full unfiltered** discovered set, while `allowed_countries` scopes
the streaming random-pick only.

## `/v1/admin/health` — the body shape is fixed

Top-level `{status, tunnels: [...]}`; each tunnel entry is exactly
`{id, healthy, handshake_age_seconds}`.

**Never add `peer_endpoint`, key material, the peer public key, or `TunnelHealth.Err` text.**
The 503 body is a diagnostic surface for operators only, and the tightened scope is what
prevents accidental leakage if the API port is ever exposed. The derived hex tunnel ids are
non-secret and safe to include.

With lazy building enabled the endpoint reports only the **currently live** devices
(1 streaming + 0..1 on-demand), not the catalog. The body shape does not change with the
device count.

> This service exposes no `/ping` and no `/health/check` pair. `/v1/admin/health` is
> authenticated readiness only. Treat that as a known gap, not as a precedent.
