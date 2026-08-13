#!/usr/bin/env bash
# Text-level review rules that no AST linter can express.
#
# Comments are invisible to go/analysis-based linters, and "a mock type without
# a compile-time interface assertion" is an absence, which ruleguard cannot
# match. Both are cheap and exact as text scans, so they live here rather than
# being forced into the wrong tool.
#
# Exit code 1 on any violation. Wire into `make lint` after golangci-lint.
set -uo pipefail

root=$(git rev-parse --show-toplevel 2>/dev/null || pwd)
cd "$root" || exit 1

status=0

report() {
	printf '%s\n' "$1" >&2
	status=1
}

# Section-divider comments. They restate structure the file already has.
if hits=$(grep -rnE '^\s*//\s*[-=*#]{3,}' --include='*.go' . 2>/dev/null); then
	report "section-divider comments (structure is not documentation):"
	printf '%s\n' "$hits" >&2
fi

# Plan and task references outside the plans/ tree. The reason belongs in the
# code; the plan number is meaningless to whoever reads the file later.
#
# .claude/agent-memory is exempt: it is written by agents to record where a piece
# of context came from, so a plan reference there is the point rather than a
# leak. The rest of .claude/ is hand-authored and stays under the rule.
if hits=$(grep -rnE '(plans/[0-9]{3}-|PLAN-[0-9]+|task #[0-9]+)' \
	--include='*.go' --include='*.sql' --include='*.md' \
	--exclude-dir=plans --exclude-dir=.git --exclude-dir=agent-memory \
	--exclude-dir=logs --exclude-dir=tmp --exclude-dir=build --exclude-dir=backups \
	. 2>/dev/null); then
	report "plan or task references outside plans/ (write the reason, not the ticket):"
	printf '%s\n' "$hits" >&2
fi

# Mock and stub types in tests must carry a compile-time interface assertion,
# so an interface change breaks the build instead of the test's meaning.
while IFS= read -r file; do
	[ -n "$file" ] || continue
	types=$(grep -oE '^type (mock|stub|fake)[A-Za-z0-9_]* ' "$file" 2>/dev/null | awk '{print $2}')
	[ -n "$types" ] || continue
	while IFS= read -r t; do
		[ -n "$t" ] || continue
		if ! grep -qE "^var _ .* = \(?\*?&?$t\b" "$file"; then
			report "$file: $t has no compile-time interface assertion (var _ Iface = (*$t)(nil))"
		fi
	done <<<"$types"
done < <(find . -name '*_test.go' -not -path './vendor/*' 2>/dev/null)

exit $status
