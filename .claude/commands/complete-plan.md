---
description: Move an active plan to plans/completed/ with the YYMMDD.NNNN.slug.md naming
argument-hint: <NNN | slug | NNN-slug.md>
---

Move the active plan identified by `$ARGUMENTS` from `plans/` to `plans/completed/` using the `YYMMDD.NNNN.slug.md` naming convention.

Steps:

1. Resolve the source file:
   - If `$ARGUMENTS` matches `NNN` (3 digits), find the unique `plans/NNN-*.md` matching it.
   - If `$ARGUMENTS` matches a slug, find `plans/*-$ARGUMENTS.md`.
   - If `$ARGUMENTS` is a full filename, use it directly.
   - If zero or multiple matches, stop and ask the user to disambiguate.
2. Verify completion gates before moving — run:
   - `make test` (which should run `go fmt`, `go vet`, `go test -race`; if the project lacks a Makefile, run those directly).
   If anything fails, stop and report failures. Do **not** move the file.
3. Compute the destination filename:
   - `YYMMDD` = today's date in UTC (e.g. `260523` for 2026-05-23). Use `date +%y%m%d`.
   - `NNNN` = the next zero-padded daily index for that date. Scan `plans/completed/$YYMMDD.*.md` and pick the highest existing `NNNN` + 1. If none exist for today, use `0001`.
   - `slug` = the slug portion from the source filename (after the `NNN-` prefix).
   - Destination: `plans/completed/$YYMMDD.$NNNN.$slug.md`.
4. Run `git mv <source> <destination>` so history is preserved.
5. Report the move and the new path.

Do not refactor or alter the plan's contents during the move. If the implementation diverged from the plan, ask the user to update the plan file first, then re-run this command.
