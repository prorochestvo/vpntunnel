# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project State

`httpproxy` is a lightweight forward HTTP proxy that routes all egress through a
userspace WireGuard tunnel (no root required). v4 ships one binary distributed as
a Docker image (`ghcr.io/<owner>/httpproxy`): the long-running daemon (`cmd/httpproxy/`).
Two environments, two hosts: staging deploys automatically on every push to `main`;
production deploys on every `v*` tag after a manual approval gate. Server-side state
lives at `/opt/httpproxy/` on each host and is hand-authored once during setup.

Operator flow (binary): download a wg-quick `.conf` from mullvad.net or any WireGuard
provider → drop it into `./configs/tunnels/` → add its path to `configs/proxy.json`
under `upstream.configs` → run `./build/httpproxy -config configs/proxy.json`.

Operator flow (Docker): drop the host config tree at `./configs/`, the host log dir at
`./logs/` (chowned to UID 65532), `docker compose up -d`.

### Binaries

- `cmd/httpproxy/` — long-running daemon; composition root wired inline in `main.go`.

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
| Orchestration | `internal/service` | `ProxyService`: `HandleHTTP`, `HandleCONNECT`, `WaitTunnels`. |
| Transport | `internal/transport/httpserver` | Thin `*http.Server` wrapper, graceful shutdown. |
| Health | `internal/health` | `/healthz` handler; consumes `tunnel.HealthReporter`. |
| Admin transport | `internal/transport/adminserver` | Thin `*http.Server` wrapper for the admin/probe listener (loopback default). |

### HTTP routes

The proxy listener (default `127.0.0.1:8080`) dispatches `CONNECT` to `HandleCONNECT`
(hijack + bidirectional copy) and everything else to `HandleHTTP` (absolute-URI forward,
strips hop-by-hop headers). When auth is configured, every request is challenged with
`407 Proxy Authentication Required` before any hijack or forwarding occurs; the
`Proxy-Authorization: Bearer <token>` header is required. The admin listener (default
`127.0.0.1:8081`) serves `GET /healthz` and returns 404 for everything else. No REST
API beyond `/healthz`.

### Config schema (v4)

Flat upstream block plus optional admin, health, and auth blocks.

```json
{
  "upstream": {
    "configs": ["./tunnels/se-sto-wg-001.conf"],
    "active": "se-sto-wg-001"
  },
  "admin": {
    "listen": "127.0.0.1:8081",
    "shutdown_timeout": "5s"
  },
  "health": {
    "handshake_max_age": "180s"
  }
}
```

Optional auth block (pick one of `token` or `token_file`; both set is a config error):

```json
{
  "auth": {
    "token_file": "./auth/token"
  }
}
```

`configs` paths are resolved relative to the directory containing `proxy.json`
(not `os.Getwd()` — systemd processes have cwd `/`). Absolute paths are used
as-is. `active` is the basename without `.conf`; empty defaults to `configs[0]`.

WireGuard options (`PrivateKey`, `Address`, `DNS`, `Endpoint`, `AllowedIPs`,
`PersistentKeepalive`, `PresharedKey`, `MTU`) are read from the `.conf` file via
the `wgconf` parser.

`admin`, `health`, and `auth` blocks are optional — missing → defaults apply
(auth missing means auth disabled). `health.handshake_max_age` defaults to 180s,
which is ~3× the 25s persistent keepalive plus a safety margin so quiet tunnels
don't flap.

### Tunnel storage

`.conf` files live in `./configs/tunnels/` (gitignored). Only
`./configs/tunnels/.gitkeep` is tracked. Treat each `.conf` like an SSH private
key (mode 0600, never commit to a shared repository).

### Dependencies

| Module | Use |
|--------|-----|
| `golang.zx2c4.com/wireguard` | Userspace WireGuard device + gVisor netstack TUN. |
| `golang.zx2c4.com/wireguard/wgctrl` | WireGuard key parsing (`wgtypes`). |
| `golang.org/x/net` | Transitive dep of wireguard-go (not imported directly). |
| `gopkg.in/natefinch/lumberjack.v2` | Rotating access log file. |
| `github.com/stretchr/testify` | Test assertions (test-only). |

Note: `golang.zx2c4.com/wireguard/tun/netstack` is a sub-package of the
`wireguard` module (not a separate module). gVisor (`gvisor.dev/gvisor`) is
a transitive dep of netstack — adds ~20MB to the binary and ~10-50MB RSS.
Pin `gvisor.dev/gvisor` to `v0.0.0-20250503011706-39ed1f5ac29c`; newer
revisions have a "two packages in same dir" build error.

### Deployment

One static binary (`CGO_ENABLED=0`). `httpproxy` runs as a daemon; bind to
`127.0.0.1` (default). The `.conf` files in `./configs/tunnels/` contain private
keys — treat them like SSH keys (mode 0600, never commit to a shared repo).

Container image at `ghcr.io/<owner>/httpproxy`, built by the shared reusable workflow
`.github/workflows/build-image.yml`. Linux amd64 only. Tags published per environment:

- **Staging** (`.github/workflows/staging.yml`, triggers on `push: branches: [main]`):
  `:main-<7-char-sha>` (immutable) and `:edge` (floating).
- **Production** (`.github/workflows/release.yml`, triggers on `push: tags: ['v*']`):
  `:X.Y.Z` (immutable), `:X.Y` (floating minor), and `:latest` (floating).

Operator runs via `docker compose up -d` on each host (see README).

## Commands

A `Makefile` exists at the repo root with the standard targets:

```bash
make build            # builds ./build/httpproxy
make build-httpproxy  # ./build/httpproxy
make test             # gofmt check + go vet + go test -race ./...
make lint             # go vet + forbidden-imports check
make fmt              # gofmt -w .
make run              # go run ./cmd/httpproxy -config ./configs/proxy.json
                      # NOTE: requires configs/proxy.json to have upstream.configs populated
make clean            # rm -rf ./build ./tmp/*.tmp
```

Use the Go toolchain directly for one-off commands. Build with `CGO_ENABLED=0`
unless the project is intentionally changed to need CGO.

```bash
# Format + vet + race tests for the whole module
CGO_ENABLED=1 go test -race ./...

# Single top-level test
CGO_ENABLED=1 go test -race -run TestFunctionName ./<package>/

# Single subtest
CGO_ENABLED=1 go test -race -run 'TestFunctionName/subtest_name' ./<package>/

# Verbose output (see every subtest pass/fail)
CGO_ENABLED=1 go test -race -v ./<package>/

# Benchmarks
CGO_ENABLED=0 go test -bench=. -benchmem -run=^$ ./<package>/

# Coverage
CGO_ENABLED=1 go test -race -coverprofile=cover.out ./... && go tool cover -html=cover.out

# Build a binary (always into ./build/, never the repo root — see Constraints)
CGO_ENABLED=0 go build -o ./build/<name> ./cmd/<name>/
```

Once a `Makefile` exists, the standard targets are expected to be: `make build`,
`make run`, `make test` (fmt + vet + race), `make lint` (vet + forbidden-imports check),
`make format`, `make clean`. Document them here at that point.

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
- **Why:** the shared tree is for the genuinely shared layers of one app. Putting
  single-consumer code (or a separate app) there bloats it and implies a contract
  that doesn't exist; a `pkg/` package nobody outside the module imports is dead
  weight. Before placing or keeping a package in the shared or public tree, check
  who actually imports it — one consumer means co-locate, no external module means
  keep it private. Never keep something in the shared tree just because it's
  "reusable in principle"; treat such a move as its own deliberate refactor.

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
- **Why:** premature extraction imposes a contract where code should diverge. Dedup
  earns its place only when it names a non-obvious invariant, removes a real
  divergence risk, or cuts genuine cognitive load — not because two snippets look
  alike.

### Business logic is organized by concern, not by launcher

- Business-logic packages are judged by being **simple and isolated**, regardless of
  which binary runs them or how they are launched ("how it starts is not the
  package's concern"). Keep a flat, per-concern split.
- Do **not** reorganize business logic by runtime-vs-operator, by deployment, or by
  consuming binary.
- **Why:** grouping by launcher couples organization to deployment, which changes;
  cohesion by concern is stabler. Isolation + simplicity is the real quality bar.

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

#### Error handling in the controller

The controller catches all errors from sub-handlers and sends the appropriate message.

```go
const errFallbackMessage = "Something went wrong. Try again later."
```

| Situation | What service returns | What user sees |
|-----------|---------------------|----------------|
| Expected business failure (validation, state) | `internal.NewPublicError("...")` | The exact message from `PublicError.Details()` |
| Unexpected / infrastructure failure | plain `error` | The fallback message |
| No error | `nil` | Normal happy-path response |

### Testing the error path

Every controller test that exercises an error branch **must** assert:

1. That a response was actually sent (the user is not left in silence).
2. That the sent text equals `PublicError.Details()` when the error is a `PublicError`.
3. That the sent text equals the fallback constant when the error is a plain error.

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
- **Forbidden imports**: list any modules that must never appear in `go.mod` (e.g.
  CGO-dependent drivers, code generators the team has rejected). Enforce via the
  lint target once the Makefile exists.
- **Testing**: Use `github.com/stretchr/testify`; run tests with `-race`;
  parallel subtests preferred where there's no shared mutable state. The
  race step in `make test` does NOT pin `CGO_ENABLED` — Go 1.26's race
  detector on Linux links libtsan via cgo and refuses to run with
  `CGO_ENABLED=0`. Letting the env default through means runner picks
  cgo (Linux has it available), darwin uses its built-in race-detector,
  and we never explicitly enable cgo for production builds (those keep
  `CGO_ENABLED=0`).
- **One `Test*` per method, scenarios as subtests**: each tested method/function gets
  exactly one top-level test function named after it (e.g. `TestEncode` for `Encode`),
  and every scenario for that method lives as a `t.Run("descriptive name", ...)`
  subtest inside it. Do **not** create separate top-level tests like
  `TestEncode_EmptyInput`, `TestEncode_Unicode`, `TestEncode_Error` — these belong
  as subtests of a single `TestEncode`. Methods on a type follow the same rule with
  the standard `TestType_Method` form (e.g. `TestUser_Validate`).
  ```go
  func TestEncode(t *testing.T) {
      t.Parallel()

      t.Run("empty input returns empty string", func(t *testing.T) {
          t.Parallel()
          // ...
      })

      t.Run("unicode is preserved", func(t *testing.T) {
          t.Parallel()
          // ...
      })

      t.Run("returns error on invalid byte", func(t *testing.T) {
          t.Parallel()
          // ...
      })
  }
  ```
- **No CGO in production**: Build with `CGO_ENABLED=0` (static binary,
  no glibc/musl link). `go test -race` does NOT pin the env — it lets
  Go pick the default per-platform (cgo on Linux, built-in on darwin)
  so the race detector works without us forcing `CGO_ENABLED=1`. See
  the Testing entry above. `go build` and `go vet` always pin
  `CGO_ENABLED=0`.
- **Compile-time interface checks**: Every mock/stub struct in test files must have a
  `var _ interfaceName = &mockStruct{}` assertion at the top of the file.
- **No section-divider comments**: Do not use `// --- section ---` or `// ----` style
  separator comments. Let the code structure speak for itself.
- **No skipped errors**: Never use `_` to discard error return values in production or
  test code. Always capture the error and assert/check it. The only exceptions are
  `fmt.Fprint*` writes to loggers, `Rollback()` calls in error-recovery paths, and
  resource `.Close()` in `t.Cleanup` / `defer`.
- **Comments**: all comments are in English and start with a lowercase first word
  (e.g. `// wrap the driver error so callers can match on it`).
- **Godoc on exported identifiers**: Every exported identifier (Type, Func, Method,
  Var, Const) gets a doc comment that starts with the identifier name and ends with
  a period — e.g. `// Encode returns the base64-encoded form of v.` Each package
  has exactly one `// Package <name> ...` declaration; `cmd/*` entry points use
  `// Command <name> ...` instead. Skip the comment entirely if it would only
  restate the signature — no `// Foo is a Foo.` fluff. Document concurrency
  guarantees, which methods return `PublicError` vs plain errors, constructor
  lifecycle contracts ("caller must Close"), and error sentinel conditions.
  Preserve existing WHY-comments verbatim; do not overwrite a substantive comment
  with a generic restatement. Unexported symbols only get comments when intent is
  non-obvious — do not bulk-add comments to private helpers.
- **Build outputs live in `./build/`, scratch in `./tmp/`, logs in `./logs/`**:
  Never run `go build` without `-o ./build/<name>` — bare `go build ./cmd/<binary>`
  drops a binary in the project root, which is **not** in `.gitignore` and
  would be picked up by `git add .`. The same applies to any throwaway artifacts,
  fixtures, or intermediate files: use `./tmp/` rather than the repo root. Runtime /
  cyclic logs go to `./logs/`. Only these three directories are gitignored at the root.
- **Docker image is distroless** (`gcr.io/distroless/static-debian12:nonroot`); runs
  as UID 65532; no shell. The `-healthcheck` binary mode is the Docker `HEALTHCHECK`;
  it TCP-dials `127.0.0.1:8081` and never reads config or logs.
- **`/healthz` response body is fixed**: never include `peer_endpoint`, key material,
  or any field outside `{status, reason, handshake_age_seconds}`. The 503 body is the
  diagnostic surface for operators only; tightened scope prevents accidental leakage
  if the admin port is ever exposed.
- **Deploy SSH keys (`SSH_PRIVATEKEY` GH secret, scoped per-environment) are ed25519,
  dedicated per server, passphrase-less. The same identifier resolves to different
  values in the `staging` and `production` GH Environments — never generate the same
  key for both hosts.** Never log private-key contents in workflow output. The
  deploy workflow populates the runner's `~/.ssh/known_hosts` via `ssh-keyscan`
  at deploy time (trust-on-first-use — there is no pinned host fingerprint).
  The SSH port is configured per-environment via `SSH_HOSTPORT`, so non-standard
  ports are supported without code changes.

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
3. **Complete** — once every acceptance criterion is met and the test suite passes, rename
   and move the file to `plans/completed/` using the date-based convention:
   ```bash
   mv plans/001-fix-auth.md plans/completed/260422.0001.fix-auth.md
   ```
4. **Archive** — if a plan is abandoned or superseded without being fully implemented or
   if we need to save intermediate data or task execution logs, move it to `plans/history/` instead.

### Plan file format

Every plan file follows this structure:

```markdown
# Task Breakdown

## Overview
## Assumptions
## Tasks
### Task N: <Title>
- Description:
- Acceptance Criteria:
- Pitfalls & edge cases:
- Complexity: Easy / Medium / Hard
## Execution Order
## Risks
## Trade-offs
```

### Rules

- **One plan per concern.** Don't bundle unrelated changes in a single plan file.
- **Plan before code.** Claude must create (or confirm an existing) plan file before
  writing or modifying any source files.
- **Keep plans honest.** If implementation diverges from the plan, update the plan file
  before moving it to `completed/`.
- **Slug matches intent.** The description part of the filename should be readable at a glance:
  `002-add-rate-limiting.md`, `003-migrate-sqlite-to-postgres.md`, not `004-task.md`.

## Agent Pipeline

All non-trivial tasks follow a three-stage pipeline using specialized agents. The
review stage fans out to **three `gocode-reviewer` instances running in parallel**,
each with a distinct lens. A separate `gocode-testdoctor` agent is invoked
on-demand whenever tests fail, at any stage.

```
User describes task
    ↓
1. gocode-architect
    → Creates plan file at plans/NNN-slug.md (see Planning Workflow)
    ↓
2. gocode-engineer
    → Implements the tasks defined in the plan
    ↓
3. gocode-reviewer × 3 (run in parallel — single message, three tool calls)
    Lens A: correctness & tests — bugs, races, edge cases, error paths,
            context propagation, resource cleanup, test coverage,
            test structure (one Test* per method with subtests),
            scenario completeness, fixtures
    Lens B: security & operations — input validation, auth boundaries,
            secrets handling, injection (SQL, command, template),
            observability (logs, metrics, traces), log volume,
            operator/runbook UX
    Lens C: performance & architecture — allocations, blocking I/O,
            goroutine/resource leaks, layer boundaries, dependency
            direction, API contracts (breaking changes, exported
            surface stability), interface scope, future-proofing
    ↓
   Orchestrator synthesises all three reports, deduplicates findings,
   resolves conflicts (e.g. one reviewer flags as P0 what another
   accepts as a trade-off), and presents the merged punch list to the user.
    ↓
  ❌ P0/P1 found?  → Back to gocode-engineer with the consolidated findings.
                             After fix, run ONE targeted reviewer pass on the changed
                             lines (not all 3 again) before re-approval.
  ⚠️  Tests failing?        → gocode-testdoctor diagnoses and patches, then rerun the
                             targeted reviewer pass.
  ✅ All three approve?     → Orchestrator moves the plan: mv plans/NNN-slug.md
                             plans/completed/YYMMDD.NNNN.slug.md
```

### Agent responsibilities

| Agent | Owns | Output |
|-------|------|--------|
| `gocode-architect` | Planning, decomposition, trade-offs | New plan file in `plans/` |
| `gocode-engineer` | Implementation, tests for new code | Code + tests in the repo |
| `gocode-reviewer` (×3, parallel) | Lens-specific verdicts, priority-ranked findings, patch sketches | Three independent review reports |
| `gocode-testdoctor` | Triage of failing tests, minimal patches | Code/test fixes, re-run of the test suite |

The orchestrating agent (the main Claude session driving the pipeline) owns
synthesis: merging the three reports, resolving conflicting verdicts, deciding
which findings to act on, and moving the plan to `completed/` once everyone
signs off.

Priority scale used by reviewers: **P0 / P1 / P2 / P3**.

### Rules

- **No skipping stages.** Every task starts with the architect and ends with the three-reviewer fan-out.
- **Plan file first.** The architect MUST produce a plan file before any code is written. If a plan already exists for the task, update it rather than creating a new one.
- **Three reviewers, three lenses, one message.** All three `gocode-reviewer` agents are launched in a single tool-call batch (multiple `Agent` blocks in one message) so they run in parallel. Each prompt names the lens explicitly and tells the agent what to SKIP (the other lenses) to avoid duplicated work.
- **No solo reviewer pass on first review.** Even for small changes the full three-lens fan-out is required, because the lenses catch genuinely different classes of issue (Lens A won't see ops/log-volume problems; Lens C won't see test gaps). Skipping lenses is what the orchestrator does AFTER a P0/P1 fix, not BEFORE the first verdict.
- **Lens prompts are self-contained.** Each reviewer's prompt must include: (1) the lens name, (2) what to focus on, (3) what to SKIP (so it doesn't restate other lenses), (4) the file list, (5) the deliverable shape (P0 / P1 / P2 / P3 with `file:line` + patch sketch), (6) the word cap (typically 600 words).
- **Re-review after fixes is single-pass.** Once an engineer addresses P0/P1 findings, the orchestrator runs ONE reviewer pass scoped to the changed lines, not the full fan-out. Re-running all three each iteration is expensive and rediscovers nothing.
- **Conflict resolution is explicit.** When reviewers disagree (one says P0, another says trade-off), the orchestrator chooses, names the rejected suggestion, and explains the reasoning to the user before moving on. The user has final say.
- **Orchestrator gates completion.** The plan moves to `plans/completed/` only after every reviewer's P0 and P1 findings are addressed (either fixed, or explicitly accepted with rationale). The rename uses the standard `YYMMDD.NNNN.slug.md` format.
- **Test suite must pass** before review begins. If it fails, hand the logs to `gocode-testdoctor` first — reviewers should not waste time on a red tree.
- **Testdoctor is scoped.** It patches tests or the minimal production code needed to make the failure go away. It does not redesign or refactor.
