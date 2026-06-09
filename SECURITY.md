# Security Policy

## Supported versions

This project ships from a single branch. The actively supported versions are:

- The latest `vX.Y.Z` tag (production image: `ghcr.io/<owner>/httpproxy:latest`).
- The current `main` branch (staging image: `ghcr.io/<owner>/httpproxy:edge`).

Older tags are not patched. If you find a vulnerability that also affects an
older tag, the fix is rolled forward via a new release — there is no separate
backport stream.

## Threat model

### What `httpproxy` protects

- **Egress IP privacy.** Every byte leaving the proxy exits via the configured
  WireGuard tunnel. Upstream origins see the WireGuard peer's IP, never the
  proxy host's real IP.
- **Token-gated access.** Optional Bearer-token authentication
  (`Proxy-Authorization: Bearer <token>`) refuses unauthenticated clients
  with a uniform `407` response. The token is compared in constant time
  (`crypto/subtle.ConstantTimeCompare`) — no length oracle, no early exit.
- **Secret containment.** WireGuard private keys and Bearer tokens are never
  logged, never echoed in error messages, never present in HTTP response
  bodies. The access log records request metadata only; no `Authorization`
  or `Proxy-Authorization` headers reach disk.

### What `httpproxy` does NOT protect

- **The client → proxy hop.** The proxy listener speaks plain HTTP. Use it
  on `127.0.0.1`, an SSH tunnel, or a Tailscale / WireGuard overlay network
  if the client is not co-located.
- **Client-side DNS leaks.** A client that resolves names locally before
  dialing through the proxy leaks those names to its local resolver. Either
  resolve through the proxy (`curl --proxy-anyauth ...`) or use DoH on the
  client.
- **Traffic analysis at the exit.** The WireGuard provider sees encrypted
  blobs but knows which exit IP corresponds to which client connection.
  Your threat model must account for them.
- **Compromised clients.** Anyone with the Bearer token can use the proxy.
  Token rotation is operator-managed; there is no per-client identity.

## Secrets discipline

- WireGuard `.conf` files live in `./configs/tunnels/`, mode `0400`,
  gitignored.
- Bearer tokens live in `./configs/auth/`, mode `0400`, gitignored.
- Deploy SSH keys (`SSH_PRIVATEKEY` GH secret, scoped per-environment) are ed25519,
  passphrase-less, dedicated per server. The same identifier resolves to different
  values in the `staging` and `production` GH Environments; never reuse the same
  key across them.
- The `/healthz` endpoint is unauthenticated by design. Keep it on loopback
  unless you intend to expose tunnel-uptime as a public oracle.

## Reporting a vulnerability

Please **do not** open public GitHub issues for security problems.

Use one of:

- **GitHub Security Advisories** — open a private advisory at
  `https://github.com/<owner>/httpproxy/security/advisories/new`.
- **Email** — `seilbekskindirov@gmail.com`.

Include a description, reproduction steps, and the affected version (tag
or commit SHA). A response acknowledging receipt is sent within 72 hours.

## Dependency policy

- `gvisor.dev/gvisor` is pinned to `v0.0.0-20250503011706-39ed1f5ac29c`.
  Newer revisions have a known "two packages in same directory" build error.
  Upgrades happen via PR after verifying the build succeeds.
- `golang.zx2c4.com/wireguard` and `wgctrl` are tracked at their latest
  stable; transitive `golang.org/x/net` follows.
- Third-party GitHub Actions are currently pinned by floating major tags
  (`@v1`, `@v4`, `@v5`). SHA-pinning is on the hardening backlog
  (`README.md` → §10).
