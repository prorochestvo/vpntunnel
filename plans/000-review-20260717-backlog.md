# Review 2026-07-17 — refactor backlog & wave plan

Source: `tmp/review_20260717.txt` (owner review of the DDD-pragmatic restructure).
Branch: `refactor/ddd-pragmatic-restructure`. Status: **backlog** — index of plans,
not itself an implementation plan. Individual `plans/NNN-slug.md` files are written
per wave, just before that wave starts (early structural moves reshape the ground
under the later cross-cutting sweeps, so writing all plans up front would guarantee
rework).

**Verified by a 7-lens reviewer gate** (extraction fidelity, dependency soundness,
rules-agent fidelity, DDD/architecture, Go conventions, security/ops, scope). All
seven returned APPROVE-WITH-CHANGES; their P0/P1 findings are folded in below, and
the handful that conflict with the owner's explicit asks are collected in
**§ Owner decisions**. Each wave's plan file still gets its own 7-lens pass before
implementation.

## The two bottlenecks (why "run everything in parallel" is a lie)

1. **`cmd/vpntunnel/main.go`** is the composition root, touched by eight items —
   verified at exact lines: S3 `os.Getenv` (:232), S4/T13 `NewTelegram` (:233), S2
   alias (:45), T09 tunnel-id-key wiring (:292), T10 `rotateAdapter` (:500), T14a
   `run`/`runWithOpts` (:171/:207), T14b flags (:139–147)/`parseTLSOptions` (:94).
   These serialize on one file.
2. **The egress port interfaces** (`internal/domain/dialer.go`): `Dialer`,
   `DialerCloser`, `Resolver`, `HealthReporter` are consumed by **5 production
   packages** (`infrastructure/wireguard`, `application`, `application/lazy`,
   `gateway/httpV1/handlers`, `infrastructure/notify`) plus ~15 test files. They
   also appear **inside other interfaces' signatures** across package boundaries
   (`handlers.Router.Route(...) (domain.Dialer, domain.Resolver, …)`,
   `Forwarder.Forward`, `RawForwarder.ForwardRaw`), so they cannot be scattered
   per-consumer (§ T08b). Reworking them is a structural move *and* a cross-cutting
   sweep, colliding with the contracts-into-tests sweep (S1).

Consequence: moves land first, cross-cutting sweeps run last over the settled tree,
design-only debates run alongside on their own track.

## Severity / type legend

- **Type:** `delete` · `move` · `sweep` · `reshape` · `discuss`.
- **Decision state:** `decided` · `discuss` · `owner-decision` (a reviewer verdict
  conflicts with the owner's explicit ask — see § Owner decisions).
- **Rule:** the cross-project standard this embodies, in
  `~/.claude/agents/code-standards-auditor.md` (R1–R15).

## Cross-project rules extracted → code-standards-auditor

| Rule | Standard | Tasks |
|------|----------|-------|
| R1 | contract assertions in tests, not impl; assert every implemented iface | S1 |
| R2 | interfaces local & minimal — **except shared cross-layer ports, kept as one contract** | T08b |
| R3 | domain value types over bare strings | T08a, T08c |
| R4 | env var names in one constants file, via constants only | S3 |
| R5 | constructors take `dsninjector.DataSource`, not a raw string to parse | S4, T13 |
| R6 | fail fast on required startup config | T14b |
| R7 | external identity/branding injected from `main` | T13 |
| R8 | prefer owner's / well-known libs over hand-rolled | T13, T16, T17 |
| R9 | no needless pass-through indirection | T14a |
| R10 | reusable logic → shared package, designed general | T09, T10 |
| R11 | in-test fixtures over `testdata/` unless testing file I/O | T04 |
| R12 | no redundant import aliases | S2 |
| R13 | names describe the concept, must not mislead | T15 |
| R14 | layering discipline; merge duplicated-concern packages | T07, T11, T12 |
| R15 | preserve secret redaction across refactors / lib swaps | T13a, S4, T16, T17 |

---

## Wave 1 — trash & isolated small wins (fully parallel)

File-disjoint; safe to run all seven concurrently.

**T01 · generatevpnconfig → shell script** · `move` · `decided` · R10-adjacent
Replace `cmd/generatevpnconfig/{main,main_test}.go` with `scripts/generate-vpn-config.sh`.
**Risk: medium (not low).** The cmd is ~31KB `main.go` + ~55KB test — not a thin
shell-out; it likely carries validation / atomic-write / idempotency the review's
"only shells out" premise understates. The T01 plan MUST enumerate the current cmd's
behavior (ref `plans/completed/260621.0001.generate-vpn-config.md`) and preserve
parity before deleting. Also update/remove the Makefile target that runs
`go run ./cmd/generatevpnconfig` (breaks `make generate-vpn-config` otherwise). Files:
delete `cmd/generatevpnconfig/`, add `scripts/generate-vpn-config.sh`, edit `Makefile`.

**T02 · remove `docs/v4-to-v5-migration.md`** · `delete` · `decided`
Server self-hosted and already migrated; the doc is spent. Delete (git retains it);
`tmp/` if a near-term reread is wanted. No inbound refs. Risk: none.

**T03 · remove stale `scripts/setup-host.sh`** · `delete` · `decided`
**Confirmed by diff:** it is *superseded* (not a byte-duplicate) by the canonical
`configs/provision-host.sh` — setup-host.sh predates the release-layout migration
(no `github_aide`→root transition, no `.conf` staging, no sudoers install, no
tunnel-id.key gen) and nothing but this backlog references it. `provision-host.sh`
is the `make init` path (Makefile:54–56, RUNBOOK:11, CLAUDE.md:200). The owner's
real doubt was script-vs-docs for a personal app; resolution: keep `provision-host.sh`
as the **one** surviving script (acceptable per `make init`), delete the stale one.

**T04 · inline `wireguard/testdata` into tests** · `reshape` · `decided` · R11
`internal/infrastructure/wireguard/testdata/uapi_canonical.txt` feeds a pure UAPI
parser, not a disk-I/O test. Inline as a const in `uapi_test.go`; delete `testdata/`.

**T05 · split `dto.go`** · `reshape` · `decided`
`internal/gateway/httpV1/dto/dto.go` → `health.go` + `rotate.go`, same package.

**T06 · justify or relocate `export_test.go`** · `discuss`
`internal/application/export_test.go` is a one-line white-box shim
(`var IsLoopbackRemote = isLoopbackRemote`) for the black-box `application_test`
package. Investigate its consumer; keep-as-is (legit Go pattern) vs fold in.

**T12 · `config` + `ipdeny`** · `move`/`reshape` · **owner-decision** · R14
Pulled into Wave 1: `config.go`/`ipdeny.go` import no `domain` and no other Wave-1
file touches them, so this is fully independent — and doing it before T07 opens
`httpV1/handlers/` dissolves a collision (ipdeny's sole prod consumer is
`handlers/forwarder.go:19,193,207`, which is inside T07's scope; both editing it
in parallel = merge loss). **Shape is an owner call (§ D1):** the DDD lens argues
`ipdeny` is a *hardcoded, deliberately non-configurable* SSRF deny-list and must NOT
be folded into the read-operator-JSON `config` package (invites a future "ipdeny
config field"). Default: keep `ipdeny` standalone (optionally rename to a security
name, e.g. `internal/tools/netguard`); update only `forwarder.go`'s import. If the
owner still wants the merge, it stays Wave 1, `forwarder.go` gets the single edit.

---

## Wave 2 — structural moves by subsystem (limited parallelism)

Ordering: **2a keystone (solo)** → **2b (T07, after 2a; T12 already done in Wave 1
so no `‖` collision)** → **2c (serialize on `main.go`)**.

**T08 · domain reshape (keystone)** · `reshape`+`sweep` · mixed · R2, R3
T08 **owns the removal/rewrite of every compile-time assertion — impl AND test —
that references a moved interface**, so Wave 2 stays green (`make test` is the wave
gate). Do not defer these to S1.
- **T08a** `country.go` → split: (a) a `domain.Country` value type with
  `ParseCountry(s) (Country, error)` that **normalizes to lowercase** (kills the
  `strings.ToLower` scattered at supervisor.go:532/534/640, eligible.go:180/194,
  scheduler.go:523); (b) the Mullvad-basename→`Country` extractor (today's
  `CountryFromID`) moves to the tunnel-naming logic (`application/lazy`
  discover/eligible), NOT onto the value type. `decided`.
- **T08b** relocate the four egress ports OUT of `domain` into a dedicated
  `internal/egress` (or `internal/port`) package — one shared contract each, NOT
  scattered per consumer (they sit in cross-package interface signatures; the
  implementer `application/lazy.OnDemandScheduler` can't be made to name a
  consumer-local type → won't compile). Localize only the genuine single-consumer
  *param* interfaces (`proxyHandler`, `asyncPool`, `tunnelPool` — already local).
  `HealthReporter` (used only via type-assertion inside `lazy`) MAY localize there.
  This honors "domain = types, no global contracts dump" without breaking the build.
  **owner-decision (§ D3)** — the owner said "drop the file, describe contracts
  locally"; that's right for param interfaces, wrong for shared ports.
- **T08c** merge `info.go` + `tunnelid.go` → `tunnel.go`; `type TunnelID string` as a
  **pure hex-validating value type**. NOTE this is a *rename*, not an add: `TunnelID`
  is currently a **function** (`HMAC-SHA256(key, basename)`) that needs key material
  + crypto — which violates domain's "stdlib only, no I/O" charter. Move the HMAC
  derivation to `internal/tools/hmackey` (with T09); rename all `domain.TunnelID(...)`
  call sites (incl. `hmackey_test.go:150`). Thread `TunnelID`/`Country` through the
  `tunnelID string` params (`Route`/`Forward`/`ForwardRaw`) or the types are cosmetic.
  Delete the dead `domain.TunnelInfo` (no non-test consumer). `decided`.
  Files: `internal/domain/*` + `internal/egress/*` (new) + all consumers. High blast
  radius, low per-site.

**T07 · gateway reshape** · `reshape` · mixed · R14 (2b)
- httpserver files → the gateway package proper. **Keep the two HTTP servers as
  distinct units** — `httpserver` (forward proxy, :7788, dispatches `ProxyService`)
  and `router` (API, :8888) are separate delivery contexts; do NOT fuse into one
  `http.go`.
- create `internal/gateway/middleware/` and group the **real** cross-cutting
  middleware there (version-agnostic): the API-server token auth
  (`gateway/router/authtokens.go`) + request-id. (NB: the forward-proxy `auth` pkg
  is NOT this — see T11.)
- **router split** (`discuss`, 10-lens): `internal/gateway/router` mixes httpV1 +
  middleware. Run the owner's 10-lens panel first; execute the chosen split here.
  Files: `internal/gateway/{httpserver,router,httpV1,middleware}/*`. Risk: high.

**T11 · auth → `internal/tools/bearerauth`** · `move` · **owner-decision** · R14 (2b, with T07)
The owner's instinct ("why is `auth` infrastructure?") is right — but it is NOT HTTP
middleware. `Verifier.Verify(header string) bool` is transport-agnostic (no
`net/http`) and its only consumer is the forward proxy (`application/proxy.go:416`).
Moving it into `gateway/middleware` would force `application → gateway`, an illegal
inward dependency. Correct home: a generic `internal/tools/bearerauth`, with the
`Verifier` port consumer-defined in `application`. The API-token + request-id
grouping (the actual middleware) stays in T07. **§ D2.**

**T09 · hmackey → reusable package** · `move` · `decided` · R10 (2c)
`cmd/vpntunnel/{hmackey,hmackey_test}.go` + the `domain.TunnelID` HMAC derivation
(from T08c) → `internal/tools/hmackey`, as a general key-gen + id-derivation helper.
Ends with a `main.go` wiring edit. **Honesty note:** `internal/tools/*` is importable
only within module `vpntunnel`, so "reuse in other projects" (the review's goal) is
NOT achievable there — the owner's own reusable libs (`dsninjector`, `loginjector`)
are standalone `github.com/prorochestvo/*` modules. **§ D4:** extract to a standalone
module for real cross-project reuse, or accept `internal/` for tidiness only (YAGNI,
one client) and drop the "universal" framing. Do not over-design an API for one caller.

**T10 · rotation → `internal/tools/rotation`** · `move` · `decided` · R10 (2c, after T07)
The `Rotator` interface + private impl → a dedicated package. `gateway/router/rotate.go`
must be **split**, not moved: it also holds the `*Server`-bound `handleRotate` HTTP
handler, which stays in the router. After T07 (both edit that file). Same `internal/`-vs-
standalone-module honesty note as T09 (§ D4). Preserve the gate closure
`svc.ActiveSessions() > 0` and `force` plumbing (the 2-device ceiling depends on it;
break-before-make lives in `lazy/supervisor.go`, untouched).

**T13 · notify: drop bespoke DSN + inject brand** · `move`+`reshape` · mixed · R5, R7, R8, R15 (2c)
- **T13a** delete `notify/dsn.go`; take settings via `dsninjector.DataSource`.
  **R15 acceptance criteria (P0 — do not lose):** (1) the DataSource parse error is
  NEVER logged/formatted raw — `dsninjector.Parse` embeds its input verbatim
  (`parser.go:99`) and the DSN *is* the token; replace with a generic message; (2)
  re-home `redactToken` (scrubs the token from URL-shaped `*url.Error` on send
  failure — a separate concern from parsing) + its regression tests to the surviving
  notify shape. Gated by T16. `decided`.
- **T13b** `#VPNTUNNEL` (message.go:22) is external identity → inject from `main`
  (non-secret; safe to log). `decided`.
  Files: `notify/*` + `main.go`. Overlaps S4.

---

## Wave 3 — cross-cutting sweeps (serial, over the settled tree)

Run **after** Wave 2. CRITICAL sweeps with real volume (S1, S4) get the owner's
5-reviewer single-lens pass; S2 (4 sites) and S3 (1 site) have nothing to panel and
are folded into the single main.go-pass review below.

**Main-file pass (T14 + S2 + S3 in one coordinated edit of `main.go`):**

**T14 · main.go cleanup** · `reshape` · mixed · R9, R6
- **T14a** collapse `run` (:171) into `runWithOpts` (:207); rename to `run`. Safe
  mechanical pass-through removal (variadic `opts ...runOpt` keeps `main`'s call
  valid). `decided`.
- **T14b** slim `main()`'s flag/TLS parsing into a `parseFlags() (…, error)` helper
  **called from `main()`, NOT from `init()`** — `flag.Parse()` in `init()` runs
  before `testing.Init()`, hits `-test.*`, and `os.Exit(2)`s every `package main`
  test. Fail-fast is already satisfied (`main` `os.Exit`s at :154/160/165 = R6).
  Keep `tlsOptions` **threaded**, not a package global — `main_test.go` passes
  divergent per-test values (real dir, `CertDir:""`, `poisonDir`) at 10+ sites; a
  global breaks those subtests + `t.Parallel()`. **owner-decision (§ D5)** — the
  owner asked for `init()` + a global; both are Go landmines here.

**S2 · remove redundant import aliases** · `sweep` · `decided` · R12 · (in main.go pass)
**Four sites, not two:** `main.go:45`, `cmd/vpntunnel/rotate.go:7`,
`cmd/vpntunnel/rotate_test.go:18`, `cmd/vpntunnel/main_test.go:35` — all
`lazy "…/lazy"` (`package lazy` == basename == alias → redundant). Note `rotate.go:7`
relocates under T10, so sweeping last hits the settled tree.

**S3 · env var names → `internal/constants.go`** · `sweep` · `decided` · R4 · (in main.go pass)
One in-code literal today: `os.Getenv("VPNTUNNEL_TELEGRAMBOT_DSN")` (:232). Hoist the
NAME to a constant; the getenv VALUE is still never logged. Honestly convention-only
at this volume — don't grow categorized constant blocks / doc ceremony for one literal;
R4/the auditor enforce growth.

**S4 · constructors → `dsninjector.DataSource`** · `sweep` · `decided` · R5, R8, R15 · 5-lens verify
`notify.NewTelegram(dsn string, …)` → `NewTelegram(ds dsninjector.DataSource, …)`;
`main` builds the `DataSource` from env and passes it. **Required (P1):** the Telegram
DSN is **optional** — a malformed value warns-and-disables, it never blocks startup
(CLAUDE.md); fail-hard applies only to genuinely-required DataSources, not this one.
Carry the R15 redaction criteria from T13a. Sweep for any other raw-settings-string
constructor. Depends on T13/T16 outcome.

**S1 · contract assertions → tests + fill gaps** · `sweep` · `decided` · R1 · 5-lens verify
Runs **last**. **Scope: the 15 NON-domain impl-side assertions** (T08b already owns
the 4 `domain.*` ones on `wireguard/dialer.go:30-33`): `slog.Handler`,
`io.WriteCloser`, `router.Rotator`, `asyncjob.{Store,Forwarder}`, `handlers.*`,
`notify.Notifier`×2, `cmd/rotate router.Rotator`, etc. Move each to a package
**test** file — but mind white-box vs black-box: 9 targets are unexported and need a
`package foo` (white-box) file, and 6 same-named test files are `package foo_test`
(black-box). Route those to an existing white-box file
(`router`→`authtokens_test.go`/`requestid_test.go`; `handlers`→`health_test.go`/
`live_health_test.go`; `asyncjob`→`gc_test.go`/`record_internal_test.go`;
`httpserver`→`starton_test.go`) and **create** `observability/*_test.go`
(`package observability`) — `scrubhandler.go:75` has no white-box file today.
Dedup: only `HealthReporter` (dialer.go:32) is already asserted in a test
(`health_test.go:90`) → delete the impl copy; the other three domain ports MOVE with
T08b. Gap-fill: `auth.Verifier` (`auth.go:19`) has a production impl but NO assertion
anywhere — add one.

---

## Wave 4 — design debates (no code until chosen; run alongside earlier waves)

**T16 and T17 verdicts should be reached early** — they gate notify (T13/S4) and the
observability assertions in S1.

**T15 · package renames** · `discuss` · R13
`lazy` names a *strategy*, misleads (`DeviceBuilderFn` builds eagerly;
`StreamingSupervisor` is always-on) → rename to a domain noun (candidates: `exit`,
`egress`, `tunnelpool`, `dialpool`). The DDD lens notes `asyncjob` is a legitimate
noun and may not need renaming — grouping both as equally guilty is half-wrong. Owner
asked for **3 architects/package + a linguist**; kept as the owner's call, though the
lens flags it as heavy for a single-binary personal app.

**T16 · notify simplification / `go-telegram/bot`** · `discuss` · R8, R15
10-lens panel on whether `internal/infrastructure/notify` is over-built. If swapping
to `github.com/go-telegram/bot`, the R15 criteria bind: the library's errors must be
proven token-free (today all funnel through `redactToken`). Gates T13a/S4.

**T17 · observability → `loginjector`** · `discuss` · R8, R15
Evaluate `github.com/prorochestvo/loginjector` vs the hand-rolled observability pkg.
The swap must retain `scrubHandler`'s host:port redaction (or keep `scrubHandler` on
top) and carry `scrubhandler_test.go`. Gates whether the S1 observability assertions
are worth moving or moot.

**T18 · consolidate `configs/` + `deploy/`** · `discuss` · R14
`deploy/` holds a single nginx edge vhost; `configs/` holds the service tree. Fold the
vhost under `configs/` (e.g. `configs/edge/`) or keep `deploy/` with an explicit
boundary. Small; decide, then move.

---

## Owner decisions (reviewer verdicts vs the review's explicit asks)

The user has final say; defaults below are the reviewers' recommendation.

- **D1 · T12 — merge `ipdeny` into `config`?** Review said yes. DDD lens says no:
  `ipdeny` is a hardcoded, deliberately non-configurable SSRF deny-list; folding it
  into the operator-JSON `config` package inverts its meaning. *Default:* keep
  standalone (optionally `internal/tools/netguard`).
- **D2 · T11 — auth destination.** Review implied `gateway/middleware`. That forces
  an illegal `application → gateway` dependency. *Default:* `internal/tools/bearerauth`,
  `Verifier` port consumer-defined in `application`. (Owner's "not infrastructure"
  instinct honored.)
- **D3 · T08b — "drop `domain/dialer.go`, describe contracts locally".** Correct for
  single-consumer param interfaces; the four egress ports appear in cross-package
  signatures and cannot be scattered (compile break). *Default:* move the four to
  `internal/egress` as one contract each; localize only the true param interfaces.
- **D4 · T09/T10 — `internal/tools` vs a standalone module.** The review wants
  cross-project reuse, but `internal/` forbids it. *Default:* extract to `internal/`
  for tidiness only (YAGNI, one client); promote to a `github.com/prorochestvo/*`
  module if/when a second consumer appears.
- **D5 · T14b — flags in `init()` + `tlsOptions` as a global.** Both break `go test`
  (init runs before `testing.Init`; a global kills the per-test seam). *Default:*
  `parseFlags()` called from `main()`, `tlsOptions` stays threaded; fail-fast is
  already satisfied by `main`'s `os.Exit`.
- **D6 · T15 — rename `asyncjob`, and the 3-architect+linguist ritual.** Lens says
  `asyncjob` is fine and the ritual is heavy for a personal app. *Default:* keep the
  ritual (owner asked); scope the rename to `lazy` unless the panel says otherwise.

---

## Parallelization map (corrected)

```
Wave 1:  T01 ‖ T02 ‖ T03 ‖ T04 ‖ T05 ‖ T06 ‖ T12   (T12 pulled in: no domain coupling, disjoint)
Wave 4:  T15 ‖ T16 ‖ T17 ‖ T18                       (design track, anytime; decide T16/T17 EARLY)

Wave 2:  T08 (keystone; OWNS every domain/egress-contract assertion rewrite, impl+test)
            → T07(+T11)                              (T12 already done → no forwarder.go collision)
            → T09 → T10 → T13                         (2c: serialize on main.go; T10 after/with T07)

Wave 3:  (Wave 2 merged) → [T14 + S2 + S3 = one main.go pass] → S4 → S1 (15 non-domain assertions)
```

Cross-wave gates: T16 ⟶ T13a/S4 · T17 ⟶ S1(observability) · T08b owns S1's 4 domain
assertions. Only structural change vs the original draft: T12 → Wave 1 (kills the sole
false-parallelism edge), and S1/T08b assertion ownership split.

## Verification protocol

- Every wave's plan file(s): **7 reviewers, distinct lenses**, before implementation
  (per the owner's standing instruction for this backlog — origin is that instruction,
  not the source review): correctness/completeness, dependency soundness, requirement
  fidelity, DDD/architecture, Go conventions, security/ops invariants, scope/pragmatism.
- CRITICAL sweeps with real volume (S1, S4): the owner's **5-reviewer single-lens**
  pass. S2/S3 (1–4 sites) fold into the main.go-pass review — nothing to panel.
- `router` split (T07) and `notify` (T16): **10-lens** architecture panels.
- `lazy`/`asyncjob` naming (T15): **3 architects/package + linguist** convergence.
- Gate: `make test` green before any review; a red tree goes to `testdoctor` first.
