# Task Breakdown

Source review: [`plans/review.20260725-v001.md`](review.20260725-v001.md) (owner, 2026-07-25), items
T001–T008 plus the header ask. Predecessor backlog:
[`plans/000-review-20260717-backlog.md`](000-review-20260717-backlog.md).

Baseline: HEAD `998a26c`, working tree clean, `make test` green, `go build ./...` OK.

## Overview

The 2026-07-25 owner review is a second pass over the tree left by the DDD-pragmatic
restructure. It asks for eight concrete changes: two that simplify the composition root
(`cmd/vpntunnel/main.go`), one that dissolves the shared egress-port package into
consumer-local contracts, one whole-repo declaration-order and test-layout
normalization, three that relocate constants and a domain type out of implementation
packages, and one that folds a single-function file back into its caller. The header ask
— "a global rule or skill that checks and normalizes my Go style automatically" — is
satisfied by extending the personal standards auditor rather than by building a linter.

This plan is a pure-refactor track: **no behaviour change, no new features, no config or
API surface change.** Every task ends with `make test` green, and every task is a
separate commit (T004 is several) so a bisect stays meaningful.

## State correction — the backlog is further along than assumed

The task brief stated that backlog Wave 3 (T14, S1–S4) and Wave 4 (T15–T18) are still
open. **Verified against HEAD `998a26c`, they are not.** This matters because it changes
the "what goes first" question from "this review vs. two open waves" to "this review vs.
three loose ends".

| Backlog item | Claimed | Verified at HEAD `998a26c` | Evidence |
|---|---|---|---|
| T14a collapse `run`/`runWithOpts` | open | **DONE** | one `run` at `cmd/vpntunnel/main.go:224` |
| T14b `parseFlags()` from `main`, `tlsOptions` threaded (D5) | open | **DONE** | `main.go:153` / `main.go:180` |
| S1 contract assertions → tests | open | **DONE** | zero `var _ Iface =` in any non-test file |
| S2 redundant import aliases | open | **DONE** | zero `alias "vpntunnel/…"` imports |
| S3 env names → constants | open | **DONE** | `constants.EnvTelegramBotDSN`, `main.go:249` |
| S4 constructors take `dsninjector.DataSource` | open | **DONE** | `notify.NewTelegram(ds, tag, opLog)` |
| T15 rename `lazy` | open | **DONE** | package is `internal/application/tunnelpool` |
| T16 notify → `go-telegram/bot` | open | **DONE** | `go-telegram/bot v1.22.0` in `go.mod` |
| T17 observability → `loginjector` | open | **DONE** | `observability/access.go:14` imports it |
| T18 consolidate `configs/` + `deploy/` | open | **DONE** | no `deploy/` dir exists |
| T01 `generatevpnconfig` → shell script | Wave 1 | **CANCELLED** | commit `4b6e543`, `plans/history/260721.0001.…` |

Genuinely still open from the backlog, and **out of scope here**:

- **T06** — justify or relocate `internal/application/export_test.go` (one-line white-box
  shim for `isLoopbackRemote`). Still `discuss`. It is a legitimate Go pattern; leave it.
- **T08c leftover** — the `TODO(T08c)` at `internal/domain/tunnel.go:30`: `TunnelID` is
  still not threaded through the `EligibleSet` catalog or the `{id}` handler, so the id
  travels as a bare `string` end to end. Needs its own plan.
- **D6** — whether `asyncjob` should be renamed. Default was "no". Leave it.

Consequence for the track decision: the owner's ordering ask ("this review first") costs
essentially nothing, because there is no half-finished backlog wave to strand.

## Assumptions

1. **Owner decisions taken as given** (not re-argued below, only encoded):
   - This review runs before the three remaining backlog loose ends.
   - **T003 overrides backlog decision D3 and the R2 caveat.** D3 and auditor rule R2
     both currently mandate keeping the four egress ports as one contract in a dedicated
     `internal/egress` package. The owner has reversed that. Newer owner word wins.
     Task 9 amends R2 so a future audit does not "fix" this back.
   - T004 covers the whole repo in one sweep: 58 production `.go` files and 58 `_test.go`
     files.
   - The header ask is satisfied by new `R#` rules in
     `~/.claude/agents/code-standards-auditor.md`. **No new linter binary, no `make lint`
     extension, no new skill.**
2. **The review template's misspellings are typos, not identifiers**: `PIUBLIC`,
   `PRIVITE`, `helpeer`, `privite`, `Cotract`, `internalCotract1`. The intent is the
   **order**. The snake_case in `helpeer_method1` / `test_helpeer_method1` /
   `Testhelpeer_methodN` is illustrative; real code keeps idiomatic Go names.
3. **T004 renames nothing.** Not a production identifier, not a test function, not a
   subtest (decision **D-A**). It moves declarations, relocates contract assertions, adds
   `t.Parallel()` where legal, and pushes helpers to the bottom. Any rename encountered
   as "obviously better" during the sweep is a separate plan.
4. Tests keep using `testify` (`assert` + `require`) — already a dependency, used in 52
   of 58 test files. The 6 files without it are not converted by this plan.
5. `make test` is the gate after every task. A red tree goes to `testdoctor` before the
   next task starts.
6. No production source file is touched by the plan author. Implementation is the
   `engineer` agent's job.

### Decisions made — all four are ruled; nothing here is open

The owner has ruled on every question this plan raised. They are recorded rather than
argued, and each is propagated into the affected task's steps and acceptance criteria.
**No task in this plan is blocked on a pending decision.**

**D-A · T004 subtest names: `subtest_NNN` is NOT literal.** *Ruled: placeholder.*
It sits in the same register as `Method1` / `ObjectN` / `PIUBLIC CONTACTS` in the same
template. **Existing descriptive `t.Run` names are kept verbatim; no subtest is renamed.**
This narrows Task 8 to exactly four operations, and nothing else:
1. declaration/section ordering (production and test files),
2. contract-assertion relocation to the top of the test file,
3. `t.Parallel()` on every test and subtest that can *legally* take it,
4. test helpers, test-only consts/vars, and test-only contracts moved to the bottom.

Consequences propagated into Task 8: the test-function **consolidation** step that an
earlier draft of this plan carried (merging `TestOnDemandScheduler_switch` and friends
into the `TestX` for their production method) is **out of scope** — it renames top-level
test identifiers, which "nothing else" excludes. It is recorded there as a follow-up.
Dropping it also makes Task 8 pure movement, which lets its verification gate tighten from
"movement plus enumerated exceptions" to "movement plus `t.Parallel()` lines only".

**D-B · T006b ipdeny: bounded version.** *Ruled: move the literal, keep the façade.*
Only the CIDR literal set moves into `package internal`. `ipdeny.DefaultDeny()` and
`ipdeny.Contains()` stay as the security façade; `ipdeny_test.go` and both
`handlers/forwarder.go` call sites are untouched. **Backlog decision D1 stands otherwise**
— the `ipdeny` package is not dissolved. Task 7's scope is this plus the `config.go:23`
constants, and nothing more.

**D-C · T001 uses `*tls.Certificate`, not the value type.** *Ruled: pointer.*
`nil` already encodes HTTP mode and `router.Options.Cert` is already `*tls.Certificate`,
so the pointer keeps both contracts intact; a value return would force either a
`len(cert.Certificate) == 0` sentinel or a change to `router.Options`. **This is a
deliberate deviation from the review's literal
`func parseFlags() (config.Config, tls.Certificate, error)`**, taken under the review's own
"примерно такой" (approximately this) wording. Recorded in Task 2 so it is not read as an
implementation slip.

**R2 amendment: approved.** Task 9 may edit the R2 caveat in
`~/.claude/agents/code-standards-auditor.md`. Scope of the edit is **only the clause that
mandates a dedicated `internal/egress`-style contracts package** — that clause currently
mandates precisely what T003 deletes, so leaving it would make the next audit reverse
T003. The rest of R2 stands, and **no other existing rule is touched.**

## Verified technical findings

Three of this review's items rest on claims that could be wrong. All three were probed in
a throwaway module before writing this plan; results below are reproduced, not reasoned.

### F1 · T003's compile break is real (and the fix is known)

Two packages each declaring their own local copy of a port **does not compile** when the
port appears in the *signature* of a cross-package interface method. Reproduced:

```
cannot use other.Scheduler{} (value of struct type other.Scheduler) as Router value
in variable declaration: other.Scheduler does not implement Router (wrong type for method Route)
        have Route(context.Context, string) (other.dialer, func(), error)
        want Route(context.Context, string) (consumerDialer, func(), error)
```

In this repo that is exactly **one** seam: `handlers.Router.Route`
(`internal/gateway/httpV1/handlers/zone_forwarder.go:44`), implemented by
`tunnelpool.OnDemandScheduler.Route` (`internal/application/tunnelpool/scheduler.go:156`)
and wired at `main.go:516` via `router.Options.ZoneRouter handlers.Router`
(`internal/gateway/router/server.go:63`).

**Everything else in T003 is safe**, because interface-to-interface assignment is
*structural*, not identity-based, and concrete-to-interface assignment likewise. So
`application`, `notify`, `wireguard`, `httpserver` and all test doubles can each declare
their own unexported copy with zero friction.

Three fixes for the one seam were all compiled successfully:

| Variant | Shape | Verdict |
|---|---|---|
| **A** | producer (`tunnelpool`) exports the ports; `handlers.Router` names `tunnelpool.Dialer`/`Resolver` | **chosen** |
| B | both sides spell the port as the *same unnamed interface literal* | works, unreadable — 6 sites × a 3-line literal |
| C | fully-local ports both sides + an adapter closure in the consumer package | works, but adds pass-through indirection (violates R9) |

**Variant A is chosen, and it is forced anyway**: `tunnelpool.DeviceBuilderFn`
(`supervisor.go:30`) and `tunnelpool.BuilderFn` (`build.go:19`) are *exported* func types
that return the port, and out-of-package callers must be able to write that signature
(`cmd/vpntunnel/main_test.go:240`, `rotate_test.go:151,182`). So `tunnelpool` must export
a `DialerCloser` name regardless of what happens to `internal/egress`. Variant A also
matches the owner's own principle — `tunnelpool` is the package that builds, owns, and
routes devices, i.e. the port *is* where it is used.

### F2 · T005's `package internal` works

`internal/constants.go` declaring `package internal` compiles, vets, and **is importable
by subpackages** as `vpntunnel/internal`. Reproduced with a scratch module
(`go build ./...` + `go vet ./...` both clean). The internal-visibility rule only requires
the importer to be rooted at `vpntunnel/`, which every `vpntunnel/internal/...` package
is. Import site: `import "vpntunnel/internal"` → `internal.EnvTelegramBotDSN`.
Precondition holds: there is currently **no** `.go` file directly under `internal/`, so
nothing else has to change package.

### F3 · T008's type is `Event`, not `Message`, and moving it creates no cycle

`internal/infrastructure/notify/notify.go:27` is `type Event struct`. There is no
`notify.Message` type anywhere; `message.go` holds only the unexported `formatMessage` /
`formatDetails` renderers, which are genuinely Telegram HTML concerns and must **not**
move. Moving `Event` (+ the `Source` enum it depends on) into `domain` creates **no import
cycle**: `domain` imports nothing outside stdlib, `notify` → `domain` is a new edge,
`tunnelpool` already imports both. Full analysis and the charter cost in Task 6.

---

## Tasks

Task numbers are execution order; the review's own id is given for cross-reference.

### Task 1 (review T002): Remove the `runOpt`/`runOptions` test seam from `main.go`

- **Description.** Delete `type runOpt func(*runOptions)` (`cmd/vpntunnel/main.go:67`),
  `type runOptions struct` (`:71`), the `var ro runOptions` / option-apply loop
  (`:225–228`), and the three option helpers in `cmd/vpntunnel/main_test.go`:
  `withSupervisorBuilder` (`:43`), `withSchedulerBuilder` (`:49`), `withShutdownCtx`
  (`:55`). Replace the mechanism with **ordinary parameters** on `run`:

  ```go
  func run(
      ctx context.Context,
      configPath string,
      tlsOpts tlsOptions,
      streamingBuilder tunnelpool.DeviceBuilderFn,
      onDemandBuilder tunnelpool.DeviceBuilderFn,
  ) error
  ```

  `main` supplies the production values it was defaulting to implicitly:
  `signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)` for `ctx`
  (with `defer stop()` moving up into `main`), and `tunnelpool.DefaultDeviceBuilder` for
  both builders. The `ro.shutdownCtx != nil` branch (`:293–299`) collapses to `ctx,
  stop := context.WithCancel(ctx)`; `run` keeps its own `WithCancel` so its `defer stop()`
  still bounds a hung startup DNS lookup.

  Functional options for three fields with exactly one production caller are the
  pass-through indirection R9 exists to catch. Explicit parameters at a composition root
  are the idiomatic shape.

- **Why this goes before Task 2.** Tasks 1 and 2 both rewrite `run`'s signature and its
  9 call sites in `main_test.go`, so one of them must be second either way. Task 1 first
  keeps the risky change isolated: Task 1 is pure parameter plumbing with zero I/O
  reordering and zero test rewrites, so it lands as a small reviewable diff against a
  green tree; Task 2 then writes the final signature exactly once, and its diff shows
  only the config/TLS/logger inversion instead of being entangled with option-removal
  churn. The reverse order was considered and rejected: Task 2-first would have to
  preserve the variadic `opts ...runOpt` tail it does not want, only for Task 1 to delete
  it immediately after.

- **Exact test dependencies on the seam, and how each is preserved.** No coverage is
  dropped. All 9 `run(...)` call sites become mechanical rewrites:

  | `main_test.go` site | Subtest | Seam used | After |
  |---|---|---|---|
  | `:357` | `TestRun/boots and shuts down on context cancel` | all three | `run(shutdownCtx, cfgPath, tlsOpts, smokeBuilder, smokeBuilder)` |
  | `:405` | `TestRun/async startup: recovery deletes pending records…` | all three | same shape |
  | `:449` | `TestRun/gc goroutine stops cleanly on context cancel` | all three | same shape |
  | `:494` | `TestRun/missing async store dir is created on startup` | all three | same shape |
  | `:533` | `TestRun/http mode (no cert dir): API answers over plain HTTP` | all three | same shape |
  | `:591` | `TestRun/https mode with unloadable cert dir fails startup` | all three | same shape |
  | `:661` | `TestRun/startup fails when allowed_countries matches no discovered config` | all three | same shape |
  | `:687` | `TestRun/telegram notifier disabled when …DSN is unset` | all three | same shape |
  | `:727` | `TestRun/malformed telegram dsn warns and disables…` | all three | same shape |

  Nothing moves "into the test" beyond the three deleted one-line helpers: `smokeBuilder`
  (`:240`) and the `shutdownCtx` already live in the test file. The injected-fake
  capability the owner asked to remove from production code is exactly what disappears —
  `runOptions`' three fields and the `runOpt` type — while the injection *points* become
  honest parameters.

- **Acceptance Criteria.**
  - `grep -rn 'runOpt\|runOptions\|withSupervisorBuilder\|withSchedulerBuilder\|withShutdownCtx' cmd/` returns nothing.
  - `run` has no variadic parameter.
  - `main` names `tunnelpool.DefaultDeviceBuilder` explicitly for both roles and owns the
    `signal.NotifyContext` + `defer stop()` pair.
  - All 9 `TestRun` subtests still exist and pass, plus `TestResolveAuthToken` (9 subtests)
    and `TestParseTLSOptions` (8 subtests) untouched.
  - `go test -v -run TestRun ./cmd/vpntunnel/ | grep -c '^=== RUN'` is unchanged from the
    pre-task baseline.
  - `make test` green.
- **Pitfalls & edge cases.**
  - `run` must still call `context.WithCancel(ctx)` and `defer stop()`; dropping it leaks
    the signal handler in production and un-bounds startup DNS.
  - `tunnelpool.DefaultDeviceBuilder`'s signature must match `DeviceBuilderFn` exactly —
    it does today (`supervisor.go:34`).
  - Do not reorder anything else in `run` in this task; keep the diff to the seam.
- **Complexity:** Easy.

---

### Task 2 (review T001): Replace `tlsOptions` with a finished `*tls.Certificate`

- **Description.** Delete `type tlsOptions struct` (`cmd/vpntunnel/main.go:88`) and
  `parseTLSOptions` (`:103`). Reshape the flag entry point so it hands `main` finished
  objects, per the review's `func parseFlags() (config.Config, tls.Certificate, error)`,
  with two deliberate deviations argued below:

  ```go
  // parseFlags parses the process CLI flags and turns them into the finished
  // startup objects. It calls flag.Parse, so it is only ever called from main —
  // never from a test (flag.Parse in a test process hits the -test.* flags).
  func parseFlags() (config.Config, *tls.Certificate, *slog.Logger, error)
  ```

  Three pure/testable helpers carry everything `parseTLSOptions` used to, so the struct
  dies without losing a single assertion:

  ```go
  // resolveCertDir returns certDir as an absolute path, preserving "" as the
  // HTTP-mode trigger (filepath.Abs("") would return the cwd and silently
  // re-enable HTTPS).
  func resolveCertDir(certDir string) (string, error)

  // parseIPSANs parses a comma-separated IP SAN list, tolerating blank entries.
  func parseIPSANs(raw string) ([]net.IP, error)

  // loadAPICert returns the API certificate, or (nil, nil) when certDir is empty
  // (plain-HTTP mode). It emits the no-TLS warning / TLS-enabled info line.
  func loadAPICert(certDir, hostname, ipSANsRaw string, opLog *slog.Logger) (*tls.Certificate, error)

  // newOperationalLogger builds the scrub-wrapped operational logger. Extracted so
  // parseFlags and the tests construct byte-identical loggers.
  func newOperationalLogger(cfg config.Operational) *slog.Logger
  ```

  `run` becomes:

  ```go
  func run(
      ctx context.Context,
      cfg config.Config,
      cert *tls.Certificate,
      opLog *slog.Logger,
      streamingBuilder tunnelpool.DeviceBuilderFn,
      onDemandBuilder tunnelpool.DeviceBuilderFn,
  ) error
  ```

- **Deviation 1 — `*tls.Certificate`, not `tls.Certificate` (ruled, decision D-C).**
  The review literally writes `func parseFlags() (config.Config, tls.Certificate, error)`.
  The owner has ruled for the **pointer** form. Reason: `nil` already encodes HTTP mode
  throughout the daemon (`main.go:477–492`), and `router.Options.Cert`
  (`internal/gateway/router/server.go`) is already `*tls.Certificate`. A value return
  would force either a `len(cert.Certificate) == 0` sentinel — a second, weaker way to
  spell "no TLS" — or a change to `router.Options`, neither of which the review asked for.
  The review's own wording is "примерно такой" (approximately this signature), so the
  pointer is inside the ask. **Record this in the commit body**, so a later reader does
  not mistake it for an implementation slip against the quoted signature.
- **Deviation 2 — `parseFlags` also returns the logger.** Unavoidable: TLS load/generate
  needs a logger (`apitls.LoadOrGenerate(dir, host, sans, opLog)`), and the logger is
  built from `cfg.Operational`, which only exists after `config.Load`. So the order must
  become flags → `config.Load` → logger → cert. Returning the logger is the only shape
  that avoids building **two** loggers and emitting the
  `"operational log scrubbing active"` line twice with inconsistent scrubbing.
  **Naming honesty (R13):** a function called `parseFlags` that reads JSON off disk,
  generates an ed25519 key pair, writes a certificate, and logs is misnamed. *Recommend
  renaming it `bootstrap()`*; keep `parseFlags` only if the owner prefers his own name.

- **The order inversion, stated plainly.** Today `run` does `config.Load` → logger → …
  → TLS cert load (`main.go:230`, `:237–239`, `:477–492`). After this task, all three
  happen in `parseFlags` before `run` is entered. The consequences are real:
  - `run` no longer performs *any* disk read before its first listener-affecting step.
    That is an improvement — the composition root's fallible I/O is front-loaded.
  - `run` can no longer fail with `"load config: …"` or `"load tls cert: …"`. Two
    existing assertions depend on that (below).
  - `config.Config` must start carrying its own directory, because `run` derives
    `configDir` from `configPath` (`main.go:302`) and uses it for the tunnels dir, the
    HMAC key file, the API tokens, and `resolveAuthToken`. **Add
    `config.Config.Dir string`**, set by `config.Load` to
    `filepath.Dir(<absolute path>)`, documented as derived (never operator-supplied,
    never a JSON key). This is additive — `config_test.go` asserts field-by-field, so no
    existing assertion breaks.

- **Test seam: what survives, what must be rewritten.** This is the honest accounting the
  brief asked for.

  *Preserved by relocation — all 8 `TestParseTLSOptions` subtests (`main_test.go:147`):*

  | Existing subtest | Moves to | Note |
  |---|---|---|
  | `empty hostname rejected` | `TestLoadAPICert` | assert error contains `-tls-hostname` |
  | `relative cert dir resolves against cwd` | `TestResolveCertDir` | stays `t.Parallel()` — pure helper, no `t.Chdir` needed |
  | `absolute cert dir passed through unchanged` | `TestResolveCertDir` | |
  | `comma-separated ip sans parsed` | `TestParseIPSANs` | |
  | `invalid ip rejected` | `TestParseIPSANs` | |
  | `empty ip-sans yields none` | `TestParseIPSANs` | |
  | `trailing comma tolerated` | `TestParseIPSANs` | |
  | `empty cert dir yields HTTP-mode tlsOptions` | `TestLoadAPICert` | rename to `…yields no certificate`; assert `(nil, nil)`. **Keep this one — it is the `filepath.Abs("")` regression guard.** |

  Splitting `resolveCertDir` and `parseIPSANs` out as pure functions is what lets these
  stay `t.Parallel()`. Folding them straight into `loadAPICert` would force `t.Chdir`
  (which forbids `t.Parallel()`) for the relative-path case.

  *Must be rewritten — 1 subtest:*
  `TestRun/https mode with unloadable cert dir fails startup` (`main_test.go:562`). It
  poisons a dir to `0777`, calls `run`, and asserts `err` contains `"load tls cert"`.
  After this task `run` never loads a cert, so **this subtest must move to
  `TestLoadAPICert/unloadable cert dir returns an error`**, passing `poisonDir` directly.
  The assertion (`apitls.ensureCertDir` rejects non-0700 with a
  `loginjector.PublicDetailsError`; error text contains the wrap) is preserved verbatim.
  **What is genuinely lost:** the current subtest proves *whole-startup abort*
  (fail-not-fallback end to end); afterwards it proves only that the cert-load step
  errors. The propagation `parseFlags` → `main` → `os.Exit(1)` becomes untested, because
  `parseFlags` calls `flag.Parse()` and therefore cannot be called from an in-process
  test (this is backlog **D5**'s landmine, and it is why `parseFlags` is untested today
  too). Accepted: `main`'s error branch is 3 lines of `fmt.Fprintln` + `os.Exit(1)`,
  identical to the two branches beside it. Do **not** try to restore end-to-end coverage
  by having `parseFlags` skip `flag.Parse` under a test flag — that is a production-code
  test hook, exactly what Task 1 just deleted.

  *Mechanically rewritten — the other 8 `TestRun` subtests:* each now builds its own
  inputs before calling `run`:
  ```go
  cfg, err := config.Load(cfgPath)
  require.NoError(t, err)
  opLog := newOperationalLogger(cfg.Operational)
  cert, err := loadAPICert(certDir, "localhost", "127.0.0.1", opLog)
  require.NoError(t, err)
  err = run(shutdownCtx, cfg, cert, opLog, smokeBuilder, smokeBuilder)
  ```
  The HTTP-mode subtest (`:512`) passes `certDir == ""` → `cert == nil`, unchanged in
  meaning. The two `captureStdout` subtests (`:668`, `:706`) still work **only because
  `newOperationalLogger` is shared** — the notifier log lines they assert on stay inside
  `run`, but they must be emitted through the same scrub-wrapped stdout handler. If the
  test built a bare `slog` logger instead, `assert.Contains(t, stdout, …)` would break.
  Call this out in the implementation.

- **Acceptance Criteria.**
  - `grep -rn 'tlsOptions\|parseTLSOptions' .` returns nothing.
  - `parseFlags` (or `bootstrap`) returns `(config.Config, *tls.Certificate, *slog.Logger, error)`
    — **pointer, per D-C** — and is referenced only from `main`.
  - HTTP mode is signalled by `cert == nil` and nothing else: `grep -rn 'len(cert.Certificate)\|len(.*\.Certificate) == 0' cmd/ internal/`
    returns nothing, and `router.Options.Cert` is unchanged.
  - `config.Config` has a documented `Dir` field, set by `config.Load`, absolute, and
    `run` derives no directory from a path parameter.
  - `resolveCertDir("")` returns `("", nil)` — regression guard for `filepath.Abs("")`.
  - `loadAPICert` returns `(nil, nil)` exactly when `certDir == ""`, and emits the
    existing `"API running WITHOUT TLS …"` warning in that branch and the
    `"API TLS enabled"` info line (with `cert_dir` + `sha256_fingerprint`) otherwise.
  - All 8 former `TestParseTLSOptions` subtests exist under their new homes; the
    `poisonDir` case exists under `TestLoadAPICert`.
  - Repo-wide subtest count (`go test ./... -v | grep -c '=== RUN'`) is **greater than or
    equal to** the pre-task baseline.
  - `make test` green.
- **Pitfalls & edge cases.**
  - `newOperationalLogger` must be used by both `parseFlags` and every test that asserts
    on stdout, or the two `captureStdout` subtests silently stop matching.
  - `config.Config.Dir` must not become a JSON field — it is derived. Confirm the `raw*`
    unmarshal structs are untouched.
  - `config.Load` must produce an **absolute** `Dir`; `run`'s current
    `filepath.Abs(rawConfigPath)` (`main.go:166`) is what makes every operator-facing
    error cwd-independent. Preserve that.
  - `apitls.LoadOrGenerate` **writes** to the cert dir. Front-loading it into `parseFlags`
    means a fresh production host now creates `/opt/vpntunnel/configs/tls/` contents
    before the access log is opened. Verify the 0700 ownership check still runs first
    (`apitls.ensureCertDir`) — it does; no permission-model change.
  - `docs`/`CLAUDE.md` mention neither `tlsOptions` nor `parseTLSOptions`; no doc edit
    needed here (the TLS **flags** are unchanged and stay documented as-is).
- **Complexity:** Hard. This is the highest-risk task in the plan.

---

### Task 3 (review T003): Delete `internal/egress`; contracts become consumer-local

- **Description.** Delete `internal/egress/egress.go` and the package. Redeclare each of
  the four ports where it is consumed, unexported wherever the compiler allows, and
  replace `Close() error` with an embedded `io.Closer`.

  **This reverses backlog decision D3 and the caveat in auditor rule R2**, both of which
  mandate the opposite. That reversal is the owner's, is deliberate, and is recorded in
  Task 9 so a later audit does not undo it.

  Per finding **F1**, the ports go as follows.

  **`internal/application/tunnelpool` — exported** (forced: `DeviceBuilderFn` and
  `BuilderFn` are exported func types returning the port, and it is the producer side of
  the one cross-package signature seam):
  - `build.go` — `Dialer`, `DialerCloser` (used by `BuilderFn:19`, `DefaultBuilder:24`,
    `BuildDialer:53`).
  - `scheduler.go` — `Resolver`, `HealthReporter` (used at `:147`, `:156`, `:225–226`,
    `:248`, `:255`, `:260`, `:397–398`, `:549–550`; and in `supervisor.go` `:216`, `:218`,
    `:546`, `:563`, `:571`, `:606`, `:638`).

  ```go
  // DialerCloser is a Dialer that owns resources and must be torn down on
  // shutdown. Close must be safe to call more than once.
  type DialerCloser interface {
      Dialer
      io.Closer
  }
  ```

  Declare them in the public-interfaces position of each file, per Task 8's template — do
  **not** create a `ports.go`; that would just be `internal/egress` under a new name.

  **Consumer-local, unexported** (all safe — interface-to-interface and
  concrete-to-interface assignment is structural):
  - `internal/application/proxy.go` — `type dialer interface { DialContext(...) }`;
    `ProxyServiceOptions.Dialer` becomes `dialer` (`:74`), field `dialer` at `:99`. This
    is the owner's own worked example, verbatim. `main` assigns the concrete
    `*tunnelpool.StreamingSupervisor` → fine.
  - `internal/gateway/httpV1/handlers` — local `dialer` + `resolver` for
    `Forwarder.Forward` (`proxy.go:85`), `RawForwarder.ForwardRaw`
    (`zone_forwarder.go:51`), `tunnelForwarder.Forward` (`forwarder.go:53–54`),
    `ForwardRaw` (`:103–104`), `transportFor` (`:158`), `denyAwareDial` (`:184`). All
    intra-package implementations, so no identity problem.
    **Exception — `Router.Route` (`zone_forwarder.go:44`) must name
    `tunnelpool.Dialer` / `tunnelpool.Resolver`.** `handlers` gains an import of
    `vpntunnel/internal/application/tunnelpool`. Direction is legal (gateway → application)
    and the edge already exists — `zone_forwarder.go` imports
    `internal/application/asyncjob` today. No cycle: `tunnelpool` imports no gateway
    package.
  - `internal/infrastructure/notify` — local `dialer` in `notify.go` for `Event.Dialer`
    (`:41`) and `probe.go`'s `probeExitIP` (`:45`). *(Task 6 then moves `Event` out; the
    local `dialer` stays for `probeExitIP`.)*
  - `internal/infrastructure/wireguard` — no production interface needed. `dialer.go` and
    `health.go` only *mention* `egress.*` in doc comments; update the prose to name the
    ports without the dead package. The four assertions at `health_test.go:90–93` become
    **test-local** interface declarations.
    *Honest cost:* a test-local copy no longer fails if `tunnelpool.Dialer` changes. The
    real compile-time guard is `tunnelpool.DefaultBuilder`/`DefaultDeviceBuilder`
    returning `*wireguard.WireGuardDialer` as `DialerCloser`, which does still break.
    Do not "fix" this by importing `tunnelpool` from a `wireguard` test — that inverts
    the layering even in test scope.

  **Test files** (17 of them import `internal/egress`). Rule: a double that feeds a
  `tunnelpool.DeviceBuilderFn`/`BuilderFn` or implements `handlers.Router` uses the
  exported `tunnelpool.*` names; every other double gets a test-local contract at the top
  of its file, per Task 8's test template.
  - `tunnelpool.*` names: `cmd/vpntunnel/main_test.go` (`:215–217`, `:240`),
    `cmd/vpntunnel/rotate_test.go` (`:23`, `:43`, `:151`, `:182`),
    `tunnelpool/build_test.go`, `tunnelpool/integration_test.go`,
    `tunnelpool/scheduler_test.go`, `tunnelpool/supervisor_test.go`,
    `handlers/proxy_test.go:53`, `handlers/zone_forwarder_test.go:36`,
    `router/integration_test.go:71`, `router/server_test.go:71`.
  - Test-local contracts: `application/proxy_test.go` (`:31`, `:91`),
    `handlers/forwarder_test.go` (`:85`, `:97`, `:107`, `:124`),
    `httpserver/server_test.go:24`, `notify/probe_test.go:20`,
    `wireguard/health_test.go` (`:90–93`).

- **Acceptance Criteria.**
  - `internal/egress/` does not exist; `grep -rn 'internal/egress' .` returns nothing
    (including comments and CLAUDE.md).
  - `DialerCloser` embeds `io.Closer`; no port declares a bare `Close() error`.
  - `tunnelpool` exports exactly four port names and no `ports.go`/`contracts.go` file
    exists in it.
  - `application`, `notify`, `handlers` (except `Router`), and every test double name
    unexported/local contracts.
  - `handlers` imports `tunnelpool`; `tunnelpool` imports no `gateway/...` package
    (`go list -deps` check, or `grep -rn 'vpntunnel/internal/gateway' internal/application/`
    returns nothing).
  - The "safe to call more than once" and "not a sub-interface of Dialer" doc contracts
    from `egress.go:38–39` and `:59–64` are carried to their new homes, not dropped.
  - `make test` green; `make lint` green.
- **Pitfalls & edge cases.**
  - **The one guaranteed compile break** is `handlers.Router.Route` vs
    `tunnelpool.OnDemandScheduler.Route`. If the implementer reaches for local unexported
    types on both sides, the build fails with the exact error in **F1**. Do not respond by
    reintroducing a shared package — use `tunnelpool.Dialer`/`Resolver` in `Router`.
  - `HealthReporter` is consumed via type-assertion (`scheduler.go:398`,
    `supervisor.go:563`). A type assertion to an *unexported* interface from another
    package is impossible; keeping it exported in `tunnelpool` (where both assertions
    live) resolves it.
  - `notify.Event.Dialer` is assigned from `tunnelpool.Dialer` values
    (`supervisor.go:639`, `scheduler.go:531`). Structural assignment — verified — but the
    method sets must stay identical; a stray extra method on either copy breaks it.
  - Blast radius: 24 files import the package (7 production, 17 test). Land it as **one**
    commit — a partial state does not compile.
  - `io.Closer` adds an `io` import to `tunnelpool/build.go`.
- **Complexity:** Hard.

---

### Task 4 (review T007): Fold `extractIdentity` into `NewTelegram`; delete `dsn.go`

- **Description.** Move `extractIdentity` (`internal/infrastructure/notify/dsn.go:27`) and
  the `tokenPattern` regexp (`:13`) into `internal/infrastructure/notify/telegram.go`, and
  delete `dsn.go`. The owner's reason is file count, and it holds: `dsn.go` is 39 lines
  with exactly one caller, `NewTelegram` (`telegram.go:28`), 12 lines away in the same
  package.

  Two shapes are possible. **Recommended: keep `extractIdentity` as an unexported
  function in `telegram.go`** (private-helpers position per Task 8's template), rather
  than inlining its body into `NewTelegram`. `NewTelegram`'s doc comment already
  references it by name, its validation is 12 lines with two distinct failure modes, and
  the 7 existing subtests target it directly. Inlining the body would force all 7
  assertions to route through `NewTelegram`, which starts a goroutine and constructs a
  `bot.Bot` — turning 7 pure unit tests into 7 tests with lifecycle. That trades the
  owner's stated goal (fewer files, cleaner implementation) for worse tests. The file
  disappears either way, which is what was asked.

  `dsn_test.go` (92 lines, `TestExtractIdentity`, 7 subtests) moves into
  `telegram_test.go` as `TestExtractIdentity`, keeping the local `parseDS` helper and all
  7 subtests byte-for-byte. Per Task 8's template it sits in production declaration order
  — after `TestNewTelegram`, since `extractIdentity` will be an unexported helper.

- **Why this can run in parallel with Task 3.** Task 3 touches `notify/notify.go` and
  `notify/probe.go`; Task 4 touches `notify/dsn.go`, `notify/telegram.go`,
  `notify/dsn_test.go`, `notify/telegram_test.go`. Disjoint file sets within the same
  package. Both must be green before Task 6.

- **Acceptance Criteria.**
  - `internal/infrastructure/notify/dsn.go` and `dsn_test.go` do not exist.
  - `extractIdentity` and `tokenPattern` live in `telegram.go`; `tokenPattern` is in the
    private-var block.
  - `TestExtractIdentity` exists in `telegram_test.go` with all 7 subtests, unchanged in
    assertion content.
  - **R15 preserved (P0):** the two secret-safety subtests
    (`malformed token shape returns error without leaking secret`,
    `error text never contains the secret`) still pass, and `extractIdentity`'s doc
    comment still records that DSN-string→`DataSource` parsing stays with the caller
    because `dsninjector.Parse` embeds its raw input (which *is* the token) in its error
    text.
  - `make test` green.
- **Pitfalls & edge cases.**
  - `dsn_test.go` is `package notify` (white-box); `telegram_test.go` must also be
    `package notify` for the merge to compile. Verify before moving — if it is
    `package notify_test`, the tests go into a white-box file in the same package
    instead, and `notify_test.go` (which *is* `package notify_test`) is not the target.
  - Do not touch `message.go` — `formatMessage`/`formatDetails` are genuine Telegram HTML
    concerns and are out of scope for both T007 and T008.
  - Deleting `dsn.go` removes the `regexp` and `strconv` imports from that file; they must
    appear in `telegram.go` instead.
- **Complexity:** Easy.

---

### Task 5 (review T005): Move `internal/constants/constants.go` → `internal/constants.go`

- **Description.** Move the file up one level so it becomes `package internal` at import
  path `vpntunnel/internal`, and delete the `internal/constants/` directory.

  **It works** — verified in a scratch module (finding **F2**): `go build` and `go vet` are
  both clean, and a subpackage imports it as `vpntunnel/internal`. The internal-visibility
  rule only requires the importer to be rooted at `vpntunnel/`. Preconditions confirmed:
  no `.go` file currently sits directly under `internal/`, so no other file has to change
  package.

  Call-site change (2 sites only — `cmd/vpntunnel/main.go:48,249` and
  `cmd/vpntunnel/main_test.go:36,719`):

  ```go
  import "vpntunnel/internal"           // was "vpntunnel/internal/constants"
  ...
  if dsn := os.Getenv(internal.EnvTelegramBotDSN); dsn != "" {
  ```

  The package doc moves too, retitled `// Package internal holds cross-cutting constant
  values shared across the vpntunnel binary…`.

  *Honest note, not a blocker:* `internal.EnvTelegramBotDSN` reads worse than
  `constants.EnvTelegramBotDSN` — `internal` names a visibility scope, not a concept, so
  the qualifier carries no information (a mild R13 tension). The owner judged the layout
  "more correct"; with 2 call sites the readability cost is negligible. Flagging it only
  so it is a known trade, and because Task 7 multiplies the call sites.

- **Acceptance Criteria.**
  - `internal/constants/` does not exist; `internal/constants.go` exists with
    `package internal`.
  - `grep -rn 'internal/constants' .` returns nothing.
  - `EnvTelegramBotDSN`'s doc comment still states that only the NAME is safe to log and
    the VALUE embeds the bot token.
  - `CGO_ENABLED=0 go vet ./...` clean (specifically: no complaint about the package name).
  - `make test` green.
- **Pitfalls & edge cases.**
  - Once `internal/constants.go` is `package internal`, **every** future `.go` file placed
    directly under `internal/` must also be `package internal`. Task 7 relies on this;
    note it in the file's doc comment so it is not discovered the hard way.
  - `t.Setenv(constants.EnvTelegramBotDSN, …)` at `main_test.go:719` must be updated with
    the rest.
  - Do **not** add an `internal/constants` alias shim "for compatibility" — there is no
    external consumer; the module is not importable from outside.
- **Complexity:** Easy.

---

### Task 6 (review T008): Move the tunnel-change `Event` into `domain`

- **Description.** The review points at `internal/infrastructure/notify/notify.go:27` and
  calls it "our internal representation of the message/event". **The type there is
  `Event`, not `Message`** — there is no `notify.Message` anywhere in the repo
  (finding **F3**). Move `Event` and the `Source` enum it depends on
  (`notify.go:18–24`, the two `iota` constants) into `internal/domain/`.

  Recommended shape — a new `internal/domain/tunnelevent.go`:

  ```go
  package domain

  // TunnelChangeEvent describes one tunnel-change occurrence a notifier may report.
  type TunnelChangeEvent struct {
      Source   TunnelChangeSource
      Title    string
      Country  string
      Filename string
      Dialer   dialer   // may be nil; the exit-IP probe is skipped when nil
  }

  // dialer is the minimal outbound-connection contract the exit-IP probe needs.
  type dialer interface {
      DialContext(ctx context.Context, network, address string) (net.Conn, error)
  }
  ```

  Rename on move — `domain.Event` would be a meaninglessly generic name in a package that
  will accumulate other types, and `TunnelChangeEvent` is what the existing doc comments
  already call it. Renaming a type as part of moving it is not the identifier churn Task 8
  forbids.

  `Notifier`, `Nop`, and `TelegramNotifier` stay in `notify` — the owner asked only about
  the event. `Notifier.Notify(ctx, ev domain.TunnelChangeEvent)`.

  **Verified: no import cycle.** `domain` imports nothing outside stdlib; `notify` → `domain`
  is a new edge; `tunnelpool` already imports both. **No Telegram concern is dragged in:**
  `formatMessage`/`formatDetails` (`message.go`), `exitInfo`/`probeExitIP` (`probe.go`),
  and the dedup/rate-limit state all stay in `notify`.

  **Two honest costs, for the record.** `internal/domain/domain.go:1–5` currently declares
  the package charter: *"no I/O, no business logic, and no imports beyond the standard
  library."* Adding a `dialer` interface puts a **behavioural port** into a
  pure-value-types package and hands `domain` a live network handle; and `Title` is a
  free-form human label shaped for a chat message (`"switched tunnel"`,
  `"on-demand: se"`), while `Source` exists solely so the *notifier* can decide whether to
  rate-limit. Both are notification-transport concerns, not domain invariants. The move is
  mechanically clean but it does amend the charter. Update the package doc to say so
  explicitly rather than leaving a comment that the code contradicts. If the owner would
  rather not amend it, the alternative is to leave `Event` in `notify` and instead tighten
  its fields to domain value types (`Country domain.Country`) — but that does not satisfy
  the review, so the default here is to execute the move.

- **Why this runs after Task 3.** `Event.Dialer` is `egress.Dialer` today. Task 3 turns it
  into a `notify`-local `dialer`; Task 6 then moves the field to a `domain`-local `dialer`
  and leaves `notify`'s copy in place for `probeExitIP`. Running Task 6 first would mean
  declaring the `domain` port against a package Task 3 is about to delete.

- **Call sites to update** (7 production, 4 test):
  `notify/notify.go` (delete `Event`/`Source`/consts, keep `Notifier`/`Nop`),
  `notify/telegram.go:78` (`Notify(_ context.Context, ev Event)`) and its internal uses of
  `ev.Source`/`SourceOnDemand`, `notify/notify_test.go:17`,
  `tunnelpool/supervisor.go:639–640`, `tunnelpool/scheduler.go:531–532`,
  `tunnelpool/supervisor_test.go` (`:188`, `:191`, `:197`, `:200`, `:1089`, `:1103`),
  `tunnelpool/scheduler_test.go:964`, plus the prose references at
  `supervisor.go:102`, `scheduler.go:52`, `supervisor_test.go:183,1063–1064`,
  `scheduler_test.go:928–930`.

- **Acceptance Criteria.**
  - `internal/domain/` declares `TunnelChangeEvent` + `TunnelChangeSource` + the two
    source constants; `notify` declares none of them.
  - `notify` retains `Notifier`, `Nop`, `TelegramNotifier`, `formatMessage`,
    `formatDetails`, `exitInfo`, `probeExitIP`, and the dedup/rate-limit state.
  - `internal/domain` imports only stdlib (`go list -f '{{.Imports}}' ./internal/domain`
    shows no non-stdlib path).
  - `internal/domain/domain.go`'s package doc is amended to acknowledge the one
    behavioural port, or the reviewer rejects the task.
  - `TestNop_Notify`, `TestStreamingSupervisor_Notifier`, and
    `TestOnDemandScheduler_Notifier` still pass with their existing assertions
    (`SourceStreaming`/`SourceOnDemand` equality checks).
  - `make test` green.
- **Pitfalls & edge cases.**
  - `notify` keeps its own `dialer` for `probeExitIP`; the `domain` one is separate.
    Assigning a `tunnelpool.Dialer` into `domain.TunnelChangeEvent.Dialer` and then
    passing it to `probeExitIP(ctx, d notify-local dialer, …)` is two structural
    assignments in a row — legal, but both method sets must stay identical.
  - Follow-up, explicitly **not** in scope: by Task 3's own doctrine, `notify.Notifier`
    should be a consumer-defined interface in `tunnelpool`, not exported from `notify`.
    T003 scopes itself to `egress.go`. Note it as a candidate for the next plan; do not
    do it here.
  - Do not move `TunnelHealth` (`domain/tunnel.go:12`) or touch the `TODO(T08c)` at
    `domain/tunnel.go:30` — separate concerns.
- **Complexity:** Medium.

---

### Task 7 (review T006): Move app-policy constants out of the implementation packages

- **Description.** The review names two sites: `internal/infrastructure/config/config.go:23`
  (the `Default*` const block) and `internal/infrastructure/ipdeny/ipdeny.go:35`
  (`defaultDeny`), with the reasoning that these are *application* constants, not
  *implementation* constants. Move both into `package internal` (created by Task 5), as
  separate files as the review suggests.

  **7a — config defaults.** Move the whole `Default*` const block (~20 constants,
  `config.go:23`–~`:80`) to a new `internal/defaults.go` (`package internal`). `config`
  imports `vpntunnel/internal` and its `applyDefaults` path reads `internal.DefaultListen`
  etc. The owner's reasoning is sound here: `DefaultAPIListen = "127.0.0.1:8888"` is an
  application policy decision, while `config`'s job is parse-and-validate.
  **Cost: 35 assertion sites in `internal/infrastructure/config/config_test.go`** (lines
  59–69, 102, 449–462, 486, 502, 702–706, 989, 1353) switch from `config.DefaultX` to
  `internal.DefaultX`. Purely mechanical, but it is the bulk of the task's diff.

  **7b — the SSRF deny-list. Ruled (decision D-B): bounded version only.** Move the CIDR
  **literal set** into `package internal`; keep `ipdeny.DefaultDeny()` and
  `ipdeny.Contains()` as the security façade. **Backlog D1 stands otherwise — the `ipdeny`
  package is not dissolved, renamed, or merged into `config`.** Concretely, a new
  `internal/ipdeny.go`:

  ```go
  // package internal
  // DenyCIDRs is the immutable list of ranges the v1 API refuses to dial through
  // any tunnel. Deliberately non-configurable: widening it is forbidden,
  // tightening requires a code change and review.
  var DenyCIDRs = []netip.Prefix{ /* the 8 existing prefixes, comments intact */ }
  ```

  `ipdeny.DefaultDeny()` becomes `return internal.DenyCIDRs`, and `ipdeny.Contains` is
  untouched. This satisfies the owner's principle ("policy values do not belong in the
  implementation") while keeping the security façade, its "MUST NOT be modified / MUST NOT
  be widened" doc contract, and **all 139 lines of `ipdeny_test.go` unchanged** — every
  assertion already goes through `ipdeny.DefaultDeny()`. `handlers/forwarder.go:193,207`
  is likewise untouched.

  **Task 7 is scoped to exactly 7a + 7b and nothing else.** In particular: do not dissolve,
  rename, or relocate the `ipdeny` package; do not touch `ipdeny.Contains`; do not move any
  other constant into `package internal` on the grounds that it looks similar.

- **Why this runs after Task 5 and after Tasks 1–2.** It needs `package internal` to
  exist (Task 5), and it edits `main.go`/`main_test.go` imports, which Tasks 1 and 2 are
  rewriting. Serialize.

- **Acceptance Criteria.**
  - `internal/defaults.go` and `internal/ipdeny.go` exist, both `package internal`.
  - `grep -n 'Default' internal/infrastructure/config/config.go` shows no `Default*`
    *declaration* (references via `internal.` are expected).
  - `config_test.go` passes with all 35 default assertions intact, retargeted at
    `internal.Default*`.
  - `ipdeny_test.go` is **unchanged** (`git diff --stat` shows 0 lines) and passes.
  - The `internal/infrastructure/ipdeny` package still exists, still exports
    `DefaultDeny()` and `Contains()`, and `handlers/forwarder.go:193,207` are unchanged
    (D-B / backlog D1). `grep -rn 'internal.DenyCIDRs' --include='*.go' .` matches only
    `internal/ipdeny.go` and `internal/infrastructure/ipdeny/ipdeny.go`.
  - `ipdeny.DefaultDeny()`'s and `internal.DenyCIDRs`' doc comments both still carry the
    "deliberately non-configurable / MUST NOT be widened" contract, and the 8 per-prefix
    rationale comments survive the move.
  - `CLAUDE.md`'s config-schema section is updated: it currently says *"The full config
    shape (every key + default) is the `raw*` struct set in `internal/config/config.go`"*
    — both the path (actual: `internal/infrastructure/config/config.go`) and the
    "+ default" claim are now stale.
  - `make test` green.
- **Pitfalls & edge cases.**
  - `config` importing `vpntunnel/internal` is a new edge from an infrastructure package
    to the root shared package. Check it does not create a cycle — it cannot today
    (`internal` imports only stdlib), but `internal` must stay that way. Add it to the
    acceptance check: `go list -f '{{.Imports}}' ./internal` shows stdlib only.
  - `internal/defaults.go` needs `time` (durations) and `internal/ipdeny.go` needs
    `net/netip`. `package internal` therefore holds three unrelated concerns
    (env names, config defaults, deny CIDRs) — the beginnings of a grab-bag. Keep them in
    **separate files** as the review asks, and stop adding to it without a reason.
  - `DefaultAPIMaxRequestBodyBytes = int64(10 * 1024 * 1024)` is explicitly typed; keep
    the `int64` conversion or `config_test.go:455` fails on type mismatch.
  - `forwardRawMaxBody` (`handlers/forwarder.go:274`) is a *different* 10 MiB constant,
    private to `handlers`. It is not in the review's scope — leave it.
- **Complexity:** Medium.

---

### Task 8 (review T004): Whole-repo declaration-order and test-layout normalization

- **Description.** Apply the review's two templates to all **58 production `.go` files**
  and all **58 `_test.go` files**.

  **Scope is exactly four operations (decision D-A), and nothing else:**
  1. declaration/section ordering, production and test files;
  2. contract assertions (`var _ Iface = …`) relocated to the top of the test file;
  3. `t.Parallel()` added to every test and subtest that can *legally* take it;
  4. test helpers, test-only consts/vars, and test-only contracts moved to the bottom.

  **Nothing is renamed** — not a production identifier, not a test function, not a
  subtest. `t.Run` names are kept **verbatim**: `subtest_NNN` in the review's template is
  a placeholder in the same register as `Method1`/`ObjectN`/`PIUBLIC CONTACTS`, and the
  descriptive names the repo already has are what make `go test` output, `-run` filters,
  and CI triage usable.

  **No behaviour change, no changed import set.** Apart from added `t.Parallel()` lines
  and the one-line `// why` comments in §"Serial carve-outs" below, every byte in the diff
  is a line that already existed, moved.

  **Production order** (per file):
  1. `package` + doc comment, then imports.
  2. Exported `const` block, then exported `var` block.
  3. Per type, grouped: `New<Type>` constructor(s) → the type → exported methods →
     unexported methods. Repeat per type.
  4. Exported interfaces.
  5. Unexported `const` block, then unexported `var` block.
  6. Unexported interfaces.
  7. Unexported helper funcs.

  **Test order** (per file):
  1. `package` + imports.
  2. Contract assertions (`var _ Iface = (*impl)(nil)`), all at the top, above the first
     `func Test`.
  3. The file's existing `Test*` functions, **reordered to follow the production file's
     declaration order** — each `TestX` sits at the position of the symbol it exercises.
     Existing function names are kept as they are; where a test is named for a scenario
     rather than a symbol, place it next to the `TestX` for the symbol it drives.
  4. `t.Parallel()` on each `TestX` and on each `t.Run` subtest **that can legally take
     it** (see §"Serial carve-outs").
  5. Test-only contracts, then test-only `const`/`var`.
  6. Test helpers last.

  **Explicitly out of scope: test-function consolidation.** The review's template implies
  one `TestX` per production func/method, which would mean merging scenario-named tests
  (`TestOnDemandScheduler_switch`, `_bringupFailure`, `_idleTTL`, `_Notifier`;
  `TestStreamingSupervisor_reconnect`, `_backoff`, `_Notifier`;
  `TestSendClientNoProxy`; `TestTelegramNotifier_SendErrorNeverLogsToken`;
  `TestNewOnDemandScheduler_panics`; `TestNewStreamingSupervisor_panics`) into the `TestX`
  for their production symbol. That renames and removes top-level test identifiers, which
  D-A's "nothing else" excludes — and it was the only non-mechanical part of this task.
  **Do not do it here.** Reordering alone already puts each of these adjacent to its
  symbol's `TestX`, which captures most of the readability benefit at none of the risk.
  Record it as a candidate follow-up plan; it should be decided per-file with the owner,
  not swept.

  **Three gaps in the template that must be resolved before starting** (otherwise 58 files
  get 58 ad-hoc answers):
  - *Exported types with no constructor* (`observability.PathSanitizePattern:25`,
    `middleware.Role:24`, `rotation.RotationOutcome:11`, `rotation.RotationResult:32`,
    `domain.RequestSummary`, `domain.TunnelHealth`, all `dto` types) → own group in
    position 3, after the primary object's group, before exported interfaces.
  - *Unexported support types* (`observability.trackingWriter:155`,
    `tunnelpool.routeRequest`, `notify.notifyJob`) → immediately after the unexported
    `const`/`var` blocks, with the unexported interfaces (position 6).
  - *Within a group, preserve existing relative order — do not alphabetize.* The review's
    `Method1…MethodN` implies no alphabetical requirement, and alphabetizing would triple
    the diff for zero benefit. **Note the deviation:** the `stack-go:conventions` skill
    says "prefer alphabetical" for methods; the owner's template is more specific and
    wins. Record this in Task 9 so an audit does not flag it.

  **What is already compliant** (so the sweep is smaller than it looks): `t.Parallel()`
  discipline is near-universal — **145 of 147** top-level test funcs and **843 of 851**
  subtests already call it. So operation 3 is expected to add **almost no** `t.Parallel()`
  calls; the real work there is *documenting* the handful of legitimately-serial cases, not
  parallelizing them. Test naming is already largely `TestType_Method`, and D-A means none
  of it changes.

  **Serial carve-outs — how the engineer decides.** A test or subtest that cannot run in
  parallel **keeps its serial form and gains a one-line `// why` comment**; it is never
  force-parallelized to make a count go up. Apply this decision procedure, in order — the
  first match wins:

  1. **Does it call `t.Setenv` or `t.Chdir`?** → serial, non-negotiable. Go's `testing`
     package *panics* if either is combined with `t.Parallel()`. Known:
     `cmd/vpntunnel/main_test.go:719` (`t.Setenv(constants.EnvTelegramBotDSN, …)`).
  2. **Does it mutate or depend on a process-global?** → serial. Known: the two
     `captureStdout` subtests (`main_test.go:685`, `:725`) swap `os.Stdout` process-wide.
  3. **Does it take an exclusive OS resource — a fixed port, a fixed path, a lock file?**
     → serial. Known: all of `TestRun` (fixed ports 17788–17802 / 18888–18902) and most of
     `internal/gateway/router/integration_test.go`.
  4. **Does it share a mutable fixture with its siblings, or assert on state accumulated
     across subtests (ordering, counters, a shared store)?** → serial. Check
     `internal/application/asyncjob/pool_test.go` (2 of 3 parallel today) against this.
  5. **Otherwise** → add `t.Parallel()`.

  Most of these already carry a `// not t.Parallel() — binds to fixed ports …`-style
  comment (see `main_test.go:282`, `:379`, `:433`, `:469`, `:513`, `:563`, `:669`, `:707`).
  Where the comment exists, **verify it and leave it**; only write a new one where a serial
  test is currently undocumented. Comment form: `// not t.Parallel() — <reason>`.

  **Verification for operation 3:** after each shard, `go test -race -count=2 ./<pkg>/...`
  must be green. `-count=2` re-runs in the same process and is what surfaces a wrongly
  parallelized test that shares state. A green single run proves nothing here.

  **What actually needs work.** Sampled representative files:
  - `internal/application/proxy.go` — `Verifier` interface at `:92` sits between
    `ProxyServiceOptions` and `ProxyService` (must move to position 4);
    `classifyAuthFailure:459` interleaved among methods; `hopByHopHeaders:546` after the
    helpers (must precede them).
  - `internal/infrastructure/observability/access.go` — `PathSanitizePattern:25` precedes
    `NewAccessLogger:56`; `trackingWriter:155` needs the new rule.
  - `internal/gateway/middleware/authtokens.go` — `Role:24` + its const block and
    `TokenRoleCount:37` precede `LoadTokens:56`; the exported const belongs in the
    top block.
  - `internal/tools/rotation/rotation.go` — `NoopRotator:60` + its method sit *after* the
    `Rotator` interface at `:49`; types must precede exported interfaces.
  - `internal/gateway/httpV1/handlers/forwarder.go` — already compliant; expect several
    files to need zero changes.

  **Integration/harness files.** `internal/gateway/router/integration_test.go` (11 tests),
  `internal/application/tunnelpool/integration_test.go`, and
  `internal/gateway/httpV1/handlers/classify_test.go` test compositions, not symbols, so
  the "order tests to match the production file" rule has no production file to match
  against. Order them by their existing logical grouping instead, and apply the other three
  operations (assertions at top, `t.Parallel()` per the carve-out procedure, helpers last)
  normally.

  **Sharding.** Nine commits, each independently green:

  | Shard | Scope | ~LOC |
  |---|---|---|
  | 1 | `cmd/generatevpnconfig` | 4 458 |
  | 2 | `cmd/vpntunnel` + `internal/tools/*` | 2 433 |
  | 3 | `internal/application` + `internal/domain` + `internal/constants.go` | 2 308 |
  | 4 | `internal/application/asyncjob` | 3 305 |
  | 5 | `internal/application/tunnelpool` (build, clock, discover, eligible, verify) | ~2 400 |
  | 6 | `internal/application/tunnelpool` (scheduler, supervisor, integration) | ~3 750 |
  | 7 | `internal/gateway/httpV1/*` | 4 296 |
  | 8 | `internal/gateway/{httpserver,middleware,router,router/apitls}` | 4 421 |
  | 9 | `internal/infrastructure/*` (config, ipdeny, notify, observability, wireguard, wgconf) | 6 454 |

  Note shard 1: `cmd/generatevpnconfig` (22 top-level tests, 4 458 LOC) is the largest
  single unit, and it survives only because backlog T01 was cancelled (`4b6e543`). It is
  in scope — "the whole repo" includes it.

  **Per-shard checklist** — run all six for every shard, in order, before committing:

  1. **Production files** reordered to the 7-position template (including the three
     resolved gaps).
  2. **Test files** reordered: assertions at top → `Test*` funcs in production declaration
     order → test-only contracts/consts/vars → helpers last.
  3. **`t.Parallel()`** applied per the serial carve-out procedure; every serial test or
     subtest carries a `// not t.Parallel() — <reason>` line.
  4. **Movement gate** (below) is clean for every file in the shard.
  5. `gofmt -l .` empty · `CGO_ENABLED=0 go vet ./...` clean · `make lint` green.
  6. `go test -race -count=2 ./<shard packages>/...` green, then `make test` green.

  **Size estimate.** With D-A ruled as a placeholder, this task is ~40 000 lines of pure
  movement plus an expected **fewer than 10** added `t.Parallel()` calls and a similar
  number of new `// why` comments. Under the literal reading it would have carried ~851
  additional subtest renames; it does not. The dominant cost is diff volume and the
  discipline to keep it movement-only — not judgement calls.

- **Why this must run last, and it genuinely must.** The brief invited disagreement;
  there is none. Task 8 rewrites the declaration order of nearly every file in the repo.
  Task 3 alone edits 24 files' interface declarations, Task 2 adds and deletes four
  functions in `main.go`, Task 6 moves a type between packages, Task 7 relocates two
  const blocks. Any of those landing *after* Task 8 either re-breaks the order or
  conflicts textually with a diff that touches most lines of most files. Running Task 8
  last also makes its verification gate possible at all (below) — that gate depends on
  nothing else changing content in the same commit. **Additional instruction for Tasks
  1–7: write every new declaration already in template order**, so Task 8's diff shrinks
  and its gate has less to check.

- **Acceptance Criteria.**
  - **Movement-only gate, per file** (this is the load-bearing check). Because D-A removed
    the consolidation step, this gate is now **exact** — no enumerated exceptions:
    ```
    diff <(git show <base>:<file> | sed 's/^[[:space:]]*//' | grep -v '^$' | sort) \
         <(sed 's/^[[:space:]]*//' "<file>" | grep -v '^$' | sort)
    ```
    The **only** permitted `>` entries are lines equal to `t.Parallel()` and new
    `// not t.Parallel() — …` comments. There must be **zero `<` entries** — nothing may be
    removed. Any content edit, import change, renamed identifier, or deleted line fails the
    gate on sight. Leading-whitespace insensitivity covers re-indentation from moving a
    declaration between nesting levels.
  - **No test lost and none renamed:** repo-wide
    `go test ./... -v 2>&1 | grep -c '^=== RUN'` equals the baseline captured before
    shard 1 (equal, not merely ≥ — nothing is added or merged). Capture that baseline
    first. Additionally, the sorted set of `--- PASS:` names is **identical** before and
    after; this is the mechanical proof that D-A was honoured and no `t.Run` string moved.
  - **`t.Parallel()` coverage:** every top-level `Test*` and every `t.Run` subtest either
    calls `t.Parallel()` or carries a `// not t.Parallel() — <reason>` comment. No test is
    both.
  - `go test -race -count=2 ./...` green — the check that catches a wrongly parallelized
    test sharing state.
  - `gofmt -l .` is empty; `CGO_ENABLED=0 go vet ./...` clean; `make lint` green.
  - Every `var _ Iface = …` assertion sits above the first `func Test` in its file.
  - No production `.go` file contains a `var _ Iface = …` assertion (R1 — already true at
    HEAD; must stay true).
  - Zero identifiers renamed anywhere:
    `git diff <base> -- '*.go' | grep '^[-+]func '` shows the same set of signatures
    added and removed (position changes only), for test and production files alike.
  - `make test` green after each of the 9 shards.
- **Pitfalls & edge cases.**
  - **Do not rename anything, and resist the temptation.** A sweep across 116 files
    surfaces dozens of names that could be better. D-A says no. The `--- PASS:` name-set
    equality criterion is there to catch it mechanically, including in files nobody
    re-reads.
  - **Do not force-parallelize.** The 8 non-parallel subtests and 2 non-parallel test funcs
    are the *most likely* to be legitimately serial, not the most likely to be oversights —
    they bind fixed ports, call `t.Setenv`, or swap `os.Stdout`. Adding `t.Parallel()` to
    one of them buys a compliance tick and a flaky CI. Run the carve-out procedure; when in
    doubt, leave it serial and write the comment.
  - Build-tag files: `middleware/authtokens_unix.go` and `authtokens_nonunix.go` are
    ordered independently; do not merge them or move declarations across the tag boundary.
  - `internal/application/export_test.go` is a one-line white-box shim; leave it as its
    own file (backlog T06 is a separate open question).
  - Moving a `//nolint:` directive away from its target line silently disables the
    suppression *and* the check. `handlers`, `main_test.go:859,898`, and
    `main_test.go:893` carry `//nolint` comments — they must travel attached.
  - Moving a `var` whose initializer depends on another package-level `var` can change
    initialization order. Go resolves package-level initialization by dependency, not by
    source order, so this is safe — but `netip.MustParsePrefix`-style initializers panic
    at init on error, so a reordering that *looks* wrong is worth a second read.
  - A single shard that touches 4 000+ lines makes `git blame` useless for that range.
    Accepted cost; the commit message must say "pure reordering, no behaviour change" so
    future archaeology skips it. Consider adding the shard commits to a
    `.git-blame-ignore-revs` file.
  - Do not fold unrelated cleanups in. If a file needs a comment fix, that is a separate
    commit — the movement-only gate will fail otherwise, which is the gate working.
- **Complexity:** Medium–Hard. D-A removed the only judgement-heavy part (test-function
  consolidation) and the ~851-rename tail, leaving volume and diff discipline as the whole
  difficulty. Nine shards of mechanical movement behind an exact gate — tedious, low-risk
  per file, unforgiving of shortcuts.

---

### Task 9 (review header ask): Extend `code-standards-auditor` with the normalization rules

- **Description.** The header ask is: *"Add a global rule or skill that analyses my review
  comments and describes/prepares a linter or skill for auto-checking Go code and bringing
  it to a 'normalized' style."* Per the owner's global **Code standards extraction**
  instruction and the explicit scope decision, satisfy it by appending new `R#` rules to
  `~/.claude/agents/code-standards-auditor.md` (currently R1–R15, 305 lines) — **no new
  linter binary, no `make lint` extension, no new skill.** That agent is the owner's
  manual audit gate; keeping it current is the whole mechanism.

  Each rule follows the file's existing four-part form: **rule / why / detect / fix**.

  **New rules to append:**

  - **R16 — Canonical production file declaration order.** The 7-position order from
    Task 8, including the three resolved gaps (constructor-less exported types;
    unexported support types; preserve relative order, do not alphabetize). *Detect:*
    grep the ordered sequence of `^func |^type |^const |^var ` per file and check it
    against the template. *Fix:* move declarations only — never edit content in the same
    commit.
  - **R17 — Canonical test file layout.** Contract assertions at top → `Test*` funcs in
    production declaration order → `t.Parallel()` at both the top level and in every
    subtest → test-only contracts/consts/vars → helpers last. Two clauses that generalize
    beyond this repo and must be in the rule:
    - **Subtest names are descriptive, never ordinal** (per decision D-A). A `t.Run` name
      is read in failure output, in `-run` filters, and in CI triage; `subtest_001` is
      worse at all three and turns every insertion into a renumbering.
    - **The serial carve-out procedure** — `t.Setenv`/`t.Chdir` → serial (the testing
      package panics otherwise); process-global mutation → serial; exclusive OS resource
      (fixed port/path/lock) → serial; shared mutable fixture or cross-subtest accumulated
      state → serial; otherwise parallel. A serial test carries
      `// not t.Parallel() — <reason>`. Never force-parallelize to raise a count; verify
      with `go test -race -count=2`.
    Include the integration/harness-file exemption from the declaration-order clause.
    *Detect:* `grep -A2 't.Run(' | grep -c 't.Parallel()'` vs `grep -c 't.Run('`;
    assertions appearing below the first `func Test`.
  - **R18 — Composition roots take explicit parameters, not functional options, for
    test seams.** From T002: functional options existing solely to inject fakes are
    production code serving tests. *Fix:* promote them to ordinary parameters and let the
    entry point name the production defaults. Cross-references R9.
  - **R19 — Flag parsing returns finished objects, and its name says what it does.**
    From T001: an entry point should hand `main` ready-to-use values rather than a bag of
    raw flag strings; and if it also loads config, generates keys, or logs, it is not
    called `parseFlags`. Keep the pure, testable parts (`resolveCertDir`, `parseIPSANs`)
    factored out, because the `flag.Parse()`-calling function is untestable in-process
    (backlog D5). Cross-references R6, R13.
  - **R20 — Application policy values do not live in implementation packages.** From
    T005/T006: default values, env var names, and hardcoded policy lists belong in the
    shared root constants package, one file per concern; the implementation package reads
    them. Carries the bounded caveat from decision D-B: a security control keeps its
    named façade — move the *literal*, not the package. Supersedes/absorbs R4, which
    covers only env var names — **extend R4's wording to point at R20 rather than
    duplicating it.**

  **Amendment to R2 — APPROVED by the owner.** The edit is authorized and **strictly
  bounded**: rewrite **only** the clause that mandates a dedicated
  `internal/egress`-style contracts package. **The rest of R2 stands, and no other
  existing rule is touched** — not R1, not R9, not R14, and R4 gets only the one-line
  pointer to R20 described above.

  The clause in question is R2's caveat (`code-standards-auditor.md:70–79`) and its *Fix*
  bullet (`:87–89`), which currently say: *"Keep such a port as one named contract in a
  dedicated `internal/port` (or `internal/egress`) package — not scattered."* That is
  precisely what T003 deletes, so left as-is the next audit reports T003's work as an R2
  violation and reverses it. Rewrite it to match the owner's reversal and the verified
  compile behaviour:
  - The default is fully consumer-local, unexported contracts. Interface-to-interface and
    concrete-to-interface assignment is structural, so this works for almost every port.
  - The **one** case that does not compile is a port appearing in the *signature* of a
    cross-package interface method (the definer and implementer must name an identical
    type). The fix there is to **export the port from the package that produces/owns it**
    and have the consumer name that — **not** to create a contracts-only package.
    A dedicated `internal/egress`/`internal/port` package is no longer acceptable.
  - Include the reproduced compiler error from finding **F1** in the *Detect* section, so
    the reasoning is not lost.
  Keep R2's opening rule (interfaces local, minimal, consumer-defined), its *Why*, and its
  "a consumer that only reads must not demand `Close`/`Seek`/`Open`" line unchanged — the
  reversal is about the carve-out's *remedy*, not about the rule.

  Also record the documented deviation from `stack-go:conventions` ("prefer alphabetical"
  for methods) inside R16, so the two sources of truth do not contradict each other.

- **Why this can run first, in parallel with everything.** It touches no repo file, so it
  cannot conflict. Doing it early gives Tasks 1–8 a written template to code against.
  Re-read it at the end: if implementation surfaced a rule the plan did not anticipate,
  append it then.

- **Acceptance Criteria.**
  - `~/.claude/agents/code-standards-auditor.md` contains R16–R20 in the existing
    rule / why / detect / fix form, in the appropriate themed section.
  - R2's caveat and *Fix* no longer recommend a dedicated egress/port package, and cite
    the cross-package-signature exception with the exported-by-producer resolution.
  - **Bounded-edit proof:** `diff` of the file against its pre-task state shows changes in
    exactly three regions — R2's caveat + *Fix*, R4's one-line pointer to R20, and the
    appended R16–R20. Every other rule is byte-identical. No rule is deleted.
  - R17 carries both the descriptive-subtest-names clause and the serial carve-out
    procedure.
  - The T003 reversal of backlog D3 is stated explicitly in R2's text so it is not
    re-reversed.
  - No new file under `~/.claude/skills/`, no change to `Makefile`, no new lint binary.
  - The file is English, professional, profanity-free (it leaves the session).
- **Pitfalls & edge cases.**
  - This file is global (all projects). Only genuinely cross-project standards go in;
    anything true only of `vpntunnel` belongs in this repo's `CLAUDE.md` or the agent
    memory. R16–R20 are all cross-project; the D-A ruling on subtest naming and the serial
    carve-out procedure both generalize, so they belong in R17.
  - Do not encode `vpntunnel`-specific paths (`internal/egress`, `tunnelpool`) as rules;
    use them only as *examples* inside *detect*/*fix*.
- **Complexity:** Medium.

---

## Execution Order

```
Task 9  (auditor rules)                      ← no repo files; start first, revisit at the end
   │
Task 1  (T002: remove runOpt seam)           ← main.go + main_test.go
   │
Task 2  (T001: tlsOptions → *tls.Certificate) ← main.go + main_test.go + config.Config.Dir
   │
   ├── Task 3  (T003: delete internal/egress)  ── 24 files
   └── Task 4  (T007: fold extractIdentity)    ── notify/{dsn,telegram}{,_test}.go
   │      (‖ safe: disjoint files inside notify)
   │
Task 6  (T008: Event → domain)               ← needs Task 3's dialer shape
   │
Task 5  (T005: constants → internal/)
   │
Task 7  (T006: config defaults + deny CIDRs → package internal)   ← needs Task 5
   │
Task 8  (T004: whole-repo order sweep, 9 shards)   ← LAST, after everything settles
```

Serialization rationale, per the brief's asks:

- **Task 1 before Task 2.** Both rewrite `run`'s signature and its 9 `main_test.go` call
  sites, so one is second either way. Task 1 is pure parameter plumbing (small, safe);
  Task 2 is the config/TLS/logger inversion (large, risky). Landing the safe one first
  means the risky diff shows only its own change and the final signature is written once.
  See Task 1's "Why this goes before Task 2".
- **Tasks 3 and 4 in parallel.** Task 3 touches `notify/notify.go` + `notify/probe.go`;
  Task 4 touches `notify/dsn.go` + `notify/telegram.go` and their tests. Disjoint.
- **Task 6 after Task 3.** `Event.Dialer` must have its post-`egress` shape before it
  moves packages.
- **Task 5 before Task 7.** Task 7 needs `package internal` to exist.
- **Tasks 5 and 7 after Task 2.** Both edit `main.go`/`main_test.go` import blocks, which
  Tasks 1–2 are rewriting; the collision is in the same import block.
- **Task 8 last.** Argued in full in Task 8; the brief's instinct is correct and there is
  no counter-argument worth making.

Gate after every task: `make test` green. A red tree goes to `testdoctor` before the next
task starts. Review per the project working agreement: three parallel `reviewer` lenses on
first pass (A correctness & tests, B security & operations, C performance & architecture),
one scoped solo reviewer on the post-fix pass. Tasks 2, 3, and 8 warrant the full fan-out
individually; Tasks 1, 4, 5, 6, 7 can be reviewed in one batch.

## Backlog annotations needed

`plans/000-review-20260717-backlog.md` is **not edited by this plan**. After this plan
completes, annotate it as follows. (Items marked *state correction* are things the backlog
already achieved but does not record — see "State correction" above.)

| Backlog item | Annotation |
|---|---|
| **T14a** (collapse `run`/`runWithOpts`) | *state correction:* **DONE** at `998a26c`. **Superseded** by this plan's Task 1, which removes the `runOpt` seam T14a left behind. |
| **T14b** + **D5** (`parseFlags` from `main`, `tlsOptions` threaded) | *state correction:* **DONE** at `998a26c`. **Superseded** by Task 2: `tlsOptions` is deleted entirely and `parseFlags` returns finished objects. D5's core finding still stands and must be carried forward — `flag.Parse` in `init()` breaks `go test`, and a package-global TLS value breaks the per-test seam. Task 2 honours both: `parseFlags` stays `main`-only and the values stay threaded as parameters. |
| **T08b** + **D3** | **REVERSED by Task 3.** The four egress ports are no longer one shared contract in `internal/egress`; that package is deleted. D3's *compile-break reasoning was correct* (reproduced — see finding F1) but applies to exactly one seam (`handlers.Router.Route`), resolved by exporting the ports from the producer package `tunnelpool`. Mark D3 superseded, not wrong. |
| **S1** (contract assertions → tests) | *state correction:* **DONE** at `998a26c` (zero impl-side assertions repo-wide). Its assertion-relocation scope is **absorbed by Task 8**, which makes "assertions at the top of the test file" a repo-wide invariant with a mechanical gate, and by Task 3, which rewrites the specific assertions that named `egress.*`. |
| **S2** (import aliases) | *state correction:* **DONE** at `998a26c`. |
| **S3** (env names → constants) | *state correction:* **DONE** at `998a26c`. **Superseded** by Task 5: the constants file moves from `internal/constants/constants.go` to `internal/constants.go` (`package internal`). |
| **S4** (constructors take `dsninjector.DataSource`) | *state correction:* **DONE** at `998a26c`. Related follow-through in Task 4, which deletes the `dsn.go` file S4/T13a left with a single function in it. R15 redaction criteria carried forward and re-asserted. |
| **T13** / **T13a** / **T13b** | *state correction:* **DONE** at `998a26c`. **T13a superseded** by Task 4 (file deleted). **T13b** (`#VPNTUNNEL` injected from `main`) unaffected. |
| **T16** (notify → `go-telegram/bot`) | *state correction:* **DONE** at `998a26c` (`go-telegram/bot v1.22.0`). Its gate on T13a/S4 is discharged. |
| **T15** (rename `lazy`) | *state correction:* **DONE** — package is `internal/application/tunnelpool`. **D6**'s `asyncjob` half remains "no rename"; unaffected. |
| **T17** (observability → `loginjector`) | *state correction:* **DONE** at `998a26c`. Its gate on S1's observability assertions is discharged. |
| **T18** (`configs/` + `deploy/`) | *state correction:* **DONE** — no `deploy/` directory exists. |
| **T01** (`generatevpnconfig` → script) | *state correction:* **CANCELLED** (`4b6e543`, `plans/history/260721.0001.generatevpnconfig-to-script.md`). Consequence for this plan: the package stays in scope for Task 8 and is its largest shard (4 458 LOC). |
| **T12** + **D1** (`ipdeny`) | **Partially revisited by Task 7b.** D1's decision (keep `ipdeny` standalone) is **upheld and re-confirmed** by owner ruling D-B — the package, its façade (`DefaultDeny()`/`Contains()`), and its tests all survive untouched. Only the CIDR literal set relocates to `package internal`. Not pending anything. |
| **T06** (`export_test.go`) | Still **open** (`discuss`). Untouched by this plan; Task 8 leaves the file in place. |
| **T08c** leftover | Still **open**: `TODO(T08c)` at `internal/domain/tunnel.go:30` — `TunnelID` is not threaded through the catalog or the `{id}` handler. Needs its own plan. Task 6 adds types to `domain` but does not address this. |
| **R2 / R4** (auditor rules table, backlog lines 47–63) | The rules table's R2 row must be updated to match Task 9's **owner-approved** amendment (the dedicated-contracts-package remedy is withdrawn; the rest of R2 stands); R4 now points at the new R20. |

## Risks

1. **Task 2 is the real risk in this plan.** Moving `config.Load`, logger construction,
   and TLS cert load/generate into the flag entry point inverts the current order and
   changes what `run` can fail with. One existing subtest
   (`TestRun/https mode with unloadable cert dir fails startup`) must be re-targeted, and
   the end-to-end "startup aborts on unloadable cert" property is downgraded to
   "the cert-load step errors" — because `parseFlags` calls `flag.Parse()` and cannot be
   invoked in-process. This is stated as an accepted loss, not hidden. *Mitigation:* the
   lost assertion covers 3 lines of `fmt.Fprintln` + `os.Exit(1)` identical to the two
   branches beside it; the security-relevant part (0700 cert-dir enforcement, fail-not-
   fallback) stays fully asserted in `TestLoadAPICert`.
2. **`captureStdout` fragility.** Two `TestRun` subtests assert on process stdout. They
   keep working only if the tests build their logger through the shared
   `newOperationalLogger`. A hand-rolled `slog` logger in the test would make the
   assertions silently vacuous rather than fail loudly. *Mitigation:* explicit acceptance
   criterion in Task 2; reviewer lens A should check it specifically.
3. **Task 3 has exactly one guaranteed compile break, and a tempting wrong fix.** The
   `handlers.Router` ↔ `tunnelpool.OnDemandScheduler` seam will not compile with local
   unexported types on both sides (reproduced in F1). The tempting response — recreate a
   shared contracts package — undoes the whole task. *Mitigation:* the resolution is
   specified in the task, and the acceptance criteria forbid a `ports.go`/`contracts.go`.
4. **Task 3 weakens the `wireguard` contract assertions.** Test-local copies of a port do
   not fail when the real port changes. *Mitigation:* the real guard
   (`DefaultBuilder`/`DefaultDeviceBuilder` returning `*WireGuardDialer` as
   `DialerCloser`) still breaks at compile time; documented in the task so nobody
   "restores" the assertion by importing `tunnelpool` from an infrastructure test.
5. **Task 8's diff destroys `git blame` for most of the repo.** ~40 000 lines move.
   *Mitigation:* nine separate commits, each labelled pure-reordering; the movement-only
   gate proves no content changed; add the shard SHAs to `.git-blame-ignore-revs`.
6. **Task 8 can silently lose or rename a test.** Ruling D-A removed the consolidation
   step that was the main vector here, so the residual risk is a subtest dropped or a name
   "improved" during a large block move. *Mitigation:* the repo-wide `grep -c '^=== RUN'`
   count must be **equal** to the pre-shard-1 baseline (not merely ≥, since nothing is
   merged), and the sorted set of `--- PASS:` names must be identical before and after —
   which is also the mechanical proof that no `t.Run` string was touched.
6b. **Task 8 can introduce flakes by force-parallelizing.** Adding `t.Parallel()` to a test
   that binds a fixed port, calls `t.Setenv`, or swaps `os.Stdout` converts a compliance
   tick into intermittent CI failure — and the 10 non-parallel cases in this repo are
   exactly the ones most likely to be deliberate. *Mitigation:* the serial carve-out
   decision procedure in Task 8, plus `go test -race -count=2` per shard, which is what
   actually surfaces shared-state parallelism.
7. **Task 8 can silently disable a linter suppression.** Moving a declaration away from
   its `//nolint:` comment removes the suppression *and* the check, with no error.
   *Mitigation:* enumerated in the task's pitfalls; `make lint` after each shard.
8. **`package internal` is unusual enough to confuse tooling and future readers.** It
   compiles and vets clean (verified), but `internal.DefaultAPIListen` reads as a
   visibility qualifier, not a concept, and some third-party linters special-case the
   name. *Mitigation:* verified with `go build` + `go vet`; if a future linter objects,
   the fallback is a named package (`internal/appconst`) at a one-line-per-call-site cost.
9. **Task 7 grows a grab-bag package.** `package internal` ends up holding env var names,
   config defaults, and SSRF CIDRs — three unrelated concerns. *Mitigation:* separate
   files per concern (as the review asks) and an explicit "stop adding without a reason"
   note. This is the known cost of the owner's placement decision, not a defect in it.
10. **Task 6 amends `domain`'s charter.** A behavioural port and a chat-message `Title`
    land in a package documented as pure value types with no I/O. *Mitigation:* amend the
    package doc in the same commit so code and comment agree; the alternative (leave
    `Event` in `notify`) does not satisfy the review.
11. **No open decisions remain** — D-A, D-B, D-C and the R2 amendment are all ruled and
    propagated into the tasks. The residual risk is *drift*: an implementer reading the
    review's raw template instead of this plan will find `subtest_NNN`, the literal
    `tls.Certificate`, and an R2 caveat that all contradict the rulings. *Mitigation:* each
    ruling is restated inside the task that implements it, not only in the decisions
    section, and each names the deviation from the review's literal wording so a reviewer
    does not flag it as a mistake.
12. **Nothing here is deployable-behaviour-neutral by accident — it is by design, and
    that is worth verifying.** This whole plan is refactor-only, so the strongest
    end-to-end signal is that `TestRun`'s 9 boot/shutdown subtests and
    `router/integration_test.go`'s 11 integration tests stay green throughout. If either
    set needs an assertion *changed* (as opposed to relocated), that is a signal
    behaviour moved — stop and re-scope.

## Trade-offs

- **Egress ports: exported from `tunnelpool` (variant A) over unnamed interface literals
  (B) or an adapter closure (C).** Chose A because it is *forced anyway* —
  `tunnelpool.DeviceBuilderFn` and `BuilderFn` are exported func types returning the port,
  so out-of-package test builders must be able to name it. B compiles but puts a 3-line
  unnamed interface literal into 6 signatures; C compiles but adds the pass-through
  indirection R9 exists to forbid. A also matches the owner's own principle: `tunnelpool`
  builds, owns, and routes devices, so the port *is* where it is used. Cost: a new
  `handlers` → `tunnelpool` import edge (legal direction; the `asyncjob` edge already
  exists) and four exported names in `tunnelpool`, which a reviewer may read as
  "`internal/egress` renamed". The distinction that matters: the ports now live with a
  real consumer instead of in a package whose only purpose was to hold them.
- **`*tls.Certificate` over the review's literal `tls.Certificate`** (owner ruling D-C).
  Chose the pointer because `nil` already means HTTP mode and `router.Options.Cert` is
  already a pointer; a value type forces a `len(cert.Certificate) == 0` sentinel — a
  second, weaker spelling of "no TLS" — or a change to `router.Options`. The review said
  "approximately this signature", so this is within the ask. Cost: the signature is not
  literally what was written; noted in Task 2 and required in the commit body.
- **`parseFlags` also returns the logger, and should be renamed.** Chose to return it
  because TLS load needs a logger, the logger needs `cfg.Operational`, and `cfg` needs
  `config.Load` — so the only alternative is two loggers and a duplicated
  `"scrubbing active"` line with inconsistent scrubbing. Recommended renaming to
  `bootstrap()` because a function that reads JSON, generates an ed25519 key pair, writes
  a certificate, and logs is not a flag parser (R13). Cost: deviates from the owner's
  chosen name; left as his call.
- **Explicit parameters over functional options for the `run` seam.** Chose four ordinary
  parameters over keeping `runOpt`. Functional options earn their keep at a wide, growing,
  multi-caller API; `run` has one production caller and three options that exist only for
  tests. Cost: `run` takes six parameters, which looks heavy — acceptable at a composition
  root, where naming the production defaults explicitly is a feature.
- **Keep `extractIdentity` as a function inside `telegram.go` rather than inlining its
  body into `NewTelegram`.** Chose this because inlining would force all 7 existing
  subtests to route through `NewTelegram`, which starts a goroutine and builds a
  `bot.Bot` — trading 7 pure unit tests for 7 lifecycle tests. The owner's stated goals
  (fewer files, cleaner implementation) are both met by deleting `dsn.go`. Cost: the file
  count drops but the function count does not.
- **`ipdeny`: move the literal, keep the façade** (owner ruling D-B). Chose the bounded
  version over dissolving the package, because a package named `ipdeny` is a better home
  for an SSRF control than one named `internal`, backlog D1 already decided this on the
  merits, and the bounded version keeps all 139 lines of `ipdeny_test.go` and both
  `handlers/forwarder.go` call sites unchanged. Cost: the owner's "move it to /internal"
  is honoured for the values but not for the functions — accepted explicitly.
- **Descriptive subtest names over `subtest_NNN` ordinals** (owner ruling D-A). The
  template's ordinals are placeholders, consistent with `Method1`/`ObjectN`/`PIUBLIC` in
  the same snippet. 851 descriptive names are what make `go test` output, `-run` filters,
  and CI triage usable, and ordinals turn every subtest insertion into a renumbering.
  Second-order benefit: with no renaming, Task 8 becomes pure movement, which let its
  verification gate tighten from "movement plus enumerated exceptions" to "movement plus
  `t.Parallel()` lines, zero deletions". Cost: this is the one place the plan does not
  follow the template's literal text, and it is recorded as such so a reviewer does not
  read it as an oversight.
- **Test-function consolidation dropped, not deferred silently.** D-A's "nothing else"
  excludes merging scenario-named tests into their symbol's `TestX`. Chose to record it as
  a named follow-up rather than smuggle it in as "part of ordering", because it renames
  top-level identifiers and was the only judgement-driven step in an otherwise mechanical
  sweep. Cost: `TestOnDemandScheduler_switch` and its ~8 siblings keep names that describe
  a scenario rather than a symbol; reordering at least parks each beside the `TestX` it
  belongs to.
- **Preserve relative order within declaration groups; do not alphabetize.** Chose the
  owner's template (exported-then-unexported grouping) over the `stack-go:conventions`
  skill's "prefer alphabetical", because the owner's template is more specific and
  alphabetizing would multiply an already-enormous diff for no readability gain. Cost: a
  documented deviation between two style sources, recorded in R16 so an audit does not
  flag it.
- **Task 8 last, accepting rework.** Every earlier task writes code that Task 8 may
  reorder again. Chose it anyway because the converse — reordering first, then structurally
  editing 24+ files — guarantees textual conflict with a diff that touches most lines of
  most files, and destroys the movement-only gate. Mitigated by requiring Tasks 1–7 to
  write new declarations in template order. Cost: some declarations get placed twice.
- **One plan for eight review items.** Chose a single plan because the items share one
  concern (normalizing the tree left by the DDD restructure) and are heavily
  interdependent: T003 gates T008, T005 gates T006, T001/T002 collide on one file, and
  T004 gates on all of them. Eight separate plans would spend more words on cross-plan
  ordering than on the work. Cost: this plan is large; the 9-task/9-shard structure and
  per-task acceptance criteria are what keep it executable one session at a time.
