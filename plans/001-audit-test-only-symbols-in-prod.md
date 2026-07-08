# Task 001 — Audit test-only symbols in production code

> **How to run (for the operator):** open a fresh session and hand this file to the
> `gocode-architect` agent — e.g. *"Execute the assignment in
> `plans/001-audit-test-only-symbols-in-prod.md`."* The architect surveys the module,
> fills in the Findings table and per-case dispositions, and expands **this same file**
> into the final plan (keep the `001` number and slug — update in place, do not create a
> new plan). It writes **no** Go source; implementation is a later `gocode-engineer` step.

## Mission

Find every symbol that lives in a production (`*.go`, non-`_test.go`) file but whose
**only compile-time consumer is a test**, and for each decide: relocate it into
`_test.go`, replace the bespoke test seam with a production interface, or keep it
(because production reads it). Produce an ordered refactor plan. Do **not** implement.

## Why (trigger / seed case)

`cmd/vpntunnel/main.go` ships test scaffolding in the binary: the functional-option
constructors `withSupervisorBuilder`, `withSchedulerBuilder`, `withShutdownCtx` are
called **only** from `cmd/vpntunnel/main_test.go`, yet they are defined in `main.go`.

Correct counter-example (do not touch): `runOpt`, `runOptions` and its fields
(`supervisorBuilder`, `schedulerBuilder`, `shutdownCtx`) legitimately **stay** in
`main.go` because production `runWithOpts` reads them — e.g. `ro.shutdownCtx` at
`main.go:289`; the builder fields default `nil → DefaultDeviceBuilder`. The `run()`
wrapper (`main.go:189`) exists only to hand tests the `runWithOpts(..., opts...)` seam.

This is the known seed; the audit must find any other cases across the module.

## The criterion — do not blur it

Placement is decided by **compile-time consumption, not intent**:

- Only consumer is a `_test.go` file → test scaffolding → **must** live in `_test.go`.
- Read by production code → **stays** in the production file even if only tests ever set
  it (moving it breaks `go build`). The **setter/constructor moves**; the
  **read-by-prod field/type stays**.

## Scope of the survey

Sweep all of `cmd/` and `internal/`. Candidate patterns:

- functional-option constructors (`func with*` / `func With*`) used only in tests;
- fakes/stubs/mocks (`type fake*|mock*|stub*|nop*`, incl. capitalized) declared in
  production files;
- exported-for-test helpers/hooks (`*ForTest`, `*ForTesting`, a package-level `var` a
  test overwrites);
- godoc admitting test intent (`"test-only"`, `"Intended for tests"`, `"for tests"`,
  `"only in tests"`, `"used by tests"`);
- thin production wrappers existing ONLY to hand tests an injection seam (like `run()`).

Suggested searches, then confirm callers:

```
grep -rn --include='*.go' -E 'test-only|Test-only|Intended for tests|for tests|only in tests|used by tests' .
grep -rn --include='*.go' -E '^func [wW]ith[A-Z]|type (fake|mock|stub|nop|Fake|Mock|Stub)' .
```

For every candidate, grep its callers and confirm **no** non-`_test.go` file references
it before flagging. Exclude anything production also uses, or list it explicitly as
"keep".

## Disposition taxonomy — pick one per case, justify it

- **Relocate** — move the symbol verbatim into the right same-package `_test.go`.
  Cheapest; for pure scaffolding.
- **Interface-ize** — replace the bespoke seam (functional options / injected func
  types / a `runOptions` bag) with a **production interface the code depends on**, so
  tests supply a fake implementation and no test-only type remains in prod. This
  evaluation is **required** (the requester specifically wants dependencies dropped in
  favor of interfaces where it helps). State the exact interface, the production type
  that implements it, and what the test fake looks like. Be honest: an interface wins
  **only** if it removes a bespoke test-only type from prod **without** forking the
  production path or bloating the constructor — otherwise recommend relocation and say
  why.
- **Keep (irreducible)** — read by production; cannot move. Document what stays vs what
  moves (e.g. `runOptions` stays, `withXxx` move).

## Invariants to respect (violate only with a loud, argued trade-off)

- Composition root stays inlined per binary in `cmd/<binary>/` (CLAUDE.md → Code
  Organization Principles). Do **not** move wiring into `internal/bootstrap` just to
  launder a seam.
- Do **not** fork/duplicate the composition-root wiring into the test — the shared smoke
  test must exercise the **real** production path, not a drifting copy.
- Align with CLAUDE.md → Code Organization Principles → *"Test-only code lives in
  `_test.go`, never in a production file"*; cite it.

## Deliverable — expand this file into the plan

Complete this file per the CLAUDE.md plan format: **Overview; Assumptions; Tasks** (each
with Description / Acceptance Criteria / Pitfalls / Complexity); **Execution Order;
Risks; Trade-offs**. Keep the `001` number and slug.

Near the top, fill the **Findings table** — one row per confirmed case:

| symbol | file:line | current mechanism | only test callers? | disposition (relocate / interface-ize / keep) | complexity |
|--------|-----------|-------------------|--------------------|-----------------------------------------------|------------|
| _(to be filled by the architect)_ | | | | | |

If the survey finds **only** the `cmd/vpntunnel/main.go` case, say so explicitly and keep
the plan short — do **not** pad it.

Implementation is out of scope for this task; a later `gocode-engineer` pass executes the
finalized plan.
